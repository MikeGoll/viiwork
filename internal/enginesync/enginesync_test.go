package enginesync

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/release"
	"github.com/janit/viiwork/v2/internal/setup/install"
	"github.com/janit/viiwork/v2/internal/update"
	"github.com/janit/viiwork/v2/internal/update/updatetest"
)

const repo = "ghcr.io/janit/viiwork-llamacpp-cuda"

// script is the "viiwork" a fake release carries: it only reports its
// version, which is all staging runs it for.
func script(version string) []byte {
	return []byte("#!/bin/sh\necho " + version + "\n")
}

// releases serves signed fake releases of every version asked for, from a
// key only the returned helper trusts.
func releases(t *testing.T) (string, ed25519.PublicKey) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	target := release.Target{OS: runtime.GOOS, Arch: runtime.GOARCH}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		version, asset, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		name := release.ArchiveName(version, target)
		top := strings.TrimSuffix(name, ".tar.gz")
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		tw.WriteHeader(&tar.Header{Name: top + "/", Typeflag: tar.TypeDir, Mode: 0o755})
		body := script(version)
		tw.WriteHeader(&tar.Header{Name: top + "/viiwork", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))})
		tw.Write(body)
		tw.Close()
		gz.Close()
		var sums strings.Builder
		for _, tg := range release.Targets {
			n := release.ArchiveName(version, tg)
			sum := sha256.Sum256([]byte(n))
			if tg == target {
				sum = sha256.Sum256(buf.Bytes())
			}
			fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), n)
		}
		switch asset {
		case name:
			w.Write(buf.Bytes())
		case "SHA256SUMS":
			w.Write([]byte(sums.String()))
		case "SHA256SUMS.sig":
			w.Write(release.Sign(priv, version, []byte(sums.String())))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, pub
}

// docker is a fake docker CLI. images maps a pulled reference to the
// viiwork its /usr/local/bin holds.
type docker struct {
	mu     sync.Mutex
	calls  []string
	images map[string][]byte
	local  map[string]bool // images on this host
	labels map[string]string
	fail   map[string]bool // a command prefix that fails
}

func (d *docker) run(_ context.Context, name string, args ...string) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := name + " " + strings.Join(args, " ")
	d.calls = append(d.calls, c)
	for prefix := range d.fail {
		if strings.HasPrefix(c, prefix) {
			return nil, errors.New("exit 1")
		}
	}
	if name != "docker" {
		return nil, errors.New("not docker")
	}
	switch {
	case args[0] == "pull":
		if _, ok := d.images[args[1]]; !ok {
			return nil, errors.New("manifest unknown")
		}
		d.local[args[1]] = true
		return []byte("pulled\n"), nil
	case args[0] == "image" && args[1] == "rm":
		if !d.local[args[2]] {
			return nil, errors.New("no such image")
		}
		delete(d.local, args[2])
		return nil, nil
	case args[0] == "image" && args[1] == "inspect" && args[3] == "{{.Id}}":
		if !d.local[args[4]] {
			return nil, errors.New("no such image")
		}
		return []byte("sha256:" + strings.Repeat("d", 64) + "\n"), nil
	case args[0] == "create":
		return []byte("Unable to find image locally\n" + strings.Repeat("ab", 32) + "\n"), nil
	case args[0] == "cp":
		// The ref the last create was for.
		var ref string
		for i := len(d.calls) - 1; i >= 0; i-- {
			if r, ok := strings.CutPrefix(d.calls[i], "docker create "); ok {
				ref = r
				break
			}
		}
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		body := d.images[ref]
		tw.WriteHeader(&tar.Header{Name: "viiwork", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))})
		tw.Write(body)
		tw.Close()
		return buf.Bytes(), nil
	case args[0] == "image" && args[1] == "inspect":
		ref := args[len(args)-1]
		repo, _, _ := strings.Cut(ref, ":v")
		if strings.Contains(args[3], "RepoDigests") {
			return []byte(`["` + repo + `@sha256:` + strings.Repeat("c", 64) + `"]` + "\n"), nil
		}
		b, _ := json.Marshal(d.labels)
		return append(b, '\n'), nil
	}
	return nil, nil
}

