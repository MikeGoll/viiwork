package update

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// stage writes a staged release by hand: binary content body, and its
// recorded sha256 (a wrong one when tamper is set).
func stage(t *testing.T, dir, version, body string, tamper bool) string {
	t.Helper()
	d := filepath.Join(dir, version)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(d, "viiwork")
	os.WriteFile(bin, []byte(body), 0o755)
	sum := sha256.Sum256([]byte(body))
	if tamper {
		sum[0] ^= 1
	}
	os.WriteFile(filepath.Join(d, "viiwork.sha256"), []byte(hex.EncodeToString(sum[:])+"\n"), 0o644)
	return bin
}

func TestStateRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "releases")
	s, err := LoadState(dir)
	if err != nil || s.Current != Builtin || s.LastGood != Builtin || s.Pending != nil {
		t.Fatalf("missing state = %+v, %v", s, err)
	}
	s.Current = "v2.6.0"
	s.Pending = &Pending{Version: "v2.6.0", Attempts: 1, Baseline: []string{"m/0"}}
	s.Launcher = &Launcher{Path: "/usr/local/bin/viiwork", SHA256: strings.Repeat("a", 64)}
	if err := SaveState(dir, s); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(dir)
	if err != nil || got.Current != "v2.6.0" || got.Pending.Attempts != 1 || got.Launcher.Path != s.Launcher.Path {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
}

func TestStateRefusesWhatItCannotRead(t *testing.T) {
	for name, content := range map[string]string{
		"truncated":       `{"current":"v2.6`,
		"not a version":   `{"current":"../../bin/sh","last_good":"builtin"}`,
		"empty last_good": `{"current":"builtin","last_good":""}`,
		"bad pending":     `{"current":"v2.6.0","last_good":"builtin","pending":{"version":"x"}}`,
	} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "state.json"), []byte(content), 0o644)
		if _, err := LoadState(dir); err == nil || !strings.Contains(err.Error(), "state.json") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestBinaryAndStaged(t *testing.T) {
	dir := t.TempDir()
	bin := stage(t, dir, "v2.6.0", "BIN", false)
	if p, err := Binary(dir, "v2.6.0"); err != nil || p != bin {
		t.Fatalf("Binary = %q, %v", p, err)
	}
	stage(t, dir, "v2.5.0", "OLD", true)
	if _, err := Binary(dir, "v2.5.0"); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Errorf("tampered binary: %v", err)
	}
	if _, err := Binary(dir, "v9.9.9"); err == nil {
		t.Error("a version never staged has a binary")
	}
	if _, err := Binary(dir, "../v2.6.0"); err == nil {
		t.Error("a path was accepted as a version")
	}
	stage(t, dir, "v2.7.0-rc.1", "RC", false)
	os.MkdirAll(filepath.Join(dir, ".v2.8.0.staging-123"), 0o755) // an interrupted stage
	if got := Staged(dir); !slices.Equal(got, []string{"v2.7.0-rc.1", "v2.6.0", "v2.5.0"}) {
		t.Errorf("Staged = %v", got)
	}
}

func TestPrune(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []string{"v2.3.0", "v2.4.0", "v2.5.0", "v2.6.0", "v2.7.0"} {
		stage(t, dir, v, v, false)
	}
	s := State{Current: "v2.4.0", LastGood: "v2.3.0"}
	if err := Prune(dir, s); err != nil {
		t.Fatal(err)
	}
	// current, last good, and the newest other one stay.
	if got := Staged(dir); !slices.Equal(got, []string{"v2.7.0", "v2.4.0", "v2.3.0"}) {
		t.Errorf("after prune: %v", got)
	}
}

func TestLaunchers(t *testing.T) {
	self, err := SelfLauncher()
	if err != nil || self.Path == "" || len(self.SHA256) != 64 {
		t.Fatalf("SelfLauncher = %+v, %v", self, err)
	}
	dir := t.TempDir()
	if _, err := RestartTarget(dir); err == nil {
		t.Error("restart target without a recorded launcher")
	}
	s := NewState()
	s.Launcher = &self
	SaveState(dir, s)
	if p, err := RestartTarget(dir); err != nil || p != self.Path {
		t.Fatalf("RestartTarget = %q, %v", p, err)
	}
	s.Launcher = &Launcher{Path: self.Path, SHA256: strings.Repeat("0", 64)}
	SaveState(dir, s)
	if _, err := RestartTarget(dir); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Errorf("replaced launcher: %v", err)
	}
}

// A later release may add fields to state.json and then roll back to this
// one. If an unknown field were refused, the launcher that must recover the
// node could not read the file, and every start would fail: docker, systemd
// and launchd would crash-loop it. Unknown fields are ignored (never acted
// on); the fields this release uses are still validated.
func TestStateReadsAFileFromANewerRelease(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"current":"v2.6.0","last_good":"builtin",`+
		`"format":2,"exec":"/bin/sh","pending":{"version":"v2.7.0","attempts":1,"canary":true},"launcher":{"future":1}}`), 0o644)
	s, err := LoadState(dir)
	if err != nil {
		t.Fatalf("a newer release's state.json: %v", err)
	}
	if s.Current != "v2.6.0" || s.LastGood != Builtin || s.Pending == nil || s.Pending.Version != "v2.7.0" {
		t.Errorf("state %+v", s)
	}
}
