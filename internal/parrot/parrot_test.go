package parrot

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// strict mirrors viiwork-parrot's own request rules, so a test fails the way
// the real node would if this client ever stopped satisfying them.
func strict(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			host = h
		}
		if a, err := netip.ParseAddr(host); host != "localhost" && (err != nil || !a.IsLoopback()) {
			http.Error(w, "Host must be a loopback address or localhost", http.StatusMisdirectedRequest)
			return
		}
		if r.Method != http.MethodGet {
			if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
				http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		next(w, r)
	}
}

func serve(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(strict(h))
	t.Cleanup(srv.Close)
	return New(strings.TrimPrefix(srv.URL, "http://"))
}

func reply(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

func TestEnsureClassifiesEveryCode(t *testing.T) {
	cases := []struct {
		name string
		code int
		body string
		want Result
	}{
		{"seeding", 200, `{"path":"/srv/parrot/g.gguf","status":{"state":"seeding","percent":100}}`,
			Result{Kind: Ready, Path: "/srv/parrot/g.gguf", State: "seeding", Percent: 100, Code: 200}},
		{"downloading", 202, `{"status":{"state":"downloading","percent":42.5,"down_rate":85000000}}`,
			Result{Kind: Pending, State: "downloading", Percent: 42.5, DownRate: 85000000, Code: 202}},
		{"unknown id", 404, `{"error":"unknown model x"}`, Result{Kind: Refused, Code: 404, Message: "unknown model x"}},
		{"not on this host", 409, `{"error":"never downloads","status":{"state":"absent"}}`,
			Result{Kind: Refused, Code: 409, State: "absent", Message: "never downloads"}},
		{"failed", 500, `{"error":"sha256 mismatch","status":{"state":"failed"}}`,
			Result{Kind: Refused, Code: 500, State: "failed", Message: "sha256 mismatch"}},
		{"no catalog", 503, `{"error":"no catalog yet"}`, Result{Kind: Unavailable, Code: 503, Message: "no catalog yet"}},
		{"no space", 507, `{"error":"insufficient disk","status":{"state":"paused"}}`,
			Result{Kind: Refused, Code: 507, State: "paused", Message: "insufficient disk"}},
		{"200 without a path", 200, `{"status":{"state":"seeding"}}`,
			Result{Kind: Refused, Code: 200, State: "seeding", Message: "viiwork-parrot answered 200 without a path"}},
		{"unexpected code, text body", 418, "teapot\n", Result{Kind: Refused, Code: 418, Message: "teapot"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotID string
			c := serve(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/ensure" {
					t.Errorf("%s %s, want POST /ensure", r.Method, r.URL.Path)
				}
				var body struct{ ID string }
				_ = json.NewDecoder(r.Body).Decode(&body)
				gotID = body.ID
				reply(tc.code, tc.body)(w, r)
			})
			got := c.Ensure(context.Background(), "granite")
			if got != tc.want {
				t.Errorf("got %+v\nwant %+v", got, tc.want)
			}
			if gotID != "granite" {
				t.Errorf("sent id %q", gotID)
			}
		})
	}
}

// strict() answers 415/421 when the headers are wrong. Reaching the handler at
// all proves the client satisfied both rules; this pins it explicitly.
func TestEnsureSatisfiesParrotRequestRules(t *testing.T) {
	c := serve(t, reply(200, `{"path":"/p","status":{"state":"seeding"}}`))
	if got := c.Ensure(context.Background(), "g"); got.Kind != Ready {
		t.Fatalf("got %+v: the client broke viiwork-parrot's Content-Type or Host rule", got)
	}
}

func TestEnsureConnectionRefusedIsUnavailable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	got := New(addr).Ensure(context.Background(), "g")
	if got.Kind != Unavailable || got.Code != 0 || got.Message == "" {
		t.Errorf("got %+v, want Unavailable with a message", got)
	}
}

func TestEnsureHonoursContext(t *testing.T) {
	done := make(chan struct{})
	defer close(done) // runs before t.Cleanup's srv.Close, so the handler is never stuck
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-done:
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	got := c.Ensure(ctx, "g")
	if time.Since(start) > 2*time.Second {
		t.Fatal("Ensure did not return promptly after its context ended")
	}
	if got.Kind != Unavailable {
		t.Errorf("got %+v, want Unavailable", got)
	}
}

// A 503 with no error text must still produce a non-empty Message: fetch.go
// logs Unavailable only when the message text is non-empty against its
// sentinel, and an empty message here used to mean total silence forever.
func TestEnsure503EmptyBodyHasAMessage(t *testing.T) {
	c := serve(t, reply(503, `{}`))
	got := c.Ensure(context.Background(), "g")
	if got.Kind != Unavailable || got.Message == "" {
		t.Errorf("got %+v, want Unavailable with a non-empty Message", got)
	}
}

// A status shape viiwork does not expect (here a bare string instead of an
// object) must not stop path from being read: only a genuine protocol
// violation (200, no path at all) is Refused.
func TestEnsure200BareStringStatusIsStillReady(t *testing.T) {
	c := serve(t, reply(200, `{"path":"/srv/parrot/g.gguf","status":"seeding"}`))
	got := c.Ensure(context.Background(), "g")
	if got.Kind != Ready || got.Path != "/srv/parrot/g.gguf" {
		t.Errorf("got %+v, want Ready with the path", got)
	}
}

// A 200 whose body is not JSON at all is a shape mismatch, not a protocol
// violation: Unavailable costs nothing to retry, while Refused would kill the
// backends on an answer that may have actually meant the weights are ready.
func TestEnsure200NonJSONBodyIsUnavailable(t *testing.T) {
	c := serve(t, reply(200, `not json`))
	got := c.Ensure(context.Background(), "g")
	if got.Kind != Unavailable {
		t.Errorf("got %+v, want Unavailable", got)
	}
}

func TestStatus(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/status" {
			t.Errorf("%s %s, want GET /status", r.Method, r.URL.Path)
		}
		reply(200, `[{"id":"g","state":"seeding","percent":100,"path":"/p/g.gguf","peers":3,"files":[]}]`)(w, r)
	})
	got, err := c.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := ModelStatus{ID: "g", State: "seeding", Percent: 100, Path: "/p/g.gguf"}
	if len(got) != 1 || got[0] != want {
		t.Errorf("got %+v, want [%+v]", got, want)
	}
}

func TestStatusErrors(t *testing.T) {
	c := serve(t, reply(500, "boom"))
	if _, err := c.Status(context.Background()); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want one naming HTTP 500", err)
	}
	c = serve(t, reply(200, "not json"))
	if _, err := c.Status(context.Background()); err == nil {
		t.Error("a malformed body must be an error")
	}
}