func (d *docker) called(prefix string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.ContainsFunc(d.calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

// hostCLI is the host's viiwork before any update: it reports v2.6.0.
const hostCLI = "#!/bin/sh\necho v2.6.0\n# host binary\n"

type env struct {
	h      *Helper
	d      *docker
	root   string
	logs   *strings.Builder
	source string
	cli    string // the host CLI install.json records
}

// newEnv is a helper-managed install under a temporary root, running image
// tag, with the published images of v2.6.0 and v2.6.1 on the registry.
func newEnv(t *testing.T, tag string) *env {
	t.Helper()
	root := t.TempDir()
	src, pub := releases(t)
	p := Paths{
		Manifest: filepath.Join(root, "etc/viiwork/install.json"),
		Config:   filepath.Join(root, "etc/viiwork/viiwork.yaml"),
		StateDir: filepath.Join(root, "var/lib/viiwork"),
		Dir:      filepath.Join(root, "var/lib/viiwork-engine"),
	}
	cli := filepath.Join(root, "usr/local/bin/viiwork")
	compose := filepath.Join(root, "etc/viiwork/docker-compose.yaml")
	for _, d := range []string{filepath.Dir(p.Manifest), filepath.Join(p.StateDir, "releases"), filepath.Dir(cli)} {
		os.MkdirAll(d, 0o755)
	}
	m := install.Manifest{Version: 1, OS: "linux", Compose: &install.Compose{File: compose, Project: "viiwork"},
		EngineHelper: &install.EngineHelper{Units: install.EngineUnits, Dir: p.Dir}, Binary: cli}
	b, _ := json.Marshal(m)
	os.WriteFile(p.Manifest, b, 0o644)
	os.WriteFile(p.Config, []byte("node: {name: n}\nupdate: {enabled: true}\n"), 0o644)
	os.WriteFile(compose, []byte("name: viiwork\nservices:\n  viiwork:\n    image: "+repo+":"+tag+"\n    container_name: viiwork\n"), 0o644)
	os.WriteFile(cli, []byte(hostCLI), 0o755)
	d := &docker{
		images: map[string][]byte{repo + ":v2.6.0": script("v2.6.0"), repo + ":v2.6.1": script("v2.6.1"),
			repo + ":v2.6.2": script("v2.6.2"), repo + ":v2.6.3": script("v2.6.3")},
		local: map[string]bool{repo + ":" + tag: true},
		labels: map[string]string{"org.opencontainers.image.version": "v2.6.1",
			"org.opencontainers.image.base.name": "ghcr.io/ggml-org/llama.cpp:server-cuda-b10438"},
		fail: map[string]bool{},
	}
	logs := &strings.Builder{}
	h := &Helper{Paths: p, Version: "v2.6.0", Target: release.Target{OS: runtime.GOOS, Arch: runtime.GOARCH},
		Keys: []ed25519.PublicKey{pub}, Client: &http.Client{}, Run: d.run, Source: src,
		Log: func(f string, a ...any) { fmt.Fprintf(logs, f+"\n", a...) }}
	return &env{h: h, d: d, root: root, logs: logs, source: src, cli: cli}
}

func (e *env) compose() string {
	b, _ := os.ReadFile(filepath.Join(e.root, "etc/viiwork/docker-compose.yaml"))
	return string(b)
}

func (e *env) state(s string) {
	os.WriteFile(filepath.Join(e.h.StateDir, "releases", "state.json"), []byte(s), 0o644)
}

func TestSwapsToCurrent(t *testing.T) {
	e := newEnv(t, "v2.6.0")
	e.state(`{"current":"v2.6.1","last_good":"builtin","pending":{"version":"v2.6.1","attempts":0,"baseline":[]}}`)
	if err := e.h.Sync(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.logs)
	}
	if !strings.Contains(e.compose(), "    image: "+repo+":v2.6.1\n    container_name") {
		t.Errorf("compose:\n%s", e.compose())
	}
	bak, _ := os.ReadFile(filepath.Join(e.root, "etc/viiwork/docker-compose.yaml.bak"))
	if !strings.Contains(string(bak), repo+":v2.6.0") {
		t.Errorf("backup:\n%s", bak)
	}
	for _, c := range []string{"docker pull " + repo + ":v2.6.1", "docker create " + repo + ":v2.6.1", "docker cp ", "docker rm -f ",
		"docker compose -f " + filepath.Join(e.root, "etc/viiwork/docker-compose.yaml") + " -p viiwork up -d"} {
		if !e.d.called(c) {
			t.Errorf("no %q in %q", c, e.d.calls)
		}
	}
	rec := readRecord(t, e)
	if rec.Original != "v2.6.0" || rec.Swapped == nil || rec.Swapped.Version != "v2.6.1" || !strings.Contains(rec.Swapped.Image, "@sha256:") {
		t.Errorf("record %+v", rec)
	}
	if fi, err := os.Stat(e.h.Dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("helper dir %v %v", fi, err)
	}
	// The host binary waits for the release to confirm.
	if b, _ := os.ReadFile(e.cli); string(b) != hostCLI {
		t.Error("the host binary was replaced before the release confirmed")
	}

	// Run again: the tag is current, so nothing is pulled or restarted.
	e.d.calls = nil
	if err := e.h.Sync(context.Background()); err != nil || len(e.d.calls) != 0 {
		t.Errorf("second run: %v, calls %q", err, e.d.calls)
	}
}

func readRecord(t *testing.T, e *env) record {
	t.Helper()
	var r record
	b, err := os.ReadFile(filepath.Join(e.h.Dir, recordFile))
	if err != nil || json.Unmarshal(b, &r) != nil {
		t.Fatalf("record: %s, %v", b, err)
	}
	return r
}

// Builtin means the tag the install had when the helper first saw it.
func TestBuiltinIsTheOriginalTag(t *testing.T) {
	e := newEnv(t, "v2.6.0")
	if err := e.h.Sync(context.Background()); err != nil || len(e.d.calls) != 0 {
		t.Fatalf("fresh: %v, calls %q", err, e.d.calls)
	}
	e.state(`{"current":"v2.6.1","last_good":"builtin"}`)
	if err := e.h.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.state(`{"current":"builtin","last_good":"builtin"}`)
	if err := e.h.Sync(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.logs)
	}
	if !strings.Contains(e.compose(), repo+":v2.6.0\n") {
		t.Errorf("not back on the original tag:\n%s", e.compose())
	}
	if rec := readRecord(t, e); rec.Original != "v2.6.0" {
		t.Errorf("record %+v", rec)
	}
}

