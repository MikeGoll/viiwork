package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/janit/viiwork/v2/internal/release"
)

type releaseOpts struct {
	reports  string // what the fake binary prints for --version; "" = the version
	requires string // JSON printed for --engine-requirements; "" = {}
	shebang  string // "" = #!/bin/sh
	otherKey bool   // sign with a key the node does not trust
	tamper   bool   // change the archive after SHA256SUMS is written
	unsigned bool   // no SHA256SUMS.sig
	info     string // JSON printed for --build-info; "" = an older binary without the flag
}

// fakeRelease serves a signed release of a shell-script "viiwork" for this
// host's target, at <source>/<version>/<asset>.
func fakeRelease(t *testing.T, version string, o releaseOpts) (string, ed25519.PublicKey) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := priv
	if o.otherKey {
		_, signer, _ = ed25519.GenerateKey(rand.Reader)
	}
	reports, requires, shebang := o.reports, o.requires, o.shebang
	if reports == "" {
		reports = version
	}
	if requires == "" {
		requires = "{}"
	}
	if shebang == "" {
		shebang = "#!/bin/sh"
	}
	info := "exit 2"
	if o.info != "" {
		info = "echo '" + o.info + "'"
	}
	script := fmt.Sprintf("%s\ncase \"$1\" in\n--version) echo %s ;;\n--engine-requirements) echo '%s' ;;\n--build-info) %s ;;\nesac\n", shebang, reports, requires, info)
	target := release.Target{OS: runtime.GOOS, Arch: runtime.GOARCH}
	name := release.ArchiveName(version, target)
	top := strings.TrimSuffix(name, ".tar.gz")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: top + "/", Typeflag: tar.TypeDir, Mode: 0o755})
	for n, body := range map[string]string{"viiwork": script, "viiwork-accept": script, "LICENSE": "MIT"} {
		tw.WriteHeader(&tar.Header{Name: top + "/" + n, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	archive := buf.Bytes()
	var sums strings.Builder
	for _, tg := range release.Targets {
		n := release.ArchiveName(version, tg)
		sum := sha256.Sum256([]byte(n)) // other targets: any digest will do
		if tg == target {
			sum = sha256.Sum256(archive)
		}
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), n)
	}
	if o.tamper {
		archive = append([]byte{}, archive...)
		archive[len(archive)/2] ^= 0xff
	}
	assets := map[string][]byte{name: archive, "SHA256SUMS": []byte(sums.String())}
	if !o.unsigned {
		assets["SHA256SUMS.sig"] = release.Sign(signer, version, []byte(sums.String()))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := assets[strings.TrimPrefix(r.URL.Path, "/"+version+"/")]
		if !ok || !strings.HasPrefix(r.URL.Path, "/"+version+"/") {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, pub
}

func stager(dir, source string, pub ed25519.PublicKey) *Stager {
	return &Stager{Dir: dir, Source: source, Client: &http.Client{}, Keys: []ed25519.PublicKey{pub},
		Target: release.Target{OS: runtime.GOOS, Arch: runtime.GOARCH}}
}

func TestStage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src, pub := fakeRelease(t, "v2.6.0", releaseOpts{requires: `{"llamacpp":"b100"}`})
	st := stager(dir, src, pub)
	var asked map[string]string
	st.Engines = func(_ context.Context, req map[string]string) error { asked = req; return nil }
	if err := st.Stage(ctx, "v2.6.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := Binary(dir, "v2.6.0"); err != nil {
		t.Fatalf("staged binary: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "v2.6.0", "viiwork-accept")); err != nil {
		t.Errorf("viiwork-accept not staged: %v", err)
	}
	if asked["llamacpp"] != "b100" {
		t.Errorf("engine requirements passed on: %v", asked)
	}
	// Staging the same signed bytes again touches nothing: a rollout retry
	// must never open a window with no release directory.
	before, _ := os.Stat(filepath.Join(dir, "v2.6.0"))
	if err := st.Stage(ctx, "v2.6.0"); err != nil {
		t.Fatalf("restage: %v", err)
	}
	after, _ := os.Stat(filepath.Join(dir, "v2.6.0"))
	if !os.SameFile(before, after) {
		t.Error("restaging identical bytes replaced the directory")
	}
	if got := Staged(dir); len(got) != 1 {
		t.Errorf("restage left %v", got)
	}
	// A damaged staged copy is replaced, and nothing is left beside it.
	os.WriteFile(filepath.Join(dir, "v2.6.0", "viiwork"), []byte("damaged"), 0o755)
	if err := st.Stage(ctx, "v2.6.0"); err != nil {
		t.Fatalf("restage over a damaged copy: %v", err)
	}
	if _, err := Binary(dir, "v2.6.0"); err != nil {
		t.Errorf("after restage: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("left behind: %v", entries)
	}
}

func TestStageRefuses(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		opts releaseOpts
		want error
		text string
	}{
		"wrong key":        {releaseOpts{otherKey: true}, ErrUnverified, "signature"},
		"unsigned":         {releaseOpts{unsigned: true}, nil, "SHA256SUMS.sig"},
		"tampered archive": {releaseOpts{tamper: true}, ErrUnverified, "sha256"},
		"wrong --version":  {releaseOpts{reports: "v9.9.9"}, ErrUnverified, "v9.9.9"},
		"cannot run":       {releaseOpts{shebang: "#!/nonexistent/interpreter"}, nil, "cannot run"},
	}
	for name, c := range cases {
		dir := t.TempDir()
		src, pub := fakeRelease(t, "v2.6.0", c.opts)
		err := stager(dir, src, pub).Stage(ctx, "v2.6.0")
		if err == nil || c.want != nil && !errors.Is(err, c.want) || !strings.Contains(err.Error(), c.text) {
			t.Errorf("%s: %v", name, err)
		}
		if got := Staged(dir); len(got) != 0 {
			t.Errorf("%s: left %v staged", name, got)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("%s: left %v behind", name, entries)
		}
	}

	dir := t.TempDir()
	src, pub := fakeRelease(t, "v2.6.0", releaseOpts{requires: `{"llamacpp":"b999999"}`})
	st := stager(dir, src, pub)
	st.Engines = func(context.Context, map[string]string) error {
		return fmt.Errorf("%w: needs b999999", ErrEngineTooOld)
	}
	if err := st.Stage(ctx, "v2.6.0"); !errors.Is(err, ErrEngineTooOld) || len(Staged(dir)) != 0 {
		t.Errorf("engine too old: %v, staged %v", err, Staged(dir))
	}

	if err := stager(t.TempDir(), src, pub).Stage(ctx, "../../etc"); !errors.Is(err, ErrUnverified) {
		t.Errorf("a path as version: %v", err)
	}
	other := stager(t.TempDir(), src, pub)
	other.Target = release.Target{OS: "plan9", Arch: "mips"}
	if err := other.Stage(ctx, "v2.6.0"); err == nil {
		t.Error("staged for a target no release is built for")
	}
	if err := stager(t.TempDir(), src, pub).Stage(ctx, "v2.7.0"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("missing release: %v", err)
	}
}

// PrepareLlama sees the staged binary's pin after it proved it runs and
// before the engine check, and the check it returns judges the requirement
// against the build the release will run, in place of Engines.
func TestStagePreparesTheReleasesLlama(t *testing.T) {
	src, pub := fakeRelease(t, "v2.6.1", releaseOpts{requires: `{"llamacpp":"b200"}`,
		info: `{"version":"v2.6.1","llama_cpp":"b200","llama_cpp_macos_sha256":"abc"}`})
	st := stager(t.TempDir(), src, pub)
	var order []string
	var got BuildInfo
	st.Engines = func(context.Context, map[string]string) error { order = append(order, "engines"); return nil }
	st.PrepareLlama = func(_ context.Context, info BuildInfo) (func(context.Context, map[string]string) error, error) {
		order, got = append(order, "prepare"), info
		return func(_ context.Context, req map[string]string) error {
			order = append(order, "check "+req["llamacpp"])
			return nil
		}, nil
	}
	if err := st.Stage(context.Background(), "v2.6.1"); err != nil {
		t.Fatal(err)
	}
	if got != (BuildInfo{Version: "v2.6.1", LlamaCpp: "b200", LlamaCppMacSHA256: "abc"}) {
		t.Errorf("build info %+v", got)
	}
	if strings.Join(order, ",") != "prepare,check b200" {
		t.Errorf("order %q", order)
	}
}

func TestStageFailsWhenTheLlamaCannotBePrepared(t *testing.T) {
	src, pub := fakeRelease(t, "v2.6.1", releaseOpts{info: `{"version":"v2.6.1","llama_cpp":"b200"}`})
	dir := t.TempDir()
	st := stager(dir, src, pub)
	st.PrepareLlama = func(context.Context, BuildInfo) (func(context.Context, map[string]string) error, error) {
		return nil, errors.New("download failed")
	}
	if err := st.Stage(context.Background(), "v2.6.1"); err == nil || !strings.Contains(err.Error(), "download failed") {
		t.Fatalf("err %v", err)
	}
	if staged := Staged(dir); len(staged) != 0 {
		t.Errorf("staged %q", staged)
	}
}

// A release built before --build-info carries no pin: nothing to prepare,
// and Engines checks the engine as configured, which is what it will run.
func TestStageWithoutBuildInfoPassesNoPin(t *testing.T) {
	src, pub := fakeRelease(t, "v2.6.1", releaseOpts{})
	st := stager(t.TempDir(), src, pub)
	engines := false
	st.Engines = func(context.Context, map[string]string) error { engines = true; return nil }
	var got *BuildInfo
	st.PrepareLlama = func(_ context.Context, info BuildInfo) (func(context.Context, map[string]string) error, error) {
		got = &info
		return nil, nil
	}
	if err := st.Stage(context.Background(), "v2.6.1"); err != nil {
		t.Fatal(err)
	}
	if got == nil || got.LlamaCpp != "" || !engines {
		t.Errorf("info %+v, engines checked %v", got, engines)
	}
}
