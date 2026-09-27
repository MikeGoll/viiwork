package uninstall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/janit/viiwork/v2/internal/setup/install"
	"github.com/janit/viiwork/v2/internal/setup/prompt"
	"github.com/janit/viiwork/v2/meshapi"
)

const img = "ghcr.io/janit/viiwork-llamacpp-cuda:v2.6.0"

type fake struct {
	h     Host
	out   *bytes.Buffer
	calls *[]string
	fail  map[string]bool // commands that fail, by their joined text
}

func (f *fake) path(p string) string { return filepath.Join(f.h.Root, p) }

func (f *fake) write(p, data string) {
	os.MkdirAll(filepath.Dir(f.path(p)), 0o755)
	os.WriteFile(f.path(p), []byte(data), 0o644)
}

// alive is how many members the node's /v1/cluster reports alive.
func newLinux(t *testing.T, alive int) *fake {
	f := &fake{out: &bytes.Buffer{}, calls: &[]string{}, fail: map[string]bool{}}
	var members []meshapi.Member
	for i := 0; i < alive; i++ {
		members = append(members, meshapi.Member{Node: "node-" + string(rune('a'+i)), State: meshapi.MemberAlive})
	}
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(meshapi.ClusterResponse{Members: members})
	}))
	t.Cleanup(node.Close)
	f.h = Host{GOOS: "linux", Root: t.TempDir(), Euid: 0, HTTP: node.Client(),
		NodeAPI: strings.TrimPrefix(node.URL, "http://"), Out: f.out,
		Exec: func(_ context.Context, _ io.Writer, name string, args ...string) error {
			c := name + " " + strings.Join(args, " ")
			*f.calls = append(*f.calls, c)
			if f.fail[c] {
				return errors.New("exit 1")
			}
			return nil
		}}
	f.write(install.ConfigFile, "node: {name: node-a}\nmesh: {open: true}\nmodels:\n"+
		"  - {name: alpha, path: /models/alpha.gguf, gpus: [0]}\n"+
		"  - {name: beta, path: /models/sub/beta-00001-of-00002.gguf, gpus: [1]}\n")
	f.write(install.EnvFile, "VIIWORK_MESH_SECRET=x\n")
	f.write(install.ComposeFile, "name: viiwork\n")
	f.write(install.BinaryPath, "#!binary")
	f.write(install.StateDir+"/releases/v2.6.0/viiwork", "old")
	f.write("/etc/viiwork/operator-notes.txt", "mine") // not the install's
	f.write("/models/alpha.gguf", "weights")
	f.write("/models/sub/beta-00001-of-00002.gguf", "w1")
	f.write("/models/sub/beta-00002-of-00002.gguf", "w2")
	f.write("/models/gamma.gguf", "unrelated")
	f.manifest(install.Manifest{})
	return f
}

// manifest writes install.json: the full Linux install, with edit applied.
func (f *fake) manifest(edit install.Manifest) {
	m := install.Manifest{
		Version: 1, OS: "linux", InstalledBy: "v2.6.0",
		Files:     []string{install.ConfigFile, install.EnvFile, install.ComposeFile},
		Dirs:      []string{install.ConfigDir, install.StateDir},
		Compose:   &install.Compose{File: install.ComposeFile, Project: "viiwork", Volumes: []string{}},
		Images:    []string{img},
		Binary:    install.BinaryPath,
		ModelDirs: []string{"/models"},
	}
	if edit.Files != nil {
		m.Files = append(m.Files, edit.Files...)
	}
	if edit.Images != nil {
		m.Images = append(m.Images, edit.Images...)
	}
	if edit.ModelDirs != nil {
		m.ModelDirs = edit.ModelDirs
	}
	data, _ := json.Marshal(m)
	f.write(install.ManifestFile, string(data))
}

func (f *fake) run(o Options, answers ...string) error {
	return Run(context.Background(), f.h, prompt.Script(f.out, answers...), o)
}

func (f *fake) exists(p string) bool { _, err := os.Stat(f.path(p)); return err == nil }