func TestRefusesAnImageThatIsNotTheSignedRelease(t *testing.T) {
	e := newEnv(t, "v2.6.0")
	e.d.images[repo+":v2.6.1"] = []byte("#!/bin/sh\necho v2.6.1 # but not the signed bytes\n")
	e.state(`{"current":"v2.6.1","last_good":"builtin"}`)
	err := e.h.Sync(context.Background())
	if err == nil || !strings.Contains(err.Error(), "signed") {
		t.Errorf("err %v", err)
	}
	if !strings.Contains(e.compose(), repo+":v2.6.0\n") || e.d.called("docker compose") {
		t.Errorf("swapped anyway:\n%s\n%q", e.compose(), e.d.calls)
	}
	if !e.d.called("docker rm -f ") {
		t.Error("the container created to read the image was left behind")
	}
}

// A compose up that fails leaves the file as it was, so the next run tries
// again rather than finding nothing to do.
func TestAFailedSwapIsUndone(t *testing.T) {
	e := newEnv(t, "v2.6.0")
	e.d.fail["docker compose"] = true
	e.state(`{"current":"v2.6.1","last_good":"builtin"}`)
	if err := e.h.Sync(context.Background()); err == nil {
		t.Fatal("no error")
	}
	if !strings.Contains(e.compose(), repo+":v2.6.0\n") {
		t.Errorf("compose:\n%s", e.compose())
	}
}

