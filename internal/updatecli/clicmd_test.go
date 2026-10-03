package updatecli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/janit/viiwork/v2/internal/release"
	"github.com/janit/viiwork/v2/internal/update"
	"github.com/janit/viiwork/v2/internal/update/updatetest"
	"github.com/janit/viiwork/v2/meshapi"
)

// cliHost is one machine for `update cli`: a node on loopback running
// version, its state directory with that release staged by the node, the
// signed release served as GitHub would, and an old CLI.
type cliHost struct {
	env      Env
	out, err *bytes.Buffer
	node     string
	stateDir string
	cli      string
	staged   string
}

const oldCLI = "#!/bin/sh\necho v2.6.0\n"

func newCLIHost(t *testing.T, version string) *cliHost {
	t.Helper()
	assets, pub := updatetest.Assets(t, version, updatetest.Opts{})
	src, _ := updatetest.Serve(t, version, assets)
	stateDir := t.TempDir()
	st := &update.Stager{Dir: update.ReleasesDir(stateDir), Source: src, Client: &http.Client{},
		Keys: []ed25519.PublicKey{pub}, Target: release.Target{OS: runtime.GOOS, Arch: runtime.GOARCH}}
	if err := st.Stage(context.Background(), version); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != meshapi.PathUpdate {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(meshapi.UpdateStatus{Enabled: true, Running: version, Current: version, LastGood: version})
	}))
	t.Cleanup(srv.Close)
	cli := filepath.Join(t.TempDir(), "viiwork")
	os.WriteFile(cli, []byte(oldCLI), 0o755)
	h := &cliHost{out: &bytes.Buffer{}, err: &bytes.Buffer{}, node: strings.TrimPrefix(srv.URL, "http://"),
		stateDir: stateDir, cli: cli, staged: filepath.Join(update.ReleasesDir(stateDir), version, "viiwork")}
	h.env = Env{Stdout: h.out, Stderr: h.err, LookupEnv: func(string) (string, bool) { return "", false },
		Keys: []ed25519.PublicKey{pub}, UpdateSource: src,
		Executable: func() (string, error) { return cli, nil }}
	return h
}

func (h *cliHost) run(args ...string) int {
	h.out.Reset()
	h.err.Reset()
	return Run(context.Background(), append([]string{"cli", "--node", h.node, "--state-dir", h.stateDir}, args...), h.env)
}

func TestUpdateCLI(t *testing.T) {
	h := newCLIHost(t, "v2.7.2")
	if code := h.run(); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	want, _ := os.ReadFile(h.staged)
	if b, _ := os.ReadFile(h.cli); !bytes.Equal(b, want) {
		t.Errorf("CLI %q", b)
	}
	if b, _ := os.ReadFile(h.cli + ".prev"); string(b) != oldCLI {
		t.Errorf(".prev %q", b)
	}
	if !strings.Contains(h.out.String(), "v2.6.0 -> v2.7.2") {
		t.Errorf("out %q", h.out)
	}
	entries, _ := os.ReadDir(filepath.Dir(h.cli))
	if len(entries) != 2 {
		t.Errorf("left behind: %v", entries)
	}

	// Again: the CLI is the release, so nothing is done.
	os.Remove(h.cli + ".prev")
	if code := h.run(); code != 0 || !strings.Contains(h.out.String(), "nothing to do") {
		t.Errorf("exit %d: %s %s", code, h.out, h.err)
	}
	if _, err := os.Stat(h.cli + ".prev"); err == nil {
		t.Error("reinstalled")
	}
}

// --to names another CLI; the running one is left alone.
func TestUpdateCLITo(t *testing.T) {
	h := newCLIHost(t, "v2.7.2")
	other := filepath.Join(t.TempDir(), "viiwork")
	os.WriteFile(other, []byte(oldCLI), 0o755)
	if code := h.run("--to", other); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	want, _ := os.ReadFile(h.staged)
	if b, _ := os.ReadFile(other); !bytes.Equal(b, want) {
		t.Errorf("--to CLI %q", b)
	}
	if b, _ := os.ReadFile(h.cli); string(b) != oldCLI {
		t.Errorf("the running CLI changed: %q", b)
	}
}

