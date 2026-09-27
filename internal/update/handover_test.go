package update

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stagedIn(m map[string]string) func(string) (string, error) {
	return func(v string) (string, error) {
		if p, ok := m[v]; ok {
			return p, nil
		}
		return "", errors.New("staged " + v + ": the binary no longer matches its recorded sha256")
	}
}

func TestDecide(t *testing.T) {
	staged := stagedIn(map[string]string{"v2.6.0": "/s/v2.6.0/viiwork", "v2.5.9": "/s/v2.5.9/viiwork"})
	fresh := NewState()

	if d := Decide(fresh, "v2.5.0", staged); d.Exec != "" || d.Changed {
		t.Errorf("builtin: %+v", d)
	}

	onStaged := State{Current: "v2.6.0", LastGood: Builtin}
	if d := Decide(onStaged, "v2.5.0", staged); d.Exec != "/s/v2.6.0/viiwork" || d.Changed {
		t.Errorf("hand over: %+v", d)
	}

	// A newer floor — a new image laid over the old one — wins, and forgets
	// the staged releases; so does an equal one, a private build included.
	for _, running := range []string{"v2.7.0", "v2.6.0", "v2.6.0-g1a2b3c4"} {
		d := Decide(onStaged, running, staged)
		if d.Exec != "" || !d.Changed || d.State.Current != Builtin || d.State.LastGood != Builtin || len(d.Logs) == 0 {
			t.Errorf("floor %s: %+v", running, d)
		}
	}

	// A pending release gets three starts to confirm.
	p := State{Current: "v2.6.0", LastGood: Builtin, Pending: &Pending{Version: "v2.6.0", Attempts: 0}}
	d := Decide(p, "v2.5.0", staged)
	if d.Exec == "" || !d.Changed || d.State.Pending.Attempts != 1 {
		t.Errorf("first start: %+v", d)
	}
	p.Pending.Attempts = 2
	d = Decide(p, "v2.5.0", staged)
	if d.Exec != "" || d.State.Current != Builtin || d.State.Pending != nil || len(d.Logs) == 0 {
		t.Errorf("third start without confirmation: %+v", d)
	}
	// ...and falls back to a staged last good release when there is one.
	p = State{Current: "v2.6.0", LastGood: "v2.5.9", Pending: &Pending{Version: "v2.6.0", Attempts: 2}}
	if d := Decide(p, "v2.5.0", staged); d.Exec != "/s/v2.5.9/viiwork" || d.State.Current != "v2.5.9" {
		t.Errorf("fall back to staged last good: %+v", d)
	}

	// A staged binary edited since staging is never run.
	broken := State{Current: "v2.4.0", LastGood: "v2.4.0"}
	d = Decide(broken, "v2.3.0", staged)
	if d.Exec != "" || d.State.Current != Builtin || d.State.LastGood != Builtin || !strings.Contains(strings.Join(d.Logs, " "), "sha256") {
		t.Errorf("tampered staged binary: %+v", d)
	}
}

func TestStartup(t *testing.T) {
	stateDir := t.TempDir()
	dir := ReleasesDir(stateDir)
	bin := stage(t, dir, "v2.6.0", "BIN", false)
	SaveState(dir, State{Current: "v2.6.0", LastGood: Builtin})
	self := func() (Launcher, error) {
		return Launcher{Path: "/usr/local/bin/viiwork", SHA256: strings.Repeat("b", 64)}, nil
	}

	exec, _, err := Startup(StartupEnv{StateDir: stateDir, Running: "v2.5.0", Self: self})
	if err != nil || exec != bin {
		t.Fatalf("Startup = %q, %v", exec, err)
	}
	s, _ := LoadState(dir)
	if s.Launcher == nil || s.Launcher.Path != "/usr/local/bin/viiwork" {
		t.Errorf("launcher not recorded: %+v", s.Launcher)
	}

	// The staged process itself: its launcher already chose it.
	if exec, _, err := Startup(StartupEnv{StateDir: stateDir, Running: "v2.6.0", HandedOver: true, Self: self}); err != nil || exec != "" {
		t.Errorf("handed over: %q, %v", exec, err)
	}
	if s2, _ := LoadState(dir); s2.Current != "v2.6.0" {
		t.Errorf("the handed-over process reset the state: %+v", s2)
	}

	os.WriteFile(filepath.Join(dir, "state.json"), []byte("{garbage"), 0o644)
	if _, _, err := Startup(StartupEnv{StateDir: stateDir, Running: "v2.5.0", Self: self}); err == nil {
		t.Error("started on an unreadable state.json")
	}
}
