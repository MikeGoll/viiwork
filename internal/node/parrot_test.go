package node

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/mesh/meshtest"
)

// A sourced model on a real node: a fake viiwork-parrot answers 202 twice and
// then 200 with a temp GGUF, and the real llamacpp engine starts the fake
// llama-server — which refuses to run unless --model is exactly that path.
func TestNodeRunsASourcedModel(t *testing.T) {
	weights := filepath.Join(t.TempDir(), "granite.gguf")
	if err := os.WriteFile(weights, []byte("gguf"), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	parrotSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/ensure" || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "bad request", http.StatusUnsupportedMediaType)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"status":{"state":"downloading","percent":40}}`)
			return
		}
		fmt.Fprintf(w, `{"path":%q,"status":{"state":"seeding","percent":100}}`, weights)
	}))
	defer parrotSrv.Close()

	model := fmt.Sprintf("  - name: sourced\n    engine: llamacpp\n    source: viiwork-parrot:granite4.2-8b-q8_0\n    context: 512\n    parallel: 2\n"+
		"    llamacpp:\n      binary: %q\n      threads: 1\n"+
		"    env:\n      NODE_HELPER: llama-server\n      FAKE_REQUIRE_MODEL: %q\n", os.Args[0], weights)
	extra := "viiwork_parrot:\n  api: " + strings.TrimPrefix(parrotSrv.URL, "http://") + "\n"
	tn := startNodeYAML(t, meshtest.NewNetwork(), "n1", []string{model}, extra, nil)

	deadline := time.Now().Add(20 * time.Second)
	for !healthyModel(tn.status(t), "sourced") {
		if time.Now().After(deadline) {
			t.Fatalf("sourced model not healthy; log:\n%s", tn.log.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("/ensure called %d times, want 3", n)
	}
	if !strings.Contains(tn.log.String(), "fetching viiwork-parrot:granite4.2-8b-q8_0") {
		t.Errorf("no progress line in the node log")
	}
}
