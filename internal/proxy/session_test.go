package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

func TestSessionKeyHeaders(t *testing.T) {
	req := func(h map[string]string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		for k, v := range h {
			r.Header.Set(k, v)
		}
		return r
	}
	if k := sessionKey(req(nil)); k != 0 {
		t.Fatalf("no header: key %d, want 0", k)
	}
	if k := sessionKey(req(map[string]string{"X-Session-Affinity": ""})); k != 0 {
		t.Fatalf("empty header: key %d, want 0", k)
	}
	aff := sessionKey(req(map[string]string{"x-session-affinity": "abc"}))
	id := sessionKey(req(map[string]string{"X-Session-Id": "abc"}))
	if aff == 0 || aff != id {
		t.Fatalf("affinity %d, id %d: the same value hashes the same from either header", aff, id)
	}
	both := sessionKey(req(map[string]string{"X-Session-Affinity": "abc", "X-Session-Id": "other"}))
	if both != aff {
		t.Fatal("X-Session-Affinity wins over X-Session-Id")
	}
	fallback := sessionKey(req(map[string]string{"X-Session-Affinity": "", "X-Session-Id": "other"}))
	if fallback == 0 || fallback == aff {
		t.Fatal("an empty X-Session-Affinity falls back to X-Session-Id")
	}
	if k := sessionKey(req(map[string]string{"X-Session-Id": "abd"})); k == aff {
		t.Fatal("different sessions, different keys")
	}
}

func BenchmarkSessionKey(b *testing.B) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("X-Session-Id", "ses_4f1c2a9e0b7d4c3e8a1f")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = sessionKey(r)
	}
}

// The key reaches the router: with three equal peers and draws that would
// spread requests across them, a session's requests all land on one peer.
func TestDispatchPassesTheSessionKey(t *testing.T) {
	run := func(header string) map[string]int {
		f := newHandlerFx(t)
		f.performance = true
		draws := []float64{0.1, 0.5, 0.9}
		i := 0
		f.rand = func() float64 { v := draws[i%len(draws)]; i++; return v }
		for _, n := range []string{"A", "B", "C"} {
			m := peerModel("m", 4, 0)
			m.TTFTOverheadMs, m.PrefillMsPer1k = 100, 1000
			f.reports.add(n, newRecEngine(t, nil).addr(), time.Now(), m)
		}
		f.build()
		seen := map[string]int{}
		for j := 0; j < 6; j++ {
			rec := f.do(http.MethodPost, "/v1/chat/completions", chatReq, func(r *http.Request) {
				if header != "" {
					r.Header.Set("X-Session-Id", header)
				}
			})
			if rec.Code != 200 {
				t.Fatalf("code %d: %s", rec.Code, rec.Body.String())
			}
			seen[rec.Header().Get(meshapi.HeaderNode)]++
		}
		return seen
	}
	if seen := run(""); len(seen) < 2 {
		t.Fatalf("without a session the draws spread the requests, got %v", seen)
	}
	if seen := run("ses_1"); len(seen) != 1 {
		t.Fatalf("one session, one host: got %v", seen)
	}
}
