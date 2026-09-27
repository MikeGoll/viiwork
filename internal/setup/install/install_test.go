package install

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
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

func linux(t *testing.T) (Linux, *[]string) {
	root := t.TempDir()
	exe := filepath.Join(t.TempDir(), "viiwork")
	os.WriteFile(exe, []byte("#!binary"), 0o755)
	var calls []string
	return Linux{
		Root: root, Out: io.Discard, Executable: exe, HTTP: &http.Client{},
		Exec: func(_ context.Context, _ io.Writer, name string, args ...string) error {
			calls = append(calls, name+" "+strings.Join(args, " "))
			return nil
		},
	}, &calls
}

func files() []File {
	return []File{
		{Path: ConfigFile, Mode: 0o644, Data: []byte("node: {}\n")},
		{Path: EnvFile, Mode: 0o640, Data: []byte("VIIWORK_MESH_SECRET=x\n")},
	}
}

func TestWriteRecordsWhatItWrote(t *testing.T) {
	l, _ := linux(t)
	m, err := l.Write(context.Background(), []string{ConfigDir, StateDir}, files(),
		Manifest{InstalledBy: "v2.6.0", Images: []string{"img:v2.6.0"}, ModelDirs: []string{"/models"}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadManifest(filepath.Join(l.Root, ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Files, []string{ConfigFile, EnvFile}) || !slices.Equal(got.Dirs, []string{ConfigDir, StateDir}) ||
		got.Binary != BinaryPath || got.OS != "linux" || got.Version != 1 || got.InstalledBy != "v2.6.0" ||
		!slices.Equal(got.Images, []string{"img:v2.6.0"}) {
		t.Errorf("manifest %+v", got)
	}
	if !slices.Equal(m.Files, got.Files) {
		t.Errorf("returned %+v", m)
	}
	fi, _ := os.Stat(filepath.Join(l.Root, EnvFile))
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("mesh.env mode %v", fi.Mode())
	}
	bin, err := os.ReadFile(filepath.Join(l.Root, BinaryPath))
	fi, _ = os.Stat(filepath.Join(l.Root, BinaryPath))
	if err != nil || string(bin) != "#!binary" || fi.Mode().Perm() != 0o755 {
		t.Errorf("binary %q %v %v", bin, fi.Mode(), err)
	}
}

func TestWriteSkipsDirsThatExisted(t *testing.T) {
	l, _ := linux(t)
	os.MkdirAll(filepath.Join(l.Root, StateDir), 0o755)
	m, err := l.Write(context.Background(), []string{ConfigDir, StateDir}, files(), Manifest{})
	if err != nil || !slices.Equal(m.Dirs, []string{ConfigDir}) {
		t.Errorf("dirs %v, %v", m.Dirs, err)
	}
}

// Ctrl-C during the write stops after the current file, and the manifest is
// rewritten to list only what exists.
func TestWriteCancelledLeavesAConsistentManifest(t *testing.T) {
	l, _ := linux(t)
	ctx, cancel := context.WithCancel(context.Background())
	l.beforeFile = func(string) { cancel() }
	_, err := l.Write(ctx, []string{ConfigDir}, files(), Manifest{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	got, err := ReadManifest(filepath.Join(l.Root, ManifestFile))
	if err != nil || !slices.Equal(got.Files, []string{ConfigFile}) || got.Binary != "" || !slices.Equal(got.Dirs, []string{ConfigDir}) {
		t.Errorf("manifest %+v, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(l.Root, EnvFile)); err == nil {
		t.Error("a file was written after cancel")
	}
}

// Cancelled before it starts, Write creates nothing, not even a manifest.
func TestWriteCancelledBeforeItStartsWritesNothing(t *testing.T) {
	l, _ := linux(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.Write(ctx, []string{ConfigDir}, files(), Manifest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(filepath.Join(l.Root, ConfigDir)); err == nil {
		t.Error("something was created")
	}
}

func TestStartPullsThenComposeUp(t *testing.T) {
	l, calls := linux(t)
	if err := l.Start(context.Background(), "img:v2.6.0"); err != nil {
		t.Fatal(err)
	}
	want := []string{"docker pull img:v2.6.0", "docker compose -f " + filepath.Join(l.Root, ComposeFile) + " -p viiwork up -d"}
	if !slices.Equal(*calls, want) {
		t.Errorf("calls %q", *calls)
	}
}

func TestStartNamesTheImageThatFailed(t *testing.T) {
	l, _ := linux(t)
	var calls int
	l.Exec = func(context.Context, io.Writer, string, ...string) error { calls++; return errors.New("exit 1") }
	err := l.Start(context.Background(), "img:v2.6.0")
	if err == nil || !strings.Contains(err.Error(), "img:v2.6.0") || calls != 1 {
		t.Errorf("err %v after %d calls", err, calls)
	}
}

func TestDiagnoseShowsUnhealthyBackendsAndLogs(t *testing.T) {
	st := meshapi.NodeStatus{Node: "node-a", Models: []meshapi.ModelStatus{{Name: "alpha", Backends: []meshapi.BackendStatus{
		{ID: "alpha-0", Status: meshapi.StatusHealthy},
		{ID: "alpha-1", Status: "unhealthy", Phase: "loading"},
	}}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(st) }))
	defer srv.Close()
	l, calls := linux(t)
	var out bytes.Buffer
	l.Out, l.NodeAPI = &out, strings.TrimPrefix(srv.URL, "http://")
	l.Diagnose(context.Background())
	if !strings.Contains(out.String(), "alpha-1: unhealthy loading") || strings.Contains(out.String(), "alpha-0") {
		t.Errorf("output %q", out.String())
	}
	if len(*calls) != 1 || !strings.HasSuffix((*calls)[0], "logs --tail 20") {
		t.Errorf("calls %q", *calls)
	}
}

func TestReadManifestRefusesANewerVersion(t *testing.T) {
	p := filepath.Join(t.TempDir(), "install.json")
	os.WriteFile(p, []byte(`{"version": 2}`), 0o644)
	if _, err := ReadManifest(p); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Errorf("%v", err)
	}
}

// A crash part-way through must leave a manifest uninstall can act on, so one
// is on disk before the first file is.
func TestManifestComesFirst(t *testing.T) {
	l, _ := linux(t)
	var seen []string
	l.beforeFile = func(path string) {
		m, err := ReadManifest(filepath.Join(l.Root, ManifestFile))
		if err != nil || !slices.Contains(m.Files, ConfigFile) || !slices.Contains(m.Files, EnvFile) || m.Binary != BinaryPath {
			t.Errorf("before %s: manifest %+v, %v", path, m, err)
		}
		seen = append(seen, path)
	}
	if _, err := l.Write(context.Background(), []string{ConfigDir}, files(), Manifest{}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Errorf("hook saw %q", seen)
	}
}

// A binary that was already there is replaced but not claimed: uninstall must
// not delete what the install did not create.
func TestExistingBinaryIsNotClaimed(t *testing.T) {
	l, _ := linux(t)
	os.MkdirAll(filepath.Join(l.Root, filepath.Dir(BinaryPath)), 0o755)
	os.WriteFile(filepath.Join(l.Root, BinaryPath), []byte("old"), 0o755)
	m, err := l.Write(context.Background(), []string{ConfigDir}, files(), Manifest{})
	if err != nil || m.Binary != "" {
		t.Errorf("binary %q, %v", m.Binary, err)
	}
}

func TestWaitUpAcceptsAnyAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	l, _ := linux(t)
	l.NodeAPI = strings.TrimPrefix(srv.URL, "http://")
	if err := l.WaitUp(context.Background(), 2*time.Second); err != nil {
		t.Errorf("a 503 from a loading node: %v", err)
	}
	srv.Close()
	if err := l.WaitUp(context.Background(), 1500*time.Millisecond); err == nil {
		t.Error("no node, yet up")
	}
}