func TestRefusesAReleaseThatDoesNotVerify(t *testing.T) {
	e := newEnv(t, "v2.6.0")
	e.h.Keys = []ed25519.PublicKey{make(ed25519.PublicKey, ed25519.PublicKeySize)}
	e.state(`{"current":"v2.6.1","last_good":"builtin"}`)
	if err := e.h.Sync(context.Background()); !errors.Is(err, update.ErrUnverified) {
		t.Errorf("err %v", err)
	}
	if e.d.called("docker") {
		t.Errorf("docker ran for an unverified release: %q", e.d.calls)
	}
}

// Nothing in the node's directory is taken but a version name: not a path,
// not a link, not a file that would block.
func TestTrustsNothingTheContainerWrote(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "state.json")
	os.WriteFile(outside, []byte(`{"current":"v2.6.1","last_good":"v2.6.1"}`), 0o644)
	cases := map[string]func(e *env){
		"a path as current": func(e *env) { e.state(`{"current":"../../../etc","last_good":"builtin"}`) },
		"a symlinked state.json": func(e *env) {
			os.Symlink(outside, filepath.Join(e.h.StateDir, "releases", "state.json"))
		},
		"a symlink inside the state dir": func(e *env) {
			os.WriteFile(filepath.Join(e.h.StateDir, "other.json"), []byte(`{"current":"v2.6.1","last_good":"v2.6.1"}`), 0o644)
			os.Symlink("../other.json", filepath.Join(e.h.StateDir, "releases", "state.json"))
		},
		"a symlinked releases dir": func(e *env) {
			os.RemoveAll(filepath.Join(e.h.StateDir, "releases"))
			os.Symlink(filepath.Dir(outside), filepath.Join(e.h.StateDir, "releases"))
		},
		"a symlinked releases dir inside the state dir": func(e *env) {
			other := filepath.Join(e.h.StateDir, "elsewhere")
			os.MkdirAll(other, 0o755)
			os.WriteFile(filepath.Join(other, "state.json"), []byte(`{"current":"v2.6.1","last_good":"v2.6.1"}`), 0o644)
			os.RemoveAll(filepath.Join(e.h.StateDir, "releases"))
			os.Symlink("elsewhere", filepath.Join(e.h.StateDir, "releases"))
		},
		"a FIFO": func(e *env) { syscall.Mkfifo(filepath.Join(e.h.StateDir, "releases", "state.json"), 0o644) },
		"a huge file": func(e *env) {
			e.state(`{"current":"v2.6.1","last_good":"builtin","x":"` + strings.Repeat("x", 1<<20) + `"}`)
		},
	}
	for name, plant := range cases {
		e := newEnv(t, "v2.6.0")
		plant(e)
		done := make(chan error, 1)
		go func() { done <- e.h.Sync(context.Background()) }()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s: accepted", name)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: blocked", name)
		}
		if e.d.called("docker") || !strings.Contains(e.compose(), repo+":v2.6.0\n") {
			t.Errorf("%s: acted: %q", name, e.d.calls)
		}
	}
}

func TestRefusesAnImageFromAnotherRepository(t *testing.T) {
	e := newEnv(t, "v2.6.0")
	compose := filepath.Join(e.root, "etc/viiwork/docker-compose.yaml")
	os.WriteFile(compose, []byte("services:\n  viiwork:\n    image: registry.example/viiwork:v2.6.0\n"), 0o644)
	e.state(`{"current":"v2.6.1","last_good":"builtin"}`)
	if err := e.h.Sync(context.Background()); err == nil || e.d.called("docker") {
		t.Errorf("err %v, calls %q", err, e.d.calls)
	}
}

