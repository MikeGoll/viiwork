package install

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/janit/viiwork/v2/meshapi"
)

func TestMacLayout(t *testing.T) {
	p := MacLayout("/Users/u")
	want := MacPaths{
		ConfigDir:    "/Users/u/.config/viiwork",
		ConfigFile:   "/Users/u/.config/viiwork/viiwork.yaml",
		ManifestFile: "/Users/u/.config/viiwork/install.json",
		Plist:        "/Users/u/Library/LaunchAgents/fi.viiwork.node.plist",
		BinaryPath:   "/Users/u/.local/bin/viiwork",
		StateDir:     "/Users/u/.local/state/viiwork",
		LogDir:       "/Users/u/Library/Logs/viiwork",
		LogFile:      "/Users/u/Library/Logs/viiwork/viiwork.log",
		LlamaRoot:    "/Users/u/.local/share/viiwork/llama.cpp",
	}
	if p != want {
		t.Errorf("%+v", p)
	}
}

func mac(t *testing.T) (Mac, *[]string) {
	exe := filepath.Join(t.TempDir(), "viiwork")
	os.WriteFile(exe, []byte("#!binary"), 0o755)
	var calls []string
	return Mac{
		Root: t.TempDir(), P: MacLayout("/Users/u"), UID: 501, Out: io.Discard, Executable: exe,
		Node: Node{HTTP: &http.Client{}},
		Exec: func(_ context.Context, _ io.Writer, name string, args ...string) error {
			calls = append(calls, name+" "+strings.Join(args, " "))
			return nil
		},
	}, &calls
}

func TestMacWriteRecordsWhatItWrote(t *testing.T) {
	m, _ := mac(t)
	files := []File{{Path: m.P.ConfigFile, Mode: 0o644, Data: []byte("node: {}\n")}, {Path: m.P.Plist, Mode: 0o600, Data: []byte("<plist/>")}}
	got, err := m.Write(context.Background(), []string{m.P.ConfigDir, m.P.StateDir}, files, Manifest{InstalledBy: "v2.6.0"})
	if err != nil {
		t.Fatal(err)
	}
	read, err := ReadManifest(filepath.Join(m.Root, m.P.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if read.OS != "darwin" || read.LaunchAgent != LaunchAgent || read.Binary != m.P.BinaryPath ||
		!slices.Equal(read.Files, []string{m.P.ConfigFile, m.P.Plist}) || !slices.Equal(read.Dirs, []string{m.P.ConfigDir, m.P.StateDir}) {
		t.Errorf("manifest %+v", read)
	}
	if !slices.Equal(got.Files, read.Files) {
		t.Errorf("returned %+v", got)
	}
	for p, mode := range map[string]os.FileMode{m.P.Plist: 0o600, m.P.ConfigFile: 0o644, m.P.BinaryPath: 0o755} {
		fi, err := os.Stat(filepath.Join(m.Root, p))
		if err != nil || fi.Mode().Perm() != mode {
			t.Errorf("%s: %v %v", p, fi, err)
		}
	}
}

func TestMacBootstrapAndLoaded(t *testing.T) {
	m, calls := mac(t)
	if err := m.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.Loaded(context.Background())
	want := []string{
		"launchctl bootstrap gui/501 " + filepath.Join(m.Root, m.P.Plist),
		"launchctl print gui/501/fi.viiwork.node",
	}
	if !slices.Equal(*calls, want) {
		t.Errorf("calls %q", *calls)
	}
	if !strings.Contains(m.Kickstart(), "launchctl kickstart -k gui/501/fi.viiwork.node") {
		t.Errorf("kickstart %q", m.Kickstart())
	}
}

func TestMacDiagnoseTailsTheLog(t *testing.T) {
	st := meshapi.NodeStatus{Node: "mac", Models: []meshapi.ModelStatus{{Name: "m", Backends: []meshapi.BackendStatus{{ID: "m-0", Status: "unhealthy", Phase: "loading"}}}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(st) }))
	defer srv.Close()
	m, _ := mac(t)
	var out bytes.Buffer
	m.Out, m.Node.API = &out, strings.TrimPrefix(srv.URL, "http://")
	var log strings.Builder
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&log, "line %d\n", i)
	}
	os.MkdirAll(filepath.Join(m.Root, m.P.LogDir), 0o755)
	os.WriteFile(filepath.Join(m.Root, m.P.LogFile), []byte(log.String()), 0o644)
	m.Diagnose(context.Background())
	s := out.String()
	if !strings.Contains(s, "m-0: unhealthy loading") || !strings.Contains(s, "line 11\n") || strings.Contains(s, "line 10\n") || !strings.Contains(s, "line 30") {
		t.Errorf("output %q", s)
	}
}
