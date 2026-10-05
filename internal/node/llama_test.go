package node

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/setup/install"
	"github.com/janit/viiwork/v2/internal/update"
	"github.com/janit/viiwork/v2/mesh/meshtest"
)

// llamaModel is a llamacpp model configured with binary ("" writes no block).
func llamaModel(t *testing.T, name, binary string) config.Model {
	t.Helper()
	m := config.Model{Name: name, Engine: "llamacpp", Path: "/models/" + name + ".gguf"}
	if binary != "" {
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte("binary: "+strconv.Quote(binary)+"\nthreads: 1\n"), &doc); err != nil {
			t.Fatal(err)
		}
		m.Options = map[string]yaml.Node{"llamacpp": *doc.Content[0]}
	}
	return m
}

func binaryOf(m config.Model) string { b, _ := llamaBinary(m); return b }

// writeServer puts an executable llama-server for tag under root that
// reports build n.
func writeServer(t *testing.T, root, tag string) string {
	t.Helper()
	p := install.LlamaServerPath(root, tag)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho 'version: "+strings.TrimPrefix(tag, "b")+" (abc)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFollowModels(t *testing.T) {
	root := t.TempDir()
	writeServer(t, root, "b1")
	writeServer(t, root, "b2")
	l := llamaFollow{root: root, pin: "b2", present: install.Fetched}
	models := []config.Model{
		llamaModel(t, "managed", install.LlamaServerPath(root, "b1")),
		llamaModel(t, "absent-tag", install.LlamaServerPath(root, "b9")),
		llamaModel(t, "elsewhere", "/opt/homebrew/bin/llama-server"),
		llamaModel(t, "path", ""),
		{Name: "vllm", Engine: "vllm"},
	}
	before := binaryOf(models[0])
	got := l.follow(models, "b2")
	want := []string{install.LlamaServerPath(root, "b2"), install.LlamaServerPath(root, "b2"), "/opt/homebrew/bin/llama-server", "", ""}
	for i, m := range got {
		if binaryOf(m) != want[i] {
			t.Errorf("%s runs %q, want %q", m.Name, binaryOf(m), want[i])
		}
	}
	if binaryOf(models[0]) != before {
		t.Error("follow changed the configured models")
	}
	// The block keeps everything else it said.
	if th := got[0].EngineBlock().Content[3].Value; th != "1" {
		t.Errorf("threads %q", th)
	}
	// The pin's build absent: every model runs as configured.
	if got := l.follow(models, "b3"); binaryOf(got[0]) != before {
		t.Errorf("followed a build that is not there: %s", binaryOf(got[0]))
	}
	if got := (llamaFollow{}).follow(models, "b2"); binaryOf(got[0]) != before {
		t.Error("a node with no llama root followed its pin")
	}
}

func writeManifest(t *testing.T, man install.Manifest) string {
	t.Helper()
	man.Version = 1
	b, _ := json.Marshal(man)
	p := filepath.Join(t.TempDir(), "install.json")
	os.WriteFile(p, b, 0o644)
	return p
}

func TestNewLlamaFollowReadsTheManifest(t *testing.T) {
	managed := llamaModel(t, "m", "/Users/u/llama.cpp/b1/llama-b1/llama-server")
	noop := func(string, ...any) {}
	cases := []struct {
		name     string
		manifest string
		want     string
	}{
		{"recorded", writeManifest(t, install.Manifest{OS: "darwin", LlamaRoot: "/Users/u/root"}), "/Users/u/root"},
		{"older manifest", writeManifest(t, install.Manifest{OS: "darwin"}), "/Users/u/llama.cpp"},
		{"no manifest", filepath.Join(t.TempDir(), "absent.json"), ""},
		{"not a wizard host", "", ""},
	}
	for _, c := range cases {
		l := newLlamaFollow(Options{InstallManifest: c.manifest, LlamaPin: "b2"}, []config.Model{managed}, noop)
		if l.root != c.want {
			t.Errorf("%s: root %q", c.name, l.root)
		}
	}
}

// End to end on a real node: the configured build does not even exist, so
// the model can only come up by running the build of the node's own pin.
func TestNodeRunsTheBuildOfItsPin(t *testing.T) {
	root := t.TempDir()
	server := install.LlamaServerPath(root, "b2")
	os.MkdirAll(filepath.Dir(server), 0o755)
	self, _ := filepath.Abs(os.Args[0])
	if err := os.Symlink(self, server); err != nil {
		t.Fatal(err)
	}
	configured := install.LlamaServerPath(root, "b1")
	model := strings.Replace(fakeModel{name: "m"}.yaml(), strconv.Quote(os.Args[0]), strconv.Quote(configured), 1)
	manifest := writeManifest(t, install.Manifest{OS: "darwin", LlamaRoot: root})
	tn := startNodeOpts(t, meshtest.NewNetwork(), "node-a", []string{model}, "", nil, func(o *Options) {
		o.LlamaPin, o.InstallManifest = "b2", manifest
	})
	waitForModels(t, tn, "m")
	if !strings.Contains(tn.log.String(), "model m runs "+server) {
		t.Errorf("log:\n%s", tn.log.String())
	}
	if got := binaryOf(tn.runningConfig().Models[0]); got != configured {
		t.Errorf("the running configuration says %s: it should say what the operator wrote", got)
	}
}

