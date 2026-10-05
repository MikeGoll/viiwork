package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// echoOriginEngine behaves like llama-server: it reflects whatever Origin it
// is sent into Access-Control-Allow-Origin.
func echoOriginEngine(w http.ResponseWriter, r *http.Request) {
	if o := r.Header.Get("Origin"); o != "" {
		w.Header().Set("Access-Control-Allow-Origin", o)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
	}
	w.Header().Add("Vary", "Origin, Accept-Encoding")
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(engineJSON))
}

// corsStandIn plays internal/node's CORS layer, which runs before the proxy:
// Vary: Origin always, and the allow header only for an allowed origin.
func corsStandIn(allowed string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" {
			w.Header().Add("Vary", "Origin")
			if o == allowed {
				w.Header().Set("Access-Control-Allow-Origin", o)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// The node's CORS layer is the only authority on CORS. An engine that echoes
// Origin must neither see Origin nor get its allow header to the client:
// otherwise a refused origin reads the output, and an allowed one gets two
// allow headers, which browsers reject.
func TestInferenceResponseCarriesOnlyTheNodesCORS(t *testing.T) {
	const allowed, refusedOrigin = "https://app.example.ts.net", "https://evil.example"
	for _, path := range []string{"local", "peer"} {
		t.Run(path, func(t *testing.T) {
			f := newHandlerFx(t)
			eng := newRecEngine(t, echoOriginEngine)
			if path == "local" {
				f.local.add("m", eng.addr())
			} else {
				f.reports.add("P", eng.addr(), time.Now(), peerModel("m", 1, 0))
			}
			f.build()
			front := corsStandIn(allowed, f.h)

			for _, origin := range []string{refusedOrigin, allowed} {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatReq))
				req.Header.Set("Origin", origin)
				req.Header.Set("Access-Control-Request-Method", "POST")
				rec := httptest.NewRecorder()
				front.ServeHTTP(rec, req)
				if rec.Code != 200 {
					t.Fatalf("%s: code=%d body=%q", origin, rec.Code, rec.Body.String())
				}
				acao := rec.Header().Values("Access-Control-Allow-Origin")
				switch origin {
				case refusedOrigin:
					if len(acao) != 0 {
						t.Errorf("refused origin got Access-Control-Allow-Origin %q", acao)
					}
				case allowed:
					if len(acao) != 1 || acao[0] != allowed {
						t.Errorf("allowed origin got Access-Control-Allow-Origin %q, want exactly one", acao)
					}
				}
				if v := rec.Header().Get("Access-Control-Allow-Credentials"); v != "" {
					t.Errorf("%s: engine's Access-Control-Allow-Credentials reached the client", origin)
				}
				if vary := rec.Header().Values("Vary"); strings.Count(strings.Join(vary, ","), "Origin") != 1 || !strings.Contains(strings.Join(vary, ","), "Accept-Encoding") {
					t.Errorf("%s: Vary = %q, want the node's Origin once and the engine's Accept-Encoding", origin, vary)
				}
				_, h := eng.last()
				if h.Get("Origin") != "" || h.Get("Access-Control-Request-Method") != "" {
					t.Errorf("%s: the engine was sent CORS headers: %v", origin, h)
				}
			}
		})
	}
}

func TestVaryWithoutOrigin(t *testing.T) {
	cases := []struct{ in, want []string }{
		{[]string{"Accept-Encoding"}, []string{"Accept-Encoding"}},
		{[]string{"Origin"}, nil},
		{[]string{"origin, Accept-Encoding", "Origin"}, []string{"Accept-Encoding"}},
		{[]string{"Accept-Encoding, Origin, X-Other"}, []string{"Accept-Encoding, X-Other"}},
	}
	for _, c := range cases {
		got := varyWithoutOrigin(c.in)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("varyWithoutOrigin(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
