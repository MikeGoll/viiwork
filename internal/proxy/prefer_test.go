package proxy

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

func TestParsePrefer(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
		ok   bool
	}{
		{"", nil, true},
		{"gb1", []string{"gb1"}, true},
		{"yeti,gb1", []string{"yeti", "gb1"}, true},
		{" yeti , gb1 ,", []string{"yeti", "gb1"}, true},
		{",,", nil, true},
		{"gb1,mesh", []string{"gb1"}, true},
		{"gb1,a b", nil, false},
		{"gb1,http://x/", nil, false},
		{"a,b,c,d,e,f,g,h", []string{"a", "b", "c", "d", "e", "f", "g", "h"}, true},
		{"a,b,c,d,e,f,g,h,i", nil, false},
		{strings.Repeat("a", maxHostLen+1), nil, false},
	} {
		got, ok := parsePrefer(c.in)
		if ok != c.ok || !reflect.DeepEqual(got, c.want) {
			t.Errorf("parsePrefer(%q) = %v, %v; want %v, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// The list reaches the router from the parameter or the header, and the
// header goes no further than this node.
func TestDispatchPassesThePreference(t *testing.T) {
	type fx struct {
		*handlerFx
		local, a, b *recEngine
	}
	setup := func(t *testing.T) fx {
		f := fx{handlerFx: newHandlerFx(t), local: newRecEngine(t, nil), a: newRecEngine(t, nil), b: newRecEngine(t, nil)}
		f.handlerFx.local.add("m", f.local.addr())
		f.reports.add("A", f.a.addr(), time.Now(), peerModel("m", 4, 0))
		f.reports.add("B", f.b.addr(), time.Now(), peerModel("m", 4, 0))
		f.build()
		return f
	}
	header := func(v string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set(meshapi.HeaderPrefer, v) }
	}
	node := func(t *testing.T, f fx, target string, edit ...func(*http.Request)) string {
		t.Helper()
		rec := f.do(http.MethodPost, target, chatReq, edit...)
		if rec.Code != 200 {
			t.Fatalf("%s: code %d: %s", target, rec.Code, rec.Body.String())
		}
		return rec.Header().Get(meshapi.HeaderNode)
	}

	t.Run("no list is local first", func(t *testing.T) {
		f := setup(t)
		if got := node(t, f, "/v1/chat/completions"); got != "self" {
			t.Errorf("node = %q", got)
		}
	})
	t.Run("parameter", func(t *testing.T) {
		f := setup(t)
		if got := node(t, f, "/v1/chat/completions?prefer=B,A"); got != "B" {
			t.Errorf("node = %q, want B", got)
		}
	})
	t.Run("header, and it stops here", func(t *testing.T) {
		f := setup(t)
		if got := node(t, f, "/v1/chat/completions", header("nosuch, A")); got != "A" {
			t.Errorf("node = %q, want A", got)
		}
		if _, h := f.a.last(); h.Get(meshapi.HeaderPrefer) != "" {
			t.Errorf("the member received %s: %q", meshapi.HeaderPrefer, h.Get(meshapi.HeaderPrefer))
		}
		if got := node(t, f, "/v1/chat/completions", header("self")); got != "self" {
			t.Errorf("node = %q, want self", got)
		}
		if _, h := f.local.last(); h.Get(meshapi.HeaderPrefer) != "" {
			t.Errorf("the engine received %s: %q", meshapi.HeaderPrefer, h.Get(meshapi.HeaderPrefer))
		}
	})
	t.Run("the parameter wins over the header", func(t *testing.T) {
		f := setup(t)
		if got := node(t, f, "/v1/chat/completions?prefer=B", header("A")); got != "B" {
			t.Errorf("node = %q, want B", got)
		}
	})
	t.Run("an unknown name falls through", func(t *testing.T) {
		f := setup(t)
		if got := node(t, f, "/v1/chat/completions?prefer=nosuch"); got != "self" {
			t.Errorf("node = %q, want self", got)
		}
	})
	t.Run("the pin wins over the list", func(t *testing.T) {
		f := setup(t)
		if got := node(t, f, "/v1/chat/completions?host=A&prefer=B"); got != "A" {
			t.Errorf("node = %q, want A", got)
		}
	})
	t.Run("a forward ignores it", func(t *testing.T) {
		f := setup(t)
		f.members = fakeMembers{memberAt("A", "127.0.0.1", meshapi.MemberAlive, false)}
		f.build()
		for _, target := range []string{"/v1/chat/completions?prefer=B", "/v1/chat/completions?prefer=b%20c"} {
			rec := f.do(http.MethodPost, target, chatReq, fromMember("A"), header("B"))
			if rec.Code != 200 || rec.Header().Get(meshapi.HeaderNode) != "self" {
				t.Errorf("%s: code %d on %q, want this node", target, rec.Code, rec.Header().Get(meshapi.HeaderNode))
			}
		}
		if f.b.hits.Load() != 0 {
			t.Error("a forward was forwarded again")
		}
	})
	t.Run("malformed is 400", func(t *testing.T) {
		f := setup(t)
		for _, do := range []func() (int, string){
			func() (int, string) {
				rec := f.do(http.MethodPost, "/v1/chat/completions?prefer=A,b%20c", chatReq)
				return rec.Code, errorOf(t, rec).Message
			},
			func() (int, string) {
				rec := f.do(http.MethodPost, "/v1/chat/completions", chatReq, header("a,b,c,d,e,f,g,h,i"))
				return rec.Code, errorOf(t, rec).Message
			},
			func() (int, string) {
				rec := f.do(http.MethodPost, "/v1/chat/completions?host=A&prefer=b%20c", chatReq)
				return rec.Code, errorOf(t, rec).Message
			},
		} {
			if code, msg := do(); code != 400 || msg != "invalid prefer parameter" {
				t.Errorf("code %d, message %q", code, msg)
			}
		}
		if f.local.hits.Load()+f.a.hits.Load()+f.b.hits.Load() != 0 {
			t.Error("a refused request reached an engine")
		}
	})
}