func llamaTarball(t *testing.T, tag string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := "#!/bin/sh\necho 'version: " + strings.TrimPrefix(tag, "b") + " (abc)'\n"
	tw.WriteHeader(&tar.Header{Name: "llama-" + tag + "/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: "llama-" + tag + "/llama-server", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))})
	tw.Write([]byte(body))
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestPrepareLlamaFetchesAndChecksTheStagedBuild(t *testing.T) {
	archive := llamaTarball(t, "b200")
	sum := sha256.Sum256(archive)
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/download/b200/llama-b200-bin-macos-arm64.tar.gz" {
			w.Write(archive)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	root := t.TempDir()
	writeServer(t, root, "b100")
	models := []config.Model{llamaModel(t, "m", install.LlamaServerPath(root, "b100"))}
	n := &Node{llama: llamaFollow{root: root, pin: "b100", present: install.Fetched}}
	fetch := install.Fetch{HTTP: srv.Client(), API: srv.URL + "/api/", Releases: srv.URL + "/download/", Out: io.Discard,
		Exec: func(context.Context, io.Writer, string, ...string) error { return nil }}
	prepare := n.prepareLlama(fetch, func() []config.Model { return models })
	ctx := context.Background()
	required := map[string]string{"llamacpp": "b150"}
	if err := update.CheckEngines(ctx, models, required); err == nil {
		t.Fatal("the running build passes a requirement it does not meet")
	}

	check, err := prepare(ctx, update.BuildInfo{Version: "v2.7.0", LlamaCpp: "b200", LlamaCppMacSHA256: hex.EncodeToString(sum[:])})
	if err != nil {
		t.Fatal(err)
	}
	if !install.Fetched(root, "b200") || !slices.Equal(paths, []string{"/download/b200/llama-b200-bin-macos-arm64.tar.gz"}) {
		t.Errorf("fetched %v, requests %q", install.Fetched(root, "b200"), paths)
	}
	if err := check(ctx, required); err != nil {
		t.Errorf("judged against the running build: %v", err)
	}

	// A digest that does not match is a stage that fails.
	if _, err := prepare(ctx, update.BuildInfo{LlamaCpp: "b300", LlamaCppMacSHA256: strings.Repeat("0", 64)}); err == nil {
		t.Error("prepared a build that is not there")
	}
	// A release without the digest predates following: nothing to do.
	if check, err := prepare(ctx, update.BuildInfo{LlamaCpp: "b200"}); check != nil || err != nil {
		t.Errorf("%v %v", check != nil, err)
	}
	if _, err := prepare(ctx, update.BuildInfo{LlamaCpp: "../x", LlamaCppMacSHA256: "ab"}); err == nil {
		t.Error("accepted a pin that is a path")
	}
}

// stageScript stages a release whose binary reports pin in --build-info.
func stageScript(t *testing.T, dir, version, pin string) {
	t.Helper()
	vdir := filepath.Join(dir, version)
	os.MkdirAll(vdir, 0o755)
	body := []byte("#!/bin/sh\necho '{\"version\":\"" + version + "\",\"llama_cpp\":\"" + pin + "\"}'\n")
	os.WriteFile(filepath.Join(vdir, "viiwork"), body, 0o755)
	sum := sha256.Sum256(body)
	os.WriteFile(filepath.Join(vdir, "viiwork.sha256"), []byte(hex.EncodeToString(sum[:])+"\n"), 0o644)
}

func pruneFixture(t *testing.T) (n *Node, dir, root string, st update.State, logs *bytes.Buffer) {
	root = t.TempDir()
	for _, tag := range []string{"b1", "b2", "b3", "b4", "b5"} {
		writeServer(t, root, tag)
	}
	dir = t.TempDir()
	stageScript(t, dir, "v2.6.1", "b2")
	stageScript(t, dir, "v2.6.2", "b3")
	floor := filepath.Join(t.TempDir(), "viiwork")
	os.WriteFile(floor, []byte("#!/bin/sh\necho '{\"version\":\"v2.6.0\",\"llama_cpp\":\"b1\"}'\n"), 0o755)
	logs = &bytes.Buffer{}
	n = &Node{llama: llamaFollow{root: root, pin: "b3", present: install.Fetched}, logger: log.New(logs, "", 0),
		cfg: &config.Config{Models: []config.Model{llamaModel(t, "m", install.LlamaServerPath(root, "b5"))}}}
	st = update.State{Current: "v2.6.2", LastGood: "v2.6.2", Previous: "v2.6.1", Launcher: &update.Launcher{Path: floor}}
	return
}

func TestPruneLlamaKeepsWhatAKeptReleaseRuns(t *testing.T) {
	n, dir, root, st, logs := pruneFixture(t)
	n.pruneLlama(dir)(st)
	for tag, want := range map[string]bool{"b1": true, "b2": true, "b3": true, "b4": false, "b5": true} {
		if got := install.Fetched(root, tag); got != want {
			t.Errorf("%s present %v, want %v", tag, got, want)
		}
	}
	if !strings.Contains(logs.String(), "removed [b4]") {
		t.Errorf("log: %s", logs)
	}
}

func TestPruneLlamaRemovesNothingItCannotAccountFor(t *testing.T) {
	n, dir, root, st, logs := pruneFixture(t)
	os.WriteFile(filepath.Join(dir, "v2.6.1", "viiwork"), []byte("tampered"), 0o755)
	n.pruneLlama(dir)(st)
	if !install.Fetched(root, "b4") || !strings.Contains(logs.String(), "keeping every build") {
		t.Errorf("b4 present %v; log: %s", install.Fetched(root, "b4"), logs)
	}
	st.Launcher = nil
	n.pruneLlama(dir)(st)
	if !install.Fetched(root, "b4") {
		t.Error("pruned without knowing the floor")
	}
}