func TestAnswersAnImageRequest(t *testing.T) {
	e := newEnv(t, "v2.6.0")
	rel := filepath.Join(e.h.StateDir, "releases")
	id := strings.Repeat("1", 32)
	os.WriteFile(filepath.Join(rel, update.ImageRequestFile), []byte(`{"version":"v2.6.1","id":"`+id+`"}`), 0o644)
	// A link where the answer goes is replaced, never written through.
	victim := filepath.Join(t.TempDir(), "victim")
	os.WriteFile(victim, []byte("keep"), 0o644)
	os.Symlink(victim, filepath.Join(rel, update.ImageResultFile))
	if err := e.h.Sync(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.logs)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Error("wrote through a symlink")
	}
	fi, err := os.Lstat(filepath.Join(rel, update.ImageResultFile))
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o644 {
		t.Fatalf("result %v %v", fi, err)
	}
	var res update.ImageResult
	b, _ := os.ReadFile(filepath.Join(rel, update.ImageResultFile))
	json.Unmarshal(b, &res)
	if !res.OK || res.ID != id || res.Version != "v2.6.1" || res.Engines["llamacpp"] != "b10438" || !strings.HasPrefix(res.Image, repo+"@sha256:") {
		t.Errorf("result %+v", res)
	}
	if e.d.called("docker compose") || !strings.Contains(e.compose(), ":v2.6.0\n") {
		t.Error("a request swapped the image")
	}
	// Answered once.
	e.d.calls = nil
	e.h.Sync(context.Background())
	if e.d.called("docker pull") {
		t.Error("an answered request was prepared again")
	}

	// A failure is answered too, with the reason.
	os.WriteFile(filepath.Join(rel, update.ImageRequestFile), []byte(`{"version":"v2.7.0","id":"`+strings.Repeat("2", 32)+`"}`), 0o644)
	e.h.Sync(context.Background())
	b, _ = os.ReadFile(filepath.Join(rel, update.ImageResultFile))
	res = update.ImageResult{}
	json.Unmarshal(b, &res)
	if res.OK || res.Error == "" || res.Version != "v2.7.0" {
		t.Errorf("failed request: %+v", res)
	}

	// A request that is not a version and an id is ignored.
	os.WriteFile(filepath.Join(rel, update.ImageRequestFile), []byte(`{"version":"../x","id":"`+strings.Repeat("3", 32)+`"}`), 0o644)
	e.d.calls = nil
	e.h.Sync(context.Background())
	if e.d.called("docker") {
		t.Errorf("acted on a bad request: %q", e.d.calls)
	}
}

func TestEnginesFromLabels(t *testing.T) {
	for _, c := range []struct {
		repo, base string
		want       map[string]string
	}{
		{"ghcr.io/janit/viiwork-llamacpp-cuda", "ghcr.io/ggml-org/llama.cpp:server-cuda-b10438", map[string]string{"llamacpp": "b10438"}},
		{"ghcr.io/janit/viiwork-llamacpp-vulkan", "ghcr.io/ggml-org/llama.cpp:server-vulkan-b10438", map[string]string{"llamacpp": "b10438"}},
		{"ghcr.io/janit/viiwork-vllm", "vllm/vllm-openai:v0.11.2", map[string]string{"vllm": "0.11.2"}},
		{"ghcr.io/janit/viiwork-llamacpp-cuda", "", map[string]string{}},
		{"ghcr.io/janit/viiwork-llamacpp-cuda", "ghcr.io/ggml-org/llama.cpp:server-cuda", map[string]string{}},
	} {
		got := enginesOf(c.repo, map[string]string{"org.opencontainers.image.base.name": c.base})
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s %s: %v", c.repo, c.base, got)
		}
	}
}

