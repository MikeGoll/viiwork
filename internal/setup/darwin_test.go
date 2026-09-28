package setup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/joincode"
	"github.com/janit/viiwork/v2/internal/setup/install"
	"github.com/janit/viiwork/v2/internal/setup/prompt"
)

const home = "/Users/u"

// macFake is newFake turned into a 36 GiB Apple Silicon Mac: sysctl answers,
// no nvidia-smi, the models in ~/models.
func macFake(t *testing.T) *fake {
	f := newFake(t)
	f.host.GOOS, f.host.GOARCH, f.host.Home, f.host.Euid, f.host.UID = "darwin", "arm64", home, 501, 501
	f.host.LlamaPin = "b1"
	run := f.host.Run
	f.host.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch name + " " + strings.Join(args, " ") {
		case "sysctl -n hw.memsize":
			return []byte("38654705664\n"), nil // 36 GiB
		case "sysctl -n iogpu.wired_limit_mb":
			return []byte("0\n"), nil
		case "sysctl -n machdep.cpu.brand_string":
			return []byte("Apple M3 Max\n"), nil
		}
		if name == "nvidia-smi" {
			return nil, errors.New("not found")
		}
		return run(ctx, name, args...)
	}
	return f
}

func TestMacHardwareAndLayout(t *testing.T) {
	f := macFake(t)
	models := filepath.Join(f.host.Root, home, "models")
	writeGGUF(t, filepath.Join(models, "huge.gguf"), "llama", 28<<30)
	writeGGUF(t, filepath.Join(models, "mid.gguf"), "llama", 19<<30)
	writeGGUF(t, filepath.Join(models, "small.gguf"), "llama", 3<<30)
	p := prompt.Script(f.out, "", "all", "", "", "", "")
	s := &state{}
	for _, step := range []func(context.Context, Host, prompt.Prompter, *state) error{hardware, chooseModels, layout} {
		if err := step(context.Background(), f.host, p, s); err != nil {
			t.Fatalf("%v\n%s", err, f.out)
		}
	}
	if s.vendor != "apple" || s.budget != 27648 || s.image != "" || s.modelsDir != home+"/models" {
		t.Errorf("state vendor %q budget %d image %q dir %q", s.vendor, s.budget, s.image, s.modelsDir)
	}
	if len(s.placed) != 1 || s.placed[0].Name != "mid" || !slices.Equal(s.placed[0].GPUs, []int{0}) {
		t.Errorf("placed %+v", s.placed)
	}
	if !strings.Contains(f.out.String(), "Not placed: huge") || !strings.Contains(f.out.String(), "shared") {
		t.Errorf("output:\n%s", f.out)
	}
	a := s.answers()
	if a.Vendor != "apple" || a.StateDir != home+"/.local/state/viiwork" || len(a.Models) != 1 ||
		a.Models[0].Path != home+"/models/mid.gguf" || a.Models[0].Binary != home+"/.local/share/viiwork/llama.cpp/b1/llama-b1/llama-server" {
		t.Errorf("answers %+v", a)
	}
}

// Editing on a Mac asks only for context and slots: there is one GPU.
func TestMacEditSkipsTheGPUQuestion(t *testing.T) {
	f := macFake(t)
	writeGGUF(t, filepath.Join(f.host.Root, home, "models", "small.gguf"), "llama", 3<<30)
	p := prompt.Script(f.out, "", "all", "", "2", "1", "8192", "1", "")
	s := &state{}
	for _, step := range []func(context.Context, Host, prompt.Prompter, *state) error{hardware, chooseModels, layout} {
		if err := step(context.Background(), f.host, p, s); err != nil {
			t.Fatalf("%v\n%s", err, f.out)
		}
	}
	if s.placed[0].Context != 8192 || s.placed[0].Parallel != 1 || strings.Contains(f.out.String(), "GPUs (card numbers)") {
		t.Errorf("placed %+v\n%s", s.placed[0], f.out)
	}
}

// llamaRelease stands in for GitHub: the b1 release's asset list and a
// tarball holding llama-b1/llama-server.
func llamaRelease(t *testing.T) (*httptest.Server, *atomic.Int32) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "llama-b1/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: "llama-b1/llama-server", Typeflag: tar.TypeReg, Mode: 0o755, Size: 8})
	tw.Write([]byte("#!server"))
	tw.Close()
	gz.Close()
	archive := buf.Bytes()
	sum := sha256.Sum256(archive)
	var hits atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/tags/b1":
			json.NewEncoder(w).Encode(map[string]any{"assets": []any{map[string]string{
				"name": "llama-b1-bin-macos-arm64.tar.gz", "browser_download_url": srv.URL + "/asset",
				"digest": "sha256:" + hex.EncodeToString(sum[:])}}})
		case "/asset":
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

