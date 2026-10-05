package install

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func writeManifest(t *testing.T, path string, m Manifest) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	b, _ := json.Marshal(m)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestManagedInstall(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "install.json")
	if ManagedInstall(p) {
		t.Error("no manifest: managed")
	}
	compose := &Compose{File: ComposeFile, Project: Project}
	helper := &EngineHelper{Units: EngineUnits, Dir: EngineDir}
	for _, c := range []struct {
		m    Manifest
		want bool
	}{
		{Manifest{Version: 1, OS: "linux", Compose: compose, EngineHelper: helper}, true},
		{Manifest{Version: 1, OS: "linux", Compose: compose}, false},     // a beta install
		{Manifest{Version: 1, OS: "linux", EngineHelper: helper}, false}, // config only
		{Manifest{Version: 1, OS: "darwin", Compose: compose, EngineHelper: helper}, false},
		{Manifest{Version: 2, OS: "linux", Compose: compose, EngineHelper: helper}, false},
	} {
		writeManifest(t, p, c.m)
		if got := ManagedInstall(p); got != c.want {
			t.Errorf("%+v: managed %v", c.m, got)
		}
	}
}

func unitFiles() []File {
	var out []File
	for _, u := range EngineUnits {
		out = append(out, File{Path: filepath.Join(SystemdDir, u), Mode: 0o644, Data: []byte("[Unit]\n")})
	}
	return out
}

func TestAddEngineHelperToAnExistingInstall(t *testing.T) {
	l, calls := linux(t)
	before := Manifest{Version: 1, OS: "linux", InstalledBy: "v2.6.0-beta4", Files: []string{ConfigFile, ComposeFile},
		Dirs: []string{ConfigDir}, Compose: &Compose{File: ComposeFile, Project: Project, Volumes: []string{}},
		Images: []string{"ghcr.io/janit/viiwork-llamacpp-cuda:v2.6.0-beta4"}, Binary: BinaryPath}
	writeManifest(t, filepath.Join(l.Root, ManifestFile), before)
	os.MkdirAll(filepath.Join(l.Root, filepath.Dir(BinaryPath)), 0o755)
	os.WriteFile(filepath.Join(l.Root, BinaryPath), []byte("old"), 0o755)

	m, err := l.AddEngineHelper(context.Background(), before, unitFiles())
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadManifest(filepath.Join(l.Root, ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	wantFiles := []string{ConfigFile, ComposeFile}
	for _, f := range unitFiles() {
		wantFiles = append(wantFiles, f.Path)
	}
	if !slices.Equal(got.Files, wantFiles) || !slices.Equal(got.Dirs, []string{ConfigDir, EngineDir}) ||
		got.EngineHelper == nil || !slices.Equal(got.EngineHelper.Units, EngineUnits) || got.Binary != BinaryPath ||
		got.InstalledBy != "v2.6.0-beta4" || !slices.Equal(got.Images, before.Images) {
		t.Errorf("manifest %+v", got)
	}
	if !slices.Equal(m.Files, got.Files) {
		t.Errorf("returned %+v", m)
	}
	if b, _ := os.ReadFile(filepath.Join(l.Root, BinaryPath)); string(b) != "#!binary" {
		t.Errorf("binary not replaced: %q", b)
	}
	if fi, err := os.Stat(filepath.Join(l.Root, EngineDir)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("helper dir: %v %v", fi, err)
	}
	for _, f := range unitFiles() {
		if _, err := os.Stat(filepath.Join(l.Root, f.Path)); err != nil {
			t.Errorf("unit: %v", err)
		}
	}
	want := []string{"systemctl daemon-reload", "systemctl enable --now viiwork-engine.path viiwork-engine.timer"}
	if !slices.Equal(*calls, want) {
		t.Errorf("calls %q", *calls)
	}
}

// A helper whose units fail to enable is not recorded: the node does not go
// managed and wait on a helper that never acts, and the next sudo viiwork
// init takes the add-helper path again. Its files are recorded all along, so
// an uninstall still removes them.
func TestAFailedEngineHelperAddCanBeRetried(t *testing.T) {
	l, _ := linux(t)
	before := Manifest{Version: 1, OS: "linux", InstalledBy: "v2.6.0", Files: []string{ConfigFile, ComposeFile},
		Dirs: []string{ConfigDir}, Compose: &Compose{File: ComposeFile, Project: Project, Volumes: []string{}}, Binary: BinaryPath}
	path := filepath.Join(l.Root, ManifestFile)
	writeManifest(t, path, before)
	os.MkdirAll(filepath.Join(l.Root, filepath.Dir(BinaryPath)), 0o755)
	ok := l.Exec
	l.Exec = func(ctx context.Context, w io.Writer, name string, args ...string) error {
		if name == "systemctl" && len(args) > 0 && args[0] == "enable" {
			return errors.New("unit masked")
		}
		return ok(ctx, w, name, args...)
	}
	if _, err := l.AddEngineHelper(context.Background(), before, unitFiles()); err == nil {
		t.Fatal("no error from a failed enable")
	}
	got, err := ReadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.EngineHelper != nil || ManagedInstall(path) {
		t.Errorf("a helper that never started is recorded: %+v", got.EngineHelper)
	}
	for _, f := range unitFiles() {
		if !slices.Contains(got.Files, f.Path) {
			t.Errorf("%s not recorded for uninstall", f.Path)
		}
	}
	l.Exec = ok
	if _, err := l.AddEngineHelper(context.Background(), got, unitFiles()); err != nil {
		t.Fatal(err)
	}
	if !ManagedInstall(path) {
		t.Error("the retry did not record the helper")
	}
}
