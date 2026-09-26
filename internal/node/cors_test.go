package node

import (
	"context"
	"errors"
	stdlog "log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/janit/viiwork/v2/internal/config"
)

func testCORS() *CORS {
	return &CORS{
		Origins:    []string{"*.ts.net", "*.example.com", "localhost"},
		TailnetIPs: true,
	}
}

func TestCORSAllows(t *testing.T) {
	c := testCORS()
	cases := []struct {
		origin string
		want   bool
		why    string
	}{
		{"https://node0.tailnet-abc.ts.net", true, "MagicDNS name"},
		{"http://node0.tailnet-abc.ts.net:8080", true, "port is not part of the host match"},
		{"https://admin.example.com", true, "allowed subdomain"},
		{"http://localhost:5180", true, "exact pattern, dev server"},
		{"http://100.100.42.7:8080", true, "tailnet IPv4 literal"},
		{"http://[fd7a:115c:a1e0::1]", true, "tailnet IPv6 literal"},

		{"https://ts.net", false, "a *. rule must not match the bare apex"},
		{"https://example.com.evil.test", false, "suffix must be a real label boundary"},
		{"https://notexample.com", false, "must not match a longer label ending the same way"},
		{"http://192.168.1.10", false, "LAN address is not tailnet"},
		{"http://100.63.255.255", false, "just below the CGNAT block"},
		{"http://100.128.0.0", false, "just above the CGNAT block"},
		{"null", false, "sandboxed iframe / file origin"},
		{"", false, "no Origin header"},
		{"chrome-extension://abcdef", false, "non-http scheme"},
	}
	for _, tc := range cases {
		if got := c.Allows(tc.origin); got != tc.want {
			t.Errorf("Allows(%q) = %v, want %v — %s", tc.origin, got, tc.want, tc.why)
		}
	}
}

// A nil CORS is the "not configured" state and must never emit a header.
func TestCORSNilAllowsNothing(t *testing.T) {
	var c *CORS
	if c.Allows("https://node0.tailnet-abc.ts.net") {
		t.Error("nil CORS allowed an origin")
	}
}

// corsServer is NewServer with CORS configured and an inference fake that
// answers GET /v1/models and 404s anything else there, as the proxy does.
func corsServer(t *testing.T, cors *CORS) http.Handler {
	t.Helper()
	d := fakeServerDeps(t)
	d.CORS = cors
	return NewServer(d)
}

func TestCORSHeadersOnGET(t *testing.T) {
	h := corsServer(t, testCORS())
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Origin", "https://admin.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://admin.example.com" {
		t.Errorf("allow-origin = %q, want the echoed origin", got)
	}
	if got := w.Header().Get("Vary"); got == "" {
		t.Error("Vary: Origin missing — a shared cache could cross origins over")
	}
	if got := w.Header().Get("Access-Control-Expose-Headers"); got == "" {
		t.Error("expose-headers missing; a browser client cannot read X-GPU-Backend")
	}
}

// Vary must be set even when the origin is refused, or a cache can hand the
// allowed origin's response to a disallowed one.
func TestCORSDisallowedOriginGetsNoHeaderButStillVaries(t *testing.T) {
	h := corsServer(t, testCORS())
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Origin", "https://evil.test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("allow-origin = %q, want empty for a disallowed origin", got)
	}
	if w.Header().Get("Vary") == "" {
		t.Error("Vary: Origin missing on the refusal path")
	}
	if w.Code != 200 {
		t.Errorf("status = %d; the request itself is not blocked, only the browser's read of it", w.Code)
	}
}

// Before CORS existed the router matched only GET and POST, so every preflight
// fell through to 404 and no cross-origin POST could work at all.
func TestCORSPreflightAnswered(t *testing.T) {
	h := corsServer(t, testCORS())
	req := httptest.NewRequest("OPTIONS", "/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://admin.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "content-type, x-viiwork-task")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("allow-methods missing")
	}
	if got := w.Header().Get("Access-Control-Allow-Headers"); got != "content-type, x-viiwork-task" {
		t.Errorf("allow-headers = %q, want the requested set echoed", got)
	}
}

func TestCORSPreflightRefused(t *testing.T) {
	h := corsServer(t, testCORS())
	req := httptest.NewRequest("OPTIONS", "/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://evil.test")
	req.Header.Set("Access-Control-Request-Method", "POST")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 so the cause is visible in devtools", w.Code)
	}
}

