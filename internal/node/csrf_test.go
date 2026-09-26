package node

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/janit/viiwork/v2/meshapi"
)

// csrfRequest is a browser-shaped write: an Origin and a Content-Type.
func csrfRequest(method, target, origin, contentType, body string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Host = "gb1.tailnet-abc.ts.net:8086"
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req
}

// The finding this guards: a text/plain POST is a "simple request" — no
// preflight — so before this any page a tailnet member visited could switch a
// host off through that member's browser. CORS only ever governed the read.
func TestCSRFRefusedOriginCannotWrite(t *testing.T) {
	for _, cors := range []*CORS{testCORS(), nil} {
		d, f := fakeServer(t)
		d.CORS = cors
		h := NewServer(d)
		cases := []struct {
			method, path, ct string
		}{
			{http.MethodPost, meshapi.PathMeshPower, "text/plain"},
			{http.MethodPost, meshapi.PathMeshPower, "application/json"},
			{http.MethodPost, meshapi.PathPower, "application/json"},
			{http.MethodPut, "/v1/aliases/x", "application/json"},
			{http.MethodDelete, "/v1/aliases/x", ""},
			{http.MethodPost, "/v1/aliases/x/revert", ""},
			// A refused origin must not spend GPU time either.
			{http.MethodPost, meshapi.PathChatCompletions, "text/plain"},
			{http.MethodPost, meshapi.PathEmbeddings, "application/json"},
		}
		for _, tc := range cases {
			for _, origin := range []string{"https://evil.test", "null"} {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, csrfRequest(tc.method, tc.path, origin, tc.ct, `{"host":"gb2","action":"off"}`))
				if rec.Code != http.StatusForbidden {
					t.Errorf("cors=%v %s %s from %s (%s): %d, want 403", cors != nil, tc.method, tc.path, origin, tc.ct, rec.Code)
				}
			}
		}
		if f.inference.Load() != 0 || f.aliases.Load() != 0 {
			t.Errorf("cors=%v: a refused write reached a handler (inference %d, aliases %d)",
				cors != nil, f.inference.Load(), f.aliases.Load())
		}
	}
}

// Reads stay as they were: a refused origin's GET is served, and CORS keeps
// the browser from reading it.
func TestCSRFRefusedOriginCanStillGet(t *testing.T) {
	d, f := fakeServer(t)
	d.CORS = testCORS()
	rec := httptest.NewRecorder()
	NewServer(d).ServeHTTP(rec, csrfRequest(http.MethodGet, meshapi.PathModels, "https://evil.test", "", ""))
	if rec.Code != 200 || f.inference.Load() != 1 {
		t.Errorf("GET from a refused origin: %d, inference %d", rec.Code, f.inference.Load())
	}
}

// The ones that must keep working: non-browser clients (no Origin), the
// node's own pages (same origin, whatever host name they were opened by), and
// allowlisted origins.
func TestCSRFAllowedWrites(t *testing.T) {
	cases := []struct {
		why, origin string
		cors        *CORS
	}{
		{"no Origin: curl, SDKs, member forwards", "", testCORS()},
		{"no Origin, no CORS configured", "", nil},
		{"same origin, host not on the allowlist", "http://gb1.tailnet-abc.ts.net:8086", &CORS{}},
		{"same origin, no CORS configured", "http://gb1.tailnet-abc.ts.net:8086", nil},
		{"allowlisted origin", "https://admin.example.com", testCORS()},
		{"tailnet IP literal", "http://100.100.42.7:5173", testCORS()},
	}
	for _, tc := range cases {
		d, f := fakeServer(t)
		d.CORS = tc.cors
		h := NewServer(d)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, csrfRequest(http.MethodPost, meshapi.PathChatCompletions, tc.origin, "application/json", "{}"))
		if rec.Code != 200 || f.inference.Load() != 1 {
			t.Errorf("%s: inference POST %d", tc.why, rec.Code)
		}
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, csrfRequest(http.MethodPut, "/v1/aliases/x", tc.origin, "application/json; charset=utf-8", "{}"))
		if rec.Code != 200 || f.aliases.Load() != 1 {
			t.Errorf("%s: alias PUT %d %q", tc.why, rec.Code, rec.Body.String())
		}
	}
}