func TestRemovesExactlyTheManifest(t *testing.T) {
	f := newLinux(t, 2)
	if err := f.run(Options{}, "uninstall"); err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
	for _, p := range []string{install.ConfigFile, install.EnvFile, install.ComposeFile, install.ManifestFile, install.StateDir, install.BinaryPath} {
		if f.exists(p) {
			t.Errorf("%s survived", p)
		}
	}
	for _, p := range []string{"/etc/viiwork/operator-notes.txt", "/models/alpha.gguf", "/models/gamma.gguf"} {
		if !f.exists(p) {
			t.Errorf("%s was removed", p)
		}
	}
	compose := "docker compose -f " + f.path(install.ComposeFile) + " -p viiwork "
	want := []string{compose + "stop -t 90", compose + "down", "docker rmi " + img}
	if !slices.Equal(*f.calls, want) {
		t.Errorf("calls %q", *f.calls)
	}
	if !strings.Contains(f.out.String(), "replicated") {
		t.Errorf("no mesh note:\n%s", f.out)
	}
}

func TestConfigDirGoesWhenEmpty(t *testing.T) {
	f := newLinux(t, 2)
	os.Remove(f.path("/etc/viiwork/operator-notes.txt"))
	if err := f.run(Options{Yes: true}); err != nil {
		t.Fatal(err)
	}
	if f.exists(install.ConfigDir) {
		t.Error("an empty /etc/viiwork was left")
	}
}

func TestAnythingButTheWordRemovesNothing(t *testing.T) {
	for _, answers := range [][]string{{"yes"}, {}} {
		f := newLinux(t, 2)
		if err := f.run(Options{}, answers...); err == nil {
			t.Errorf("%q: no error", answers)
		}
		if !f.exists(install.ManifestFile) || !f.exists(install.BinaryPath) || len(*f.calls) != 0 {
			t.Errorf("%q: something was removed or run: %q", answers, *f.calls)
		}
	}
}