func TestUpdateCLIDryRun(t *testing.T) {
	h := newCLIHost(t, "v2.7.2")
	if code := h.run("--dry-run"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if !strings.Contains(h.out.String(), "would verify "+h.staged) || !strings.Contains(h.out.String(), h.cli+".prev") {
		t.Errorf("out %q", h.out)
	}
	if b, _ := os.ReadFile(h.cli); string(b) != oldCLI {
		t.Errorf("dry run wrote %q", b)
	}
	if entries, _ := os.ReadDir(filepath.Dir(h.cli)); len(entries) != 1 {
		t.Errorf("dry run left %v", entries)
	}
}

// The node's staged copy is in a directory its container can write: one
// that is not the signed release's viiwork is refused, and nothing changes.
func TestUpdateCLIRefusesAnUnverifiedCopy(t *testing.T) {
	h := newCLIHost(t, "v2.7.2")
	os.WriteFile(h.cli+".prev", []byte("older"), 0o755)
	os.WriteFile(h.staged, []byte("#!/bin/sh\necho v2.7.2\n# planted\n"), 0o755)
	if code := h.run(); code != 1 || !strings.Contains(h.err.String(), "not the signed v2.7.2 release") {
		t.Errorf("exit %d: %s", code, h.err)
	}
	if b, _ := os.ReadFile(h.cli); string(b) != oldCLI {
		t.Errorf("CLI %q", b)
	}
	if b, _ := os.ReadFile(h.cli + ".prev"); string(b) != "older" {
		t.Errorf(".prev %q", b)
	}

	// A release whose signature does not hold is refused the same way.
	h = newCLIHost(t, "v2.7.2")
	_, other := updatetest.Assets(t, "v2.7.2", updatetest.Opts{})
	h.env.Keys = []ed25519.PublicKey{other}
	if code := h.run(); code != 1 || !strings.Contains(h.err.String(), "not installing") {
		t.Errorf("exit %d: %s", code, h.err)
	}
	if b, _ := os.ReadFile(h.cli); string(b) != oldCLI {
		t.Errorf("CLI %q", b)
	}
}

func TestUpdateCLIRefusals(t *testing.T) {
	// The release is not in the state directory.
	h := newCLIHost(t, "v2.7.2")
	os.RemoveAll(filepath.Dir(h.staged))
	if code := h.run(); code != 1 || !strings.Contains(h.err.String(), "is missing") {
		t.Errorf("missing release: exit %d: %s", code, h.err)
	}

	// A link planted there is not followed.
	h = newCLIHost(t, "v2.7.2")
	os.Remove(h.staged)
	os.Symlink("/etc/passwd", h.staged)
	if code := h.run(); code != 1 || !strings.Contains(h.err.String(), "not a regular file") {
		t.Errorf("link: exit %d: %s", code, h.err)
	}

	// No permission to write the CLI's directory.
	if os.Geteuid() != 0 {
		h = newCLIHost(t, "v2.7.2")
		os.Chmod(filepath.Dir(h.cli), 0o555)
		defer os.Chmod(filepath.Dir(h.cli), 0o755)
		if code := h.run(); code != 1 || !strings.Contains(h.err.String(), "sudo") {
			t.Errorf("read-only: exit %d: %s", code, h.err)
		}
		if b, _ := os.ReadFile(h.cli); string(b) != oldCLI {
			t.Errorf("CLI %q", b)
		}
	}

	// No state directory to be found.
	h = newCLIHost(t, "v2.7.2")
	code := Run(context.Background(), []string{"cli", "--node", h.node}, h.env)
	if code != 1 || !strings.Contains(h.err.String(), "--state-dir") {
		t.Errorf("no state dir: exit %d: %s", code, h.err)
	}

	// The state directory comes from the config.
	h = newCLIHost(t, "v2.7.2")
	cfg := filepath.Join(t.TempDir(), "viiwork.yaml")
	os.WriteFile(cfg, []byte("node: {name: n, state_dir: "+h.stateDir+"}\nupdate: {enabled: true}\n"), 0o644)
	h.env.DefaultConfig = cfg
	if code := Run(context.Background(), []string{"cli", "--node", h.node}, h.env); code != 0 {
		t.Errorf("state dir from the config: exit %d: %s", code, h.err)
	}

	// No node to ask.
	h = newCLIHost(t, "v2.7.2")
	if code := Run(context.Background(), []string{"cli", "--node", "127.0.0.1:1", "--state-dir", h.stateDir}, h.env); code != 1 ||
		!strings.Contains(h.err.String(), "which release it runs") {
		t.Errorf("no node: exit %d: %s", code, h.err)
	}
}