// A same-origin check must compare the port too: a page served by another
// service on the same host is a different origin.
func TestCSRFSameHostOtherPortIsCrossOrigin(t *testing.T) {
	d, _ := fakeServer(t)
	d.CORS = &CORS{}
	rec := httptest.NewRecorder()
	NewServer(d).ServeHTTP(rec, csrfRequest(http.MethodPost, meshapi.PathChatCompletions,
		"http://gb1.tailnet-abc.ts.net:3000", "application/json", "{}"))
	if rec.Code != http.StatusForbidden {
		t.Errorf("other port on the same host: %d, want 403", rec.Code)
	}
}

// Control writes are JSON or nothing: any other type is a request a browser
// sends without a preflight. Inference keeps accepting whatever it did, since
// `curl -d` sends form encoding and those clients send no Origin anyway.
func TestCSRFControlWritesRequireJSON(t *testing.T) {
	d, f := fakeServer(t)
	d.CORS = testCORS()
	h := NewServer(d)
	for _, tc := range []struct{ method, path, ct string }{
		{http.MethodPost, meshapi.PathMeshPower, "text/plain"},
		{http.MethodPost, meshapi.PathPower, "application/x-www-form-urlencoded"},
		{http.MethodPost, meshapi.PathMeshPower, ""},
		{http.MethodPut, "/v1/aliases/x", "multipart/form-data; boundary=x"},
		{http.MethodDelete, "/v1/aliases/x", ""},
		{http.MethodPost, "/v1/aliases/x/revert", ""},
		{http.MethodPost, "/v1/aliases/x/revert", "application/jsonp"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, csrfRequest(tc.method, tc.path, "", tc.ct, `{"host":"gb2","action":"off"}`))
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("%s %s (%q): %d, want 415", tc.method, tc.path, tc.ct, rec.Code)
		}
	}
	if f.aliases.Load() != 0 {
		t.Errorf("a non-JSON control write reached the alias handler")
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, csrfRequest(http.MethodPost, meshapi.PathChatCompletions, "", "application/x-www-form-urlencoded", "{}"))
	if rec.Code != 200 {
		t.Errorf("inference with curl's default type: %d, want it served", rec.Code)
	}
	// Reads of the control families are untouched.
	if rec := get(h, meshapi.PathAliases); rec.Code != 200 {
		t.Errorf("GET /v1/aliases: %d", rec.Code)
	}
}

// An older alias CLI sends delete and revert with no body and no Content-Type.
// Nodes upgrade one at a time, so those must keep working; a browser cannot
// exploit it, since a cross-origin write always carries Origin.
func TestCSRFBodilessWriteFromOlderCLI(t *testing.T) {
	d, f := fakeServer(t)
	d.CORS = testCORS()
	h := NewServer(d)
	for _, tc := range []struct{ method, path string }{
		{http.MethodDelete, "/v1/aliases/x"},
		{http.MethodPost, "/v1/aliases/x/revert"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, csrfRequest(tc.method, tc.path, "", "", ""))
		if rec.Code != 200 {
			t.Errorf("%s %s without body or type: %d, want it served", tc.method, tc.path, rec.Code)
		}
	}
	if f.aliases.Load() != 2 {
		t.Errorf("alias handler reached %d times, want 2", f.aliases.Load())
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, csrfRequest(http.MethodPost, "/v1/aliases/x/revert", "https://evil.test", "", ""))
	if rec.Code != http.StatusForbidden {
		t.Errorf("bodiless write from a refused origin: %d, want 403", rec.Code)
	}
}

// The refusal must still carry Vary: Origin, or a cache could hand one
// origin's 403 to another.
func TestCSRFRefusalVaries(t *testing.T) {
	d, _ := fakeServer(t)
	d.CORS = testCORS()
	rec := httptest.NewRecorder()
	NewServer(d).ServeHTTP(rec, csrfRequest(http.MethodPost, meshapi.PathMeshPower, "https://evil.test", "application/json", "{}"))
	if rec.Code != http.StatusForbidden || rec.Header().Get("Vary") == "" {
		t.Errorf("%d, Vary %q", rec.Code, rec.Header().Get("Vary"))
	}
}
