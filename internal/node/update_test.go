package node

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/update"
	"github.com/janit/viiwork/v2/mesh/meshtest"
	"github.com/janit/viiwork/v2/meshapi"
)

func postUpdate(t *testing.T, url, body string) int {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestUpdateDisabledByDefault(t *testing.T) {
	tn := startNode(t, meshtest.NewNetwork(), "node-a", nil, "", nil)
	var st meshapi.UpdateStatus
	if code := getJSON(t, tn.url(meshapi.PathUpdate), &st); code != 200 || st.Enabled || st.Running != "v2-test" || st.Current != meshapi.UpdateBuiltin {
		t.Fatalf("GET %d %+v", code, st)
	}
	if code := postUpdate(t, tn.url(meshapi.PathUpdateStage), `{"version":"v2.6.0"}`); code != 403 {
		t.Errorf("stage on a disabled node: %d", code)
	}
}

func TestActivateRestartsTheNode(t *testing.T) {
	tn := startNode(t, meshtest.NewNetwork(), "node-a", nil, "update:\n  enabled: true\n", nil)
	dir := update.ReleasesDir(tn.stateDir)
	vdir := filepath.Join(dir, "v9.0.0")
	os.MkdirAll(vdir, 0o755)
	os.WriteFile(filepath.Join(vdir, "viiwork"), []byte("BIN"), 0o755)
	sum := sha256.Sum256([]byte("BIN"))
	os.WriteFile(filepath.Join(vdir, "viiwork.sha256"), []byte(hex.EncodeToString(sum[:])+"\n"), 0o644)

	if code := postUpdate(t, tn.url(meshapi.PathUpdateActivate), `{"version":"v9.0.0"}`); code != 202 {
		t.Fatalf("activate: %d", code)
	}
	select {
	case err := <-tn.done:
		if !errors.Is(err, ErrRestart) {
			t.Fatalf("Run returned %v, want ErrRestart", err)
		}
		tn.done <- err // for the harness's cleanup
	case <-time.After(20 * time.Second):
		t.Fatal("the node did not shut down for its restart")
	}
	st, err := update.LoadState(dir)
	if err != nil || st.Current != "v9.0.0" || st.Pending == nil {
		t.Fatalf("state = %+v, %v", st, err)
	}
}