type macInstall struct {
	*fake
	home     string
	p        install.MacPaths
	hits     *atomic.Int32
	loaded   bool
	headless bool // no GUI session: an SSH login with nobody at the console
}

// newMac is a Mac with a real temporary home (no root prefix: everything a
// Mac install writes is under the home), one 7 GiB model in ~/models, and a
// release server for llama.cpp b1.
func newMac(t *testing.T) *macInstall {
	f := macFake(t)
	m := &macInstall{fake: f, home: t.TempDir()}
	m.p = install.MacLayout(m.home)
	f.host.Root, f.host.Home = "", m.home
	writeGGUF(t, filepath.Join(m.home, "models", "alpha-8b.gguf"), "llama", 7<<30)
	srv, hits := llamaRelease(t)
	m.hits = hits
	f.host.LlamaAPI, f.host.Download = srv.URL+"/tags/", srv.Client()
	f.host.Exec = func(_ context.Context, _ io.Writer, name string, args ...string) error {
		c := name + " " + strings.Join(args, " ")
		if strings.HasPrefix(c, "launchctl print gui/501/") {
			if m.loaded {
				return nil
			}
			return errors.New("could not find service")
		}
		if c == "launchctl print gui/501" {
			if m.headless {
				return errors.New("Domain does not support specified action")
			}
			return nil
		}
		*f.calls = append(*f.calls, c)
		return nil
	}
	return m
}

func (m *macInstall) run(answers ...string) error {
	return Run(context.Background(), m.host, prompt.Script(m.out, answers...), m.p.ConfigFile)
}

var macAnswers = []string{
	"",      // models directory: ~/models
	"all",   // the one model
	"alpha", // its name
	"",      // accept the layout
	"",      // a new secured mesh
	"y",     // updates
	"",      // node name: node-a
	"",      // write and start
}

func TestMacNewSecuredMesh(t *testing.T) {
	m := newMac(t)
	if err := m.run(macAnswers...); err != nil {
		t.Fatalf("%v\n%s", err, m.out)
	}
	plist := string(mustRead(t, m.p.Plist))
	fi, _ := os.Stat(m.p.Plist)
	if fi.Mode().Perm() != 0o600 || !strings.Contains(plist, "VIIWORK_MESH_SECRET") || !strings.Contains(plist, m.p.BinaryPath) {
		t.Errorf("plist %v:\n%s", fi.Mode(), plist)
	}
	secret := plist[strings.Index(plist, "<key>VIIWORK_MESH_SECRET</key>"):]
	secret = strings.TrimSpace(secret[strings.Index(secret, "<string>")+8 : strings.Index(secret, "</string>")])
	cfg, err := config.Load(m.p.ConfigFile, func(k string) (string, bool) { return secret, k == "VIIWORK_MESH_SECRET" })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GPU.Vendor != "apple" || cfg.Node.StateDir != m.p.StateDir || cfg.Models[0].Path != filepath.Join(m.home, "models", "alpha-8b.gguf") {
		t.Errorf("config %+v", cfg)
	}
	server := install.LlamaServerPath(m.p.LlamaRoot, "b1")
	if b, err := os.ReadFile(server); err != nil || string(b) != "#!server" {
		t.Errorf("llama-server %q %v", b, err)
	}
	if !strings.Contains(string(mustRead(t, m.p.ConfigFile)), server) {
		t.Error("the config does not name the fetched llama-server")
	}
	man, err := install.ReadManifest(m.p.ManifestFile)
	if err != nil {
		t.Fatal(err)
	}
	if man.OS != "darwin" || man.LaunchAgent != install.LaunchAgent || man.Binary != m.p.BinaryPath ||
		!slices.Equal(man.Files, []string{m.p.ConfigFile, m.p.Plist}) || !slices.Contains(man.Dirs, filepath.Join(m.p.LlamaRoot, "b1")) {
		t.Errorf("manifest %+v", man)
	}
	want := []string{"xattr -dr com.apple.quarantine", "launchctl bootstrap gui/501 " + m.p.Plist}
	if len(*m.calls) != 2 || !strings.HasPrefix((*m.calls)[0], want[0]) || (*m.calls)[1] != want[1] {
		t.Errorf("calls %q", *m.calls)
	}
	out := m.out.String()
	for _, w := range []string{"caffeinate", "viiwork1-", "kickstart -k gui/501/fi.viiwork.node"} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	if b64 := secret; strings.Contains(out, b64) {
		t.Error("the secret was printed")
	}
	_ = base64.StdEncoding
}