func TestSelfUpdateAfterTheReleaseConfirms(t *testing.T) {
	e := newEnv(t, "v2.6.0")
	e.state(`{"current":"v2.6.1","last_good":"builtin","pending":{"version":"v2.6.1","attempts":1,"baseline":[]}}`)
	e.h.Sync(context.Background())
	if b, _ := os.ReadFile(e.cli); string(b) != hostCLI {
		t.Fatal("replaced while pending")
	}
	e.state(`{"current":"v2.6.1","last_good":"v2.6.1"}`)
	if err := e.h.Sync(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.logs)
	}
	if b, _ := os.ReadFile(e.cli); !bytes.Equal(b, script("v2.6.1")) {
		t.Errorf("host binary %q", b)
	}
	if b, _ := os.ReadFile(e.cli + ".prev"); string(b) != hostCLI {
		t.Errorf("previous %q", b)
	}
	if fi, _ := os.Stat(e.cli); fi.Mode().Perm() != 0o755 {
		t.Errorf("mode %v", fi.Mode())
	}
	if !strings.Contains(e.logs.String(), e.cli+": v2.6.0 -> v2.6.1") {
		t.Errorf("not logged:\n%s", e.logs)
	}
	entries, _ := os.ReadDir(filepath.Dir(e.cli))
	if len(entries) != 2 {
		t.Errorf("left behind: %v", entries)
	}

	// A CLI that reports the release is left alone, and nothing is fetched.
	src := e.h.Source
	e.h.Source = "http://127.0.0.1:1"
	os.Remove(e.cli + ".prev")
	if err := e.h.Sync(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.logs)
	}
	if _, err := os.Stat(e.cli + ".prev"); err == nil {
		t.Error("reinstalled a CLI that is the release")
	}
	e.h.Source = src

	// So is one past it.
	e = newEnv(t, "v2.6.1")
	os.WriteFile(e.cli, script("v2.7.0"), 0o755)
	e.state(`{"current":"v2.6.1","last_good":"v2.6.1"}`)
	e.h.Sync(context.Background())
	if b, _ := os.ReadFile(e.cli); !bytes.Equal(b, script("v2.7.0")) {
		t.Error("replaced a newer CLI")
	}
}

// The CLI is installed only from the helper's own verification: a release
// whose archive does not match its signed sums leaves the old CLI and its
// .prev exactly as they were.
func TestSelfUpdateRefusesAnUnverifiedRelease(t *testing.T) {
	e := newEnv(t, "v2.6.1")
	assets, pub := updatetest.Assets(t, "v2.6.1", updatetest.Opts{Tamper: true})
	e.h.Source, _ = updatetest.Serve(t, "v2.6.1", assets)
	e.h.Keys = []ed25519.PublicKey{pub}
	os.WriteFile(e.cli+".prev", []byte("older"), 0o755)
	e.state(`{"current":"v2.6.1","last_good":"v2.6.1"}`)
	err := e.h.Sync(context.Background())
	if !errors.Is(err, update.ErrUnverified) {
		t.Errorf("err %v", err)
	}
	if b, _ := os.ReadFile(e.cli); string(b) != hostCLI {
		t.Errorf("CLI %q", b)
	}
	if b, _ := os.ReadFile(e.cli + ".prev"); string(b) != "older" {
		t.Errorf(".prev %q", b)
	}
}

// Only the path install.json records is ever written: without one, nothing.
func TestSelfUpdateNeedsARecordedCLI(t *testing.T) {
	e := newEnv(t, "v2.6.1")
	m, _ := install.ReadManifest(e.h.Manifest)
	m.Binary = ""
	b, _ := json.Marshal(m)
	os.WriteFile(e.h.Manifest, b, 0o644)
	e.state(`{"current":"v2.6.1","last_good":"v2.6.1"}`)
	if err := e.h.Sync(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.logs)
	}
	if b, _ := os.ReadFile(e.cli); string(b) != hostCLI {
		t.Errorf("CLI %q", b)
	}
	if entries, _ := os.ReadDir(filepath.Dir(e.cli)); len(entries) != 1 {
		t.Errorf("wrote %v", entries)
	}
}