func TestKeepImagesAndImagesInUse(t *testing.T) {
	f := newLinux(t, 2)
	if err := f.run(Options{Yes: true, KeepImages: true}); err != nil {
		t.Fatal(err)
	}
	for _, c := range *f.calls {
		if strings.Contains(c, "rmi") {
			t.Errorf("--keep-images ran %q", c)
		}
	}
	f = newLinux(t, 2)
	f.fail["docker rmi "+img] = true
	if err := f.run(Options{Yes: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.out.String(), "kept "+img) || f.exists(install.BinaryPath) {
		t.Errorf("an image in use stopped the uninstall:\n%s", f.out)
	}
}

func TestDeleteModels(t *testing.T) {
	f := newLinux(t, 2)
	if err := f.run(Options{DeleteModels: true}, "uninstall", "delete models"); err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
	for _, p := range []string{"/models/alpha.gguf", "/models/sub/beta-00001-of-00002.gguf", "/models/sub/beta-00002-of-00002.gguf"} {
		if f.exists(p) {
			t.Errorf("%s survived --delete-models", p)
		}
	}
	if !f.exists("/models/gamma.gguf") {
		t.Error("a model the config did not serve was deleted")
	}
	// Without its own confirmation, the models stay but the rest goes.
	f = newLinux(t, 2)
	if err := f.run(Options{DeleteModels: true}, "uninstall", "no"); err == nil {
		t.Error("no error")
	}
	if !f.exists("/models/alpha.gguf") || !f.exists(install.BinaryPath) {
		t.Error("a refused model deletion still removed things")
	}
}

func TestTamperedManifestIsContained(t *testing.T) {
	f := newLinux(t, 2)
	f.write("/etc/passwd", "root")
	f.write("/home/x/file", "x")
	f.manifest(install.Manifest{
		Files:     []string{"/etc/passwd", "/home/x/file", "/etc/viiwork/../passwd"},
		Images:    []string{"postgres:16"},
		ModelDirs: []string{"/"},
	})
	f.write(install.ConfigFile, "node: {name: node-a}\nmodels:\n  - {name: a, path: /models/../etc/passwd}\n")
	if err := f.run(Options{Yes: true, DeleteModels: true}); err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
	for _, p := range []string{"/etc/passwd", "/home/x/file"} {
		if !f.exists(p) {
			t.Errorf("%s was removed", p)
		}
	}
	for _, c := range *f.calls {
		if strings.Contains(c, "postgres") {
			t.Errorf("ran %q", c)
		}
	}
	for _, want := range []string{"/etc/passwd", "postgres:16", "models directory /"} {
		if !strings.Contains(f.out.String(), "skipped "+want) {
			t.Errorf("not reported: %s\n%s", want, f.out)
		}
	}
}

func TestLastMemberWarning(t *testing.T) {
	f := newLinux(t, 1)
	if err := f.run(Options{Yes: true}); err != nil {
		t.Fatal(err)
	}
	if out := f.out.String(); !strings.Contains(out, "last member") || !strings.Contains(out, "mesh secret") {
		t.Errorf("output:\n%s", out)
	}
}

func TestNoManifest(t *testing.T) {
	f := newLinux(t, 2)
	os.Remove(f.path(install.ManifestFile))
	err := f.run(Options{Yes: true})
	if err == nil || !strings.Contains(err.Error(), "--from-config") || !f.exists(install.BinaryPath) {
		t.Errorf("err %v", err)
	}
	if err := f.run(Options{FromConfig: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.out.String(), "/models/alpha.gguf") || !strings.Contains(f.out.String(), "Nothing was removed") ||
		!f.exists(install.ConfigFile) || len(*f.calls) != 0 {
		t.Errorf("--from-config:\n%s\ncalls %q", f.out, *f.calls)
	}
}

func TestNeedsRoot(t *testing.T) {
	f := newLinux(t, 2)
	f.h.Euid = 1000
	if err := f.run(Options{Yes: true}); err == nil || !strings.Contains(err.Error(), "root") {
		t.Errorf("err %v", err)
	}
}

func TestMac(t *testing.T) {
	f := newLinux(t, 2)
	home := "/Users/u"
	f.h.GOOS, f.h.Home, f.h.Euid, f.h.UID = "darwin", home, 501, 501
	cfg := home + "/.config/viiwork/viiwork.yaml"
	plist := home + "/Library/LaunchAgents/fi.viiwork.node.plist"
	llama := home + "/.local/share/viiwork/llama.cpp/b10437"
	f.write(cfg, "node: {name: mac}\nmodels:\n  - {name: m, path: "+home+"/models/m.gguf}\n")
	f.write(plist, "<plist/>")
	f.write(llama+"/llama-server", "bin")
	f.write(home+"/.local/bin/viiwork", "#!binary")
	f.write(home+"/models/m.gguf", "w")
	data, _ := json.Marshal(install.Manifest{Version: 1, OS: "darwin", Files: []string{cfg, plist, "/etc/hosts"},
		Dirs: []string{llama}, LaunchAgent: "fi.viiwork.node", Binary: home + "/.local/bin/viiwork", ModelDirs: []string{home + "/models"}})
	f.write(home+"/.config/viiwork/install.json", string(data))
	f.write("/etc/hosts", "hosts")
	if err := f.run(Options{Yes: true}); err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
	if !slices.Equal(*f.calls, []string{"launchctl print gui/501/fi.viiwork.node", "launchctl bootout gui/501/fi.viiwork.node"}) {
		t.Errorf("calls %q", *f.calls)
	}
	for _, p := range []string{cfg, plist, llama, home + "/.local/bin/viiwork", home + "/.config/viiwork"} {
		if f.exists(p) {
			t.Errorf("%s survived", p)
		}
	}
	if !f.exists("/etc/hosts") || !f.exists(home+"/models/m.gguf") {
		t.Error("removed something outside the install")
	}
}

// A node that could not be stopped keeps running with restart: always; deleting
// its files and manifest under it would leave a crash loop nothing can undo.
func TestStopFailureRemovesNothing(t *testing.T) {
	f := newLinux(t, 2)
	f.fail["docker compose -f "+f.path(install.ComposeFile)+" -p viiwork stop -t 90"] = true
	if err := f.run(Options{Yes: true}); err == nil {
		t.Fatal("no error")
	}
	for _, p := range []string{install.ManifestFile, install.ConfigFile, install.BinaryPath, install.StateDir} {
		if !f.exists(p) {
			t.Errorf("%s was removed", p)
		}
	}
	for _, c := range *f.calls {
		if strings.Contains(c, "rmi") || strings.Contains(c, "down") {
			t.Errorf("ran %q after a failed stop", c)
		}
	}
}

// The installer writes its manifest before the compose file: a crash between
// them leaves nothing to stop, and uninstall must still finish.
func TestProvisionalManifestWithoutComposeFile(t *testing.T) {
	f := newLinux(t, 2)
	os.Remove(f.path(install.ComposeFile))
	if err := f.run(Options{Yes: true, KeepImages: true}); err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
	if len(*f.calls) != 0 || f.exists(install.ManifestFile) {
		t.Errorf("calls %q, manifest left: %v", *f.calls, f.exists(install.ManifestFile))
	}
}

// When a removal fails, the manifest stays so that a rerun can finish, and the
// command says so.
func TestRemovalFailureKeepsTheManifest(t *testing.T) {
	f := newLinux(t, 2)
	locked := f.path(install.StateDir + "/releases")
	os.Chmod(locked, 0o555)
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	if err := f.run(Options{Yes: true}); err == nil {
		t.Fatal("no error")
	}
	if !f.exists(install.ManifestFile) {
		t.Error("the manifest went though a removal failed")
	}
}

func TestMacRefusesRoot(t *testing.T) {
	f := newLinux(t, 2)
	f.h.GOOS, f.h.Home, f.h.Euid = "darwin", "/Users/u", 0
	if err := f.run(Options{Yes: true}); err == nil || !strings.Contains(err.Error(), "without sudo") {
		t.Errorf("err %v", err)
	}
}

func TestMacBootoutFailureRemovesNothing(t *testing.T) {
	f := newLinux(t, 2)
	home := "/Users/u"
	f.h.GOOS, f.h.Home, f.h.Euid, f.h.UID = "darwin", home, 501, 501
	f.write(home+"/.local/bin/viiwork", "#!binary")
	data, _ := json.Marshal(install.Manifest{Version: 1, OS: "darwin", LaunchAgent: "fi.viiwork.node", Binary: home + "/.local/bin/viiwork"})
	f.write(home+"/.config/viiwork/install.json", string(data))
	f.fail["launchctl bootout gui/501/fi.viiwork.node"] = true
	if err := f.run(Options{Yes: true}); err == nil {
		t.Fatal("no error")
	}
	if !f.exists(home+"/.local/bin/viiwork") || !f.exists(home+"/.config/viiwork/install.json") {
		t.Error("files removed after a failed bootout")
	}
}

// Removing /var/lib/viiwork/x/secret where x is a symlink would delete the
// file the link points at: such an entry is skipped and named.
func TestAPathThroughASymlinkIsSkipped(t *testing.T) {
	f := newLinux(t, 2)
	f.write("/outside/secret", "keep me")
	os.Symlink(f.path("/outside"), f.path(install.StateDir+"/x"))
	f.manifest(install.Manifest{Files: []string{install.StateDir + "/x/secret"}})
	if err := f.run(Options{Yes: true}); err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
	if !f.exists("/outside/secret") {
		t.Error("a file behind a symlink was removed")
	}
	if !strings.Contains(f.out.String(), "skipped "+install.StateDir+"/x/secret") {
		t.Errorf("not reported:\n%s", f.out)
	}
}

// A symlinked model is not deleted through the link, and the link is not
// removed while the prompt claims the weights went: it is skipped and named.
func TestASymlinkedModelIsSkipped(t *testing.T) {
	f := newLinux(t, 2)
	f.write("/elsewhere/real.gguf", "weights")
	os.Remove(f.path("/models/alpha.gguf"))
	os.Symlink(f.path("/elsewhere/real.gguf"), f.path("/models/alpha.gguf"))
	if err := f.run(Options{Yes: true, DeleteModels: true}); err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
	if !f.exists("/elsewhere/real.gguf") {
		t.Error("the link's target was deleted")
	}
	if _, err := os.Lstat(f.path("/models/alpha.gguf")); err != nil {
		t.Error("the link was removed though its weights stay")
	}
	if !strings.Contains(f.out.String(), "skipped model /models/alpha.gguf") {
		t.Errorf("not reported:\n%s", f.out)
	}
	if f.exists("/models/sub/beta-00001-of-00002.gguf") {
		t.Error("the other model was not deleted")
	}
}
