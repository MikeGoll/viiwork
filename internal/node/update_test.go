package node

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/janit/viiwork/v2/internal/release"
	"github.com/janit/viiwork/v2/internal/update/updatetest"
	"github.com/janit/viiwork/v2/mesh"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/config"
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

// On a helper-managed install the node never restarts itself for an update:
// the helper's compose up does, on the new image.
func TestManagedActivateLeavesTheNodeRunning(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	cfgPath := filepath.Join(dir, "viiwork.yaml")
	os.MkdirAll(stateDir, 0o755)
	writeFile(t, cfgPath, nodeConfigYAML("node-a", stateDir, nil, "update:\n  enabled: true\n"))
	cfg, err := config.Load(cfgPath, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	tn := &testNode{cfgPath: cfgPath, stateDir: stateDir, log: &syncBuffer{}}
	n, err := New(cfg, Options{ConfigPath: cfgPath, Version: "v2-test", Log: tn.log, LookupEnv: noEnv, Listen: loopbackListen,
		MeshTune: meshTune(meshtest.NewNetwork(), "node-a", &tn.gossip), Managed: true})
	if err != nil {
		t.Fatal(err)
	}
	tn.Node = n
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()
	<-n.Ready()

	rel := update.ReleasesDir(stateDir)
	vdir := filepath.Join(rel, "v9.0.0")
	os.MkdirAll(vdir, 0o755)
	os.WriteFile(filepath.Join(vdir, "viiwork"), []byte("BIN"), 0o755)
	sum := sha256.Sum256([]byte("BIN"))
	os.WriteFile(filepath.Join(vdir, "viiwork.sha256"), []byte(hex.EncodeToString(sum[:])+"\n"), 0o644)
	if code := postUpdate(t, tn.url(meshapi.PathUpdateActivate), `{"version":"v9.0.0"}`); code != 409 {
		t.Fatalf("activate before the helper prepared the image: %d", code)
	}
	os.WriteFile(filepath.Join(rel, update.ImageResultFile), []byte(`{"version":"v9.0.0","id":"`+strings.Repeat("a", 32)+`","ok":true}`), 0o644)
	if code := postUpdate(t, tn.url(meshapi.PathUpdateActivate), `{"version":"v9.0.0"}`); code != 202 {
		t.Fatalf("activate: %d", code)
	}
	select {
	case err := <-done:
		done <- err
		t.Fatalf("the node stopped itself: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if st, err := update.LoadState(rel); err != nil || st.Current != "v9.0.0" || st.Pending == nil {
		t.Fatalf("state = %+v, %v", st, err)
	}
}

// fakeParrot answers /ensure-release with dir and records the request.
func fakeParrot(t *testing.T, dir string) (api string, got *struct {
	Repo, Version string
	Peers         []string
}) {
	got = &struct {
		Repo, Version string
		Peers         []string
	}{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(got)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"path": dir})
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), got
}

// GitHub up: the node takes the signed files from GitHub and the archive
// from its parrot, telling parrot the mesh's other members as peers.
func TestStageThroughParrot(t *testing.T) {
	assets, pub := updatetest.Assets(t, "v9.1.0", updatetest.Opts{})
	github, hits := updatetest.Serve(t, "v9.1.0", assets)
	api, got := fakeParrot(t, updatetest.WriteDir(t, assets))

	net := meshtest.NewNetwork()
	extra := "update:\n  enabled: true\nviiwork_parrot:\n  api: " + api + "\n"
	opts := func(o *Options) { o.ReleaseKeys = []ed25519.PublicKey{pub}; o.UpdateSource = github }
	a := startNodeOpts(t, net, "node-a", nil, extra, nil, opts)
	seedA := func(tn *testNode) func(*mesh.Options) {
		return meshTune(net, "node-b", &tn.gossip, func() netip.AddrPort { return a.gossip })
	}
	startNodeOpts(t, net, "node-b", nil, extra, seedA, opts)
	until(t, 10*time.Second, "node-b alive in node-a's view", func() bool { return len(a.meshPeers()) == 1 })
	want := a.meshPeers()

	if code := postUpdate(t, a.url(meshapi.PathUpdateStage), `{"version":"v9.1.0"}`); code != 200 {
		t.Fatalf("stage: %d\n%s", code, a.log.String())
	}
	if got.Repo != "janit/viiwork" || got.Version != "v9.1.0" {
		t.Errorf("parrot asked for %+v", got)
	}
	if len(got.Peers) != 1 || got.Peers[0] != want[0] {
		t.Errorf("peers = %v, want %v (the other member, not this node)", got.Peers, want)
	}
	name := release.ArchiveName("v9.1.0", release.Target{OS: runtime.GOOS, Arch: runtime.GOARCH})
	if hits.Get("SHA256SUMS.sig") != 1 || hits.Get(name) != 0 {
		t.Errorf("GitHub served the signature %d times and the archive %d times; want 1 and 0", hits.Get("SHA256SUMS.sig"), hits.Get(name))
	}
	if _, err := update.Binary(update.ReleasesDir(a.stateDir), "v9.1.0"); err != nil {
		t.Error(err)
	}
}

// GitHub unreachable: the node stages entirely from its parrot, and says so.
func TestStageThroughParrotWhileGitHubIsDown(t *testing.T) {
	assets, pub := updatetest.Assets(t, "v9.1.0", updatetest.Opts{})
	api, _ := fakeParrot(t, updatetest.WriteDir(t, assets))
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()

	extra := "update:\n  enabled: true\nviiwork_parrot:\n  api: " + api + "\n"
	a := startNodeOpts(t, meshtest.NewNetwork(), "node-a", nil, extra, nil, func(o *Options) {
		o.ReleaseKeys = []ed25519.PublicKey{pub}
		o.UpdateSource = down.URL
	})
	if code := postUpdate(t, a.url(meshapi.PathUpdateStage), `{"version":"v9.1.0"}`); code != 200 {
		t.Fatalf("stage: %d\n%s", code, a.log.String())
	}
	if !strings.Contains(a.log.String(), "GitHub unreachable") {
		t.Errorf("log does not say GitHub was unreachable:\n%s", a.log.String())
	}
}
