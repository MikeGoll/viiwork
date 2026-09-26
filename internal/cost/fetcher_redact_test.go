package cost

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const secretKey = "s3cr3t-entsoe-key+/="

// The API key travels as a query parameter, so a transport error's text is the
// whole URL. It must never reach the log.
func TestFetcherNeverLogsTheKeyOnTransportError(t *testing.T) {
	var buf bytes.Buffer
	f := NewSpotFetcher(secretKey, "10YFI-1--------U", "http://127.0.0.1:1/api")
	f.logger = log.New(&buf, "", 0)
	f.Fetch(context.Background())

	out := buf.String()
	if !strings.Contains(out, "ENTSO-E fetch failed") {
		t.Fatalf("expected a fetch failure to be logged, got %q", out)
	}
	assertNoKey(t, out)
	if !strings.Contains(out, "securityToken="+redacted) {
		t.Errorf("the log should show where the key was redacted: %q", out)
	}
}

// A server that echoes the request back must not smuggle the key into the log
// either.
func TestFetcherRedactsAnEchoedKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("bad token " + r.URL.Query().Get("securityToken") + " in " + r.URL.RawQuery))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	f := NewSpotFetcher(secretKey, "10YFI-1--------U", srv.URL+"/api")
	f.logger = log.New(&buf, "", 0)
	f.Fetch(context.Background())

	out := buf.String()
	if !strings.Contains(out, "returned 401") {
		t.Fatalf("expected the 401 to be logged, got %q", out)
	}
	assertNoKey(t, out)
}

func assertNoKey(t *testing.T, out string) {
	t.Helper()
	for _, leak := range []string{secretKey, "s3cr3t-entsoe-key%2B%2F%3D", "s3cr3t"} {
		if strings.Contains(out, leak) {
			t.Errorf("log leaks the API key (%q): %q", leak, out)
		}
	}
}