func TestNotAManagedInstall(t *testing.T) {
	e := newEnv(t, "v2.6.0")
	m := install.Manifest{Version: 1, OS: "linux", Compose: &install.Compose{File: "/x", Project: "viiwork"}}
	b, _ := json.Marshal(m)
	os.WriteFile(e.h.Manifest, b, 0o644)
	e.state(`{"current":"v2.6.1","last_good":"builtin"}`)
	if err := e.h.Sync(context.Background()); err == nil || e.d.called("docker") {
		t.Errorf("err %v", err)
	}
}

func (d *docker) count(prefix string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, c := range d.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// request writes the node's image request for version.
func (e *env) request(version string, n int) {
	id := fmt.Sprintf("%032x", n)
	os.WriteFile(filepath.Join(e.h.StateDir, "releases", update.ImageRequestFile), []byte(`{"version":"`+version+`","id":"`+id+`"}`), 0o644)
}

func (e *env) result() update.ImageResult {
	var res update.ImageResult
	b, _ := os.ReadFile(filepath.Join(e.h.StateDir, "releases", update.ImageResultFile))
	json.Unmarshal(b, &res)
	return res
}

// A node's state cannot choose a downgrade: the helper goes back only to a
// release it has itself run on this host, or to the install's own tag.
func TestRefusesADowngradeItHasNotRun(t *testing.T) {
	e := newEnv(t, "v2.6.1")
	e.state(`{"current":"v2.6.0","last_good":"v2.6.0"}`)
	err := e.h.Sync(context.Background())
	if err == nil || !strings.Contains(err.Error(), "older") {
		t.Errorf("err %v", err)
	}
	if e.d.called("docker") || !strings.Contains(e.compose(), repo+":v2.6.1\n") {
		t.Errorf("acted: %q", e.d.calls)
	}
	// Nor is it pulled for a stage.
	e.state(`{"current":"builtin","last_good":"builtin"}`)
	e.request("v2.6.0", 1)
	e.h.Sync(context.Background())
	if res := e.result(); res.OK || !strings.Contains(res.Error, "older") || e.d.called("docker pull") {
		t.Errorf("request: %+v, calls %q", res, e.d.calls)
	}
}

func TestRollsBackToAVersionItRan(t *testing.T) {
	e := newEnv(t, "v2.6.0")
	for _, v := range []string{"v2.6.1", "v2.6.2", "v2.6.1", "v2.6.0"} {
		e.state(`{"current":"` + v + `","last_good":"` + v + `"}`)
		if err := e.h.Sync(context.Background()); err != nil {
			t.Fatalf("to %s: %v\n%s", v, err, e.logs)
		}
		if !strings.Contains(e.compose(), repo+":"+v+"\n") {
			t.Fatalf("not on %s:\n%s", v, e.compose())
		}
	}
	if rec := readRecord(t, e); !slices.Equal(rec.History, []string{"v2.6.2", "v2.6.1", "v2.6.0"}) {
		t.Errorf("history %q", rec.History)
	}
}

func TestHistoryIsBounded(t *testing.T) {
	var r record
	for i := range historyMax + 5 {
		r.addHistory(fmt.Sprintf("v2.6.%d", i))
	}
	r.addHistory("v2.6.7")
	if len(r.History) != historyMax || r.History[len(r.History)-1] != "v2.6.7" || slices.Index(r.History, "v2.6.7") != historyMax-1 {
		t.Errorf("history %q", r.History)
	}
}

// Every image the helper pulls is removed once nothing needs it: never one
// it did not pull, never the running one, the original or the last two it
// ran, and at most one prepared for a stage.
func TestRemovesImagesItPulled(t *testing.T) {
	e := newEnv(t, "v2.6.0")
	e.request("v2.6.1", 1)
	if err := e.h.Sync(context.Background()); err != nil || !e.result().OK {
		t.Fatalf("%v %+v\n%s", err, e.result(), e.logs)
	}
	// A second stage wins: the first prepared image goes.
	e.request("v2.6.2", 2)
	e.h.Sync(context.Background())
	if !e.d.called("docker image rm "+repo+":v2.6.1") || !e.d.local[repo+":v2.6.2"] {
		t.Errorf("calls %q", e.d.calls)
	}
	// Activated: already local, so not pulled again.
	e.d.calls = nil
	e.state(`{"current":"v2.6.2","last_good":"v2.6.0","pending":{"version":"v2.6.2","attempts":0,"baseline":[]}}`)
	if err := e.h.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.d.called("docker pull") || !strings.Contains(e.compose(), ":v2.6.2\n") {
		t.Errorf("pulled a local image: %q", e.d.calls)
	}
	// The next release: v2.6.2 stays as one of the last two the host ran.
	e.state(`{"current":"v2.6.3","last_good":"v2.6.3"}`)
	e.h.Sync(context.Background())
	if !e.d.local[repo+":v2.6.2"] || !e.d.local[repo+":v2.6.3"] || !e.d.local[repo+":v2.6.0"] {
		t.Errorf("removed a kept image: %v", e.d.local)
	}
	// The original was not pulled by the helper: never removed, even once
	// it is out of the keep set's history.
	if e.d.called("docker image rm " + repo + ":v2.6.0") {
		t.Error("removed an image the helper did not pull")
	}
	if rec := readRecord(t, e); slices.Contains(rec.Pulled, repo+":v2.6.1") {
		t.Errorf("record still lists a removed image: %+v", rec)
	}
}

// A failed removal only logs, and is tried again next run.
func TestImageRemovalFailureOnlyLogs(t *testing.T) {
	e := newEnv(t, "v2.6.0")
	e.d.fail["docker image rm"] = true
	e.request("v2.6.1", 1)
	e.h.Sync(context.Background())
	e.request("v2.6.2", 2)
	if err := e.h.Sync(context.Background()); err != nil {
		t.Errorf("err %v", err)
	}
	if !strings.Contains(e.logs.String(), "removing") || !slices.Contains(readRecord(t, e).Pulled, repo+":v2.6.1") {
		t.Errorf("logs:\n%s\nrecord %+v", e.logs, readRecord(t, e))
	}
}

// With GitHub down the helper verifies the release from the host's
// viiwork-parrot, as the node does, and still swaps.
func TestSwapsFromParrotWhileGitHubIsDown(t *testing.T) {
	e := newEnv(t, "v2.6.0")
	dir := t.TempDir()
	name := release.ArchiveName("v2.6.1", e.h.Target)
	for _, asset := range []string{"SHA256SUMS", "SHA256SUMS.sig", name} {
		resp, err := http.Get(e.source + "/v2.6.1/" + asset)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		os.WriteFile(filepath.Join(dir, asset), b, 0o644)
	}
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	e.h.Source = down.URL
	e.h.Local = func(context.Context, string) (string, error) { return dir, nil }
	e.state(`{"current":"v2.6.1","last_good":"builtin","pending":{"version":"v2.6.1","attempts":0,"baseline":[]}}`)
	if err := e.h.Sync(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.logs)
	}
	if !strings.Contains(e.compose(), "    image: "+repo+":v2.6.1\n") {
		t.Errorf("compose:\n%s", e.compose())
	}
	if !strings.Contains(e.logs.String(), "GitHub unreachable") {
		t.Errorf("logs:\n%s", e.logs)
	}
}