// A plain OPTIONS with no Access-Control-Request-Method is not a preflight and
// must keep its previous behaviour rather than being swallowed as one.
func TestCORSNonPreflightOptionsStill404s(t *testing.T) {
	h := corsServer(t, testCORS())
	req := httptest.NewRequest("OPTIONS", "/v1/models", nil)
	req.Header.Set("Origin", "https://admin.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// Without CORS configured the API must behave exactly as it did before.
func TestNoCORSConfiguredSendsNoHeaders(t *testing.T) {
	h := corsServer(t, nil)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Origin", "https://admin.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("allow-origin = %q, want none when CORS is unconfigured", got)
	}
}

// EventSource sends no preflight, so an SSE endpoint that misses the header on
// its own GET response is unusable cross-origin no matter what OPTIONS does.
func TestCORSHeaderOnSSEEndpoint(t *testing.T) {
	h := corsServer(t, testCORS())
	req := httptest.NewRequest("GET", "/v1/metrics/stream", nil)
	req.Header.Set("Origin", "https://node0.tailnet-abc.ts.net")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://node0.tailnet-abc.ts.net" {
		t.Errorf("allow-origin = %q on the SSE path", got)
	}
}

// corsNode is just enough Node for buildCORS: a MagicDNS answer and a log.
func corsNode(suffix string, err error, log *strings.Builder) *Node {
	return &Node{
		o:      Options{MagicDNS: func(context.Context, string) (string, error) { return suffix, err }},
		logger: stdlog.New(log, "", 0),
	}
}

func corsConfig(t *testing.T, doc string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Unset allow_origins used to default to "*.ts.net", which is every Tailscale
// customer's domain — Funnel pages included. The node now trusts only its own
// tailnet's MagicDNS domain, as tailscaled reports it.
func TestBuildCORSDerivesOwnTailnet(t *testing.T) {
	var log strings.Builder
	c := corsNode("tail1234.ts.net", nil, &log).buildCORS(corsConfig(t, ""))
	if c == nil {
		t.Fatal("no CORS for an unset allowlist")
	}
	for origin, want := range map[string]bool{
		"https://gb1.tail1234.ts.net":     true,
		"http://localhost:5173":           true,
		"http://127.0.0.1:8086":           true,
		"http://100.100.42.7":             true, // tailnet IP literal
		"https://someone-else.ts.net":     false,
		"https://funnel.tail9999.ts.net":  false,
		"https://tail1234.ts.net.evil.io": false,
	} {
		if c.Allows(origin) != want {
			t.Errorf("Allows(%q) = %v, want %v", origin, !want, want)
		}
	}
	if !strings.Contains(log.String(), "*.tail1234.ts.net") {
		t.Errorf("the derived list is not logged: %q", log.String())
	}
}

func TestBuildCORSFallsBackWithoutTailscaled(t *testing.T) {
	var log strings.Builder
	c := corsNode("", errors.New("no socket"), &log).buildCORS(corsConfig(t, ""))
	if c == nil {
		t.Fatal("no CORS at all; the local origins should remain")
	}
	if c.Allows("https://gb1.tail1234.ts.net") || c.Allows("https://anything.ts.net") {
		t.Error("a MagicDNS name is allowed with no tailnet domain known")
	}
	if !c.Allows("http://localhost:5173") || !c.Allows("http://100.100.42.7") {
		t.Error("the fallback lost localhost or tailnet IP literals")
	}
	if !strings.Contains(log.String(), "unknown") || !strings.Contains(log.String(), "api.cors.allow_origins") {
		t.Errorf("the fallback is not explained in the log: %q", log.String())
	}
}

// An operator's list is theirs: never asked about, never extended.
func TestBuildCORSKeepsExplicitList(t *testing.T) {
	var log strings.Builder
	n := corsNode("tail1234.ts.net", nil, &log)
	n.o.MagicDNS = func(context.Context, string) (string, error) {
		t.Error("tailscaled consulted for an explicit list")
		return "", nil
	}
	c := n.buildCORS(corsConfig(t, "api:\n  cors:\n    allow_origins: [\"*.example.com\"]\n    allow_tailnet_ips: false\n"))
	if c == nil || len(c.Origins) != 1 || c.Origins[0] != "*.example.com" || c.TailnetIPs {
		t.Fatalf("explicit list altered: %+v", c)
	}
	if c := n.buildCORS(corsConfig(t, "api:\n  cors:\n    allow_origins: []\n")); c != nil {
		t.Errorf("allow_origins: [] must mean no CORS, got %+v", c)
	}
}