func TestMacAbortWritesNothing(t *testing.T) {
	for k := 0; k < len(macAnswers); k++ {
		m := newMac(t)
		answers := slices.Clone(macAnswers[:k])
		if k == len(macAnswers)-1 {
			answers = append(answers, "n")
		}
		if err := m.run(answers...); !errors.Is(err, prompt.ErrAbort) {
			t.Fatalf("cut at %d: %v", k, err)
		}
		for _, p := range []string{".config", "Library", ".local"} {
			if _, err := os.Stat(filepath.Join(m.home, p)); err == nil {
				t.Errorf("cut at %d: ~/%s was written", k, p)
			}
		}
		if len(*m.calls) != 0 || m.hits.Load() != 0 {
			t.Errorf("cut at %d: ran %q, %d downloads", k, *m.calls, m.hits.Load())
		}
	}
}

func TestMacPreflight(t *testing.T) {
	cases := map[string]func(m *macInstall){
		"without sudo":  func(m *macInstall) { m.host.Euid = 0 },
		"pin":           func(m *macInstall) { m.host.LlamaPin = "" },
		"Apple Silicon": func(m *macInstall) { m.host.GOARCH = "amd64" },
		"already exists": func(m *macInstall) {
			os.MkdirAll(m.p.ConfigDir, 0o755)
			os.WriteFile(m.p.ConfigFile, nil, 0o644)
		},
		"fi.viiwork.node.plist": func(m *macInstall) {
			os.MkdirAll(filepath.Dir(m.p.Plist), 0o755)
			os.WriteFile(m.p.Plist, nil, 0o600)
		},
		"loaded":      func(m *macInstall) { m.loaded = true },
		"GUI session": func(m *macInstall) { m.headless = true },
	}
	for want, breakIt := range cases {
		m := newMac(t)
		breakIt(m)
		if err := m.run(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
}

func TestMacReusesAHandFetchedBuild(t *testing.T) {
	m := newMac(t)
	server := install.LlamaServerPath(m.p.LlamaRoot, "b1")
	os.MkdirAll(filepath.Dir(server), 0o755)
	os.WriteFile(server, []byte("#!mine"), 0o755)
	if err := m.run(macAnswers...); err != nil {
		t.Fatalf("%v\n%s", err, m.out)
	}
	man, _ := install.ReadManifest(m.p.ManifestFile)
	if m.hits.Load() != 0 || slices.Contains(man.Dirs, filepath.Join(m.p.LlamaRoot, "b1")) {
		t.Errorf("%d downloads, dirs %q", m.hits.Load(), man.Dirs)
	}
	if b, _ := os.ReadFile(server); string(b) != "#!mine" {
		t.Error("the hand-fetched build was replaced")
	}
	if !strings.Contains(m.out.String(), "reused as found") || strings.Contains(m.out.String(), "checked against") {
		t.Errorf("the preview claims a check that will not happen:\n%s", m.out)
	}
}

func TestDefaultConfigPath(t *testing.T) {
	if got := DefaultConfigPath("darwin", "/Users/u"); got != "/Users/u/.config/viiwork/viiwork.yaml" {
		t.Errorf("darwin: %s", got)
	}
	if got := DefaultConfigPath("linux", "/root"); got != "/etc/viiwork/viiwork.yaml" {
		t.Errorf("linux: %s", got)
	}
}

// A failed download never started anything: the advice is to undo and rerun,
// not to restart an agent that was never loaded.
func TestMacFetchFailureSaysNothingStarted(t *testing.T) {
	m := newMac(t)
	m.host.LlamaAPI += "missing/"
	if err := m.run(macAnswers...); err == nil {
		t.Fatal("setup reported success")
	}
	out := m.out.String()
	out = out[max(0, strings.Index(out, "Setup stopped")):] // after the preview, which names kickstart in the plist
	if !strings.Contains(out, "Nothing was started") || !strings.Contains(out, "viiwork uninstall") || strings.Contains(out, "kickstart") {
		t.Errorf("output:\n%s", out)
	}
	for _, c := range *m.calls {
		if strings.Contains(c, "bootstrap") {
			t.Errorf("ran %q", c)
		}
	}
	// launchd would load a plist left behind at the next login, and start a
	// node with no llama-server that still joins the mesh with the secret.
	if _, err := os.Stat(m.p.Plist); err == nil {
		t.Error("the LaunchAgent was left in place after a failed fetch")
	}
}

// The plist the installer writes is one join-code can read the secret from.
func TestJoinCodeReadsTheInstalledPlist(t *testing.T) {
	m := newMac(t)
	if err := m.run(macAnswers...); err != nil {
		t.Fatalf("%v\n%s", err, m.out)
	}
	var out, errs bytes.Buffer
	code := joincode.Run(context.Background(), []string{"--node", m.host.NodeAPI}, joincode.Env{
		Stdout: &out, Stderr: &errs, Client: m.host.HTTP, ReadFile: os.ReadFile, Plist: m.p.Plist,
		LookupEnv: func(string) (string, bool) { return "", false },
	})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	c, err := joincode.Decode(strings.TrimSpace(out.String()))
	if err != nil || len(c.Secret) != 32 {
		t.Errorf("%+v, %v", c, err)
	}
}

// Found on ruutana: its models were viiwork-parrot's, so no suggested
// directory existed, an empty answer got a message about path syntax, and
// "~/…" was refused. Parrot's data_dir is suggested now, an empty answer is
// asked for plainly, and ~ is the home directory.
func TestModelsPromptOnAParrotMac(t *testing.T) {
	m := newMac(t)
	// The model lives only in parrot's store, as on ruutana.
	os.RemoveAll(filepath.Join(m.home, "models"))
	store := filepath.Join(m.home, ".local", "share", "viiwork-parrot", "models")
	writeGGUF(t, filepath.Join(store, "alpha-8b.gguf"), "llama", 7<<30)
	os.MkdirAll(filepath.Join(m.home, ".config", "viiwork-parrot"), 0o755)
	os.WriteFile(filepath.Join(m.home, ".config", "viiwork-parrot", "viiwork-parrot.yaml"),
		[]byte("node:\n  data_dir: "+store+"\n  state_dir: /elsewhere\n"), 0o644)

	p := prompt.Script(m.out, "")
	s := &state{vendor: "apple"}
	if err := chooseModels(context.Background(), m.host, p, s); !errors.Is(err, prompt.ErrAbort) {
		t.Fatalf("err %v", err)
	}
	if !strings.Contains(m.out.String(), "Models directory ["+store+"]") {
		t.Errorf("parrot's directory was not suggested:\n%s", m.out)
	}

	// With nothing to suggest, an empty answer is asked for, not refused as a
	// malformed path; then "~/…" is expanded.
	os.Remove(filepath.Join(m.home, ".config", "viiwork-parrot", "viiwork-parrot.yaml"))
	m.out.Reset()
	s = &state{vendor: "apple"}
	p = prompt.Script(m.out, "", "~/.local/share/viiwork-parrot/models", "all", "")
	if err := chooseModels(context.Background(), m.host, p, s); err != nil {
		t.Fatalf("%v\n%s", err, m.out)
	}
	out := m.out.String()
	if !strings.Contains(out, "Enter the directory that holds your GGUF model files") || strings.Contains(out, "without ':'") {
		t.Errorf("empty answer:\n%s", out)
	}
	if s.modelsDir != store || len(s.models) != 1 {
		t.Errorf("dir %q models %d", s.modelsDir, len(s.models))
	}
}

// ~/.local/bin is not on a Mac's default PATH, so the commands the finish
// screen names would be "command not found": the wizard says how to add it.
func TestMacFinishSaysHowToReachTheBinary(t *testing.T) {
	m := newMac(t)
	m.host.Path = "/usr/bin:/bin"
	if err := m.run(macAnswers...); err != nil {
		t.Fatalf("%v\n%s", err, m.out)
	}
	if !strings.Contains(m.out.String(), `export PATH="$HOME/.local/bin:$PATH"`) {
		t.Errorf("no PATH hint:\n%s", m.out)
	}
	m = newMac(t)
	m.host.Path = "/usr/bin:" + filepath.Join(m.home, ".local", "bin")
	if err := m.run(macAnswers...); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(m.out.String(), "export PATH=") {
		t.Error("a PATH hint although ~/.local/bin is on PATH")
	}
}
