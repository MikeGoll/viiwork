package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

// A pending release the floor already is — the image a Docker install's
// helper swapped in, or a binary laid over by hand at the same version — is
// run and kept pending, so the confirmer decides it, rather than forgotten
// by the newer-floor rule.
func TestDecideKeepsAPendingReleaseTheFloorAlreadyIs(t *testing.T) {
	staged := stagedIn(map[string]string{"v2.6.0": "/s/v2.6.0/viiwork", "v2.5.9": "/s/v2.5.9/viiwork"})
	p := State{Current: "v2.6.0", LastGood: "v2.5.9", Pending: &Pending{Version: "v2.6.0", Baseline: []string{"m/0"}}}
	d := Decide(p, "v2.6.0", staged)
	if d.Exec != "" || !d.Changed || d.State.Current != "v2.6.0" || d.State.LastGood != "v2.5.9" ||
		d.State.Pending == nil || d.State.Pending.Attempts != 1 || len(d.State.Pending.Baseline) != 1 {
		t.Errorf("equal floor, pending: %+v", d)
	}

	// Its third start without confirming falls back to last good, which the
	// newer-floor rule then resolves as ever: the floor runs.
	p.Pending.Attempts = 2
	d = Decide(p, "v2.6.0", staged)
	if d.Exec != "" || d.State.Pending != nil || d.State.Current != Builtin || d.State.LastGood != Builtin {
		t.Errorf("third start: %+v", d)
	}

	// A private build of the version is not that release: the rule as before.
	p.Pending.Attempts = 0
	d = Decide(p, "v2.6.0-g1a2b3c4", staged)
	if d.Exec != "" || d.State.Current != Builtin || d.State.Pending != nil {
		t.Errorf("private build: %+v", d)
	}
}

func TestDecideManaged(t *testing.T) {
	if d := DecideManaged(NewState(), "v2.6.0"); d.Exec != "" || d.Changed {
		t.Errorf("builtin: %+v", d)
	}

	// The helper swapped the image in: count the start, keep it pending.
	p := State{Current: "v2.6.1", LastGood: "v2.6.0", Pending: &Pending{Version: "v2.6.1"}}
	d := DecideManaged(p, "v2.6.1")
	if d.Exec != "" || !d.Changed || d.State.Pending == nil || d.State.Pending.Attempts != 1 || d.State.Current != "v2.6.1" {
		t.Errorf("swapped in: %+v", d)
	}
	p.Pending.Attempts = 2
	d = DecideManaged(p, "v2.6.1")
	if d.Exec != "" || d.State.Pending != nil || d.State.Current != "v2.6.0" || d.State.LastGood != "v2.6.0" ||
		!strings.Contains(strings.Join(d.Logs, " "), "helper") {
		t.Errorf("third start: %+v", d)
	}

	// Not swapped yet: nothing is counted and nothing is exec'd.
	p.Pending.Attempts = 0
	if d := DecideManaged(p, "v2.6.0"); d.Exec != "" || d.Changed {
		t.Errorf("not swapped yet: %+v", d)
	}

	// The image is the source of truth: no floor ever resets the state.
	for _, running := range []string{"v2.7.0", "v2.6.1", "v2.5.0"} {
		s := State{Current: "v2.6.1", LastGood: "v2.6.1", Previous: "v2.6.0"}
		if d := DecideManaged(s, running); d.Exec != "" || d.Changed {
			t.Errorf("confirmed, running %s: %+v", running, d)
		}
	}
}

func TestStartupManagedNeverExecs(t *testing.T) {
	stateDir := t.TempDir()
	dir := ReleasesDir(stateDir)
	stage(t, dir, "v2.6.0", "BIN", false)
	SaveState(dir, State{Current: "v2.6.0", LastGood: "v2.6.0"})
	self := func() (Launcher, error) {
		return Launcher{Path: "/usr/local/bin/viiwork", SHA256: strings.Repeat("b", 64)}, nil
	}
	exec, _, err := Startup(StartupEnv{StateDir: stateDir, Running: "v2.5.0", Managed: true, Self: self})
	if err != nil || exec != "" {
		t.Fatalf("Startup = %q, %v", exec, err)
	}
	if s, _ := LoadState(dir); s.Current != "v2.6.0" || s.LastGood != "v2.6.0" {
		t.Errorf("state changed: %+v", s)
	}
}

// On a helper-managed install the release is the image: its engine, not the
// running one, is what the requirements are checked against.
func TestStagePreparesTheImage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src, pub := fakeRelease(t, "v2.6.0", releaseOpts{requires: `{"llamacpp":"b100"}`})
	st := stager(dir, src, pub)
	st.Engines = func(context.Context, map[string]string) error {
		t.Error("the running engines were checked")
		return nil
	}
	var asked string
	var req map[string]string
	st.PrepareImage = func(_ context.Context, v string, r map[string]string) error { asked, req = v, r; return nil }
	if err := st.Stage(ctx, "v2.6.0"); err != nil {
		t.Fatal(err)
	}
	if asked != "v2.6.0" || req["llamacpp"] != "b100" {
		t.Errorf("PrepareImage(%q, %v)", asked, req)
	}
	// Staged before, by a node without a helper: the image is still prepared.
	asked = ""
	if err := st.Stage(ctx, "v2.6.0"); err != nil || asked != "v2.6.0" {
		t.Errorf("restage: %v, asked %q", err, asked)
	}

	dir = t.TempDir()
	st = stager(dir, src, pub)
	st.PrepareImage = func(context.Context, string, map[string]string) error { return errors.New("pull failed") }
	if err := st.Stage(ctx, "v2.6.0"); err == nil || !strings.Contains(err.Error(), "pull failed") || len(Staged(dir)) != 0 {
		t.Errorf("a failed image: %v, staged %v", err, Staged(dir))
	}
}

func TestCheckEngineVersions(t *testing.T) {
	models := modelsWithLlama(t, "version: 120 (abc)")
	if err := CheckEngineVersions(models, map[string]string{"llamacpp": "b100"}, map[string]string{"llamacpp": "b10438"}); err != nil {
		t.Errorf("new enough: %v", err)
	}
	err := CheckEngineVersions(models, map[string]string{"llamacpp": "b20000"}, map[string]string{"llamacpp": "b10438"})
	if !errors.Is(err, ErrEngineTooOld) || !strings.Contains(err.Error(), "b10438") {
		t.Errorf("too old: %v", err)
	}
	if err := CheckEngineVersions(models, map[string]string{"llamacpp": "b100"}, nil); !errors.Is(err, ErrEngineTooOld) {
		t.Errorf("unknown version passed: %v", err)
	}
	if err := CheckEngineVersions(models, map[string]string{"vllm": "0.11.0"}, nil); err != nil {
		t.Errorf("an engine no model uses was checked: %v", err)
	}
}

// managedService is service() on a helper-managed install whose helper gets
// wait to restart the node.
func managedService(t *testing.T, wait time.Duration) (*Service, *atomic.Int32, chan struct{}, *logs) {
	t.Helper()
	s, restarts := service(t, true, allow(true))
	stopping := make(chan struct{})
	l := &logs{}
	s.Managed, s.HelperWait, s.Stopping, s.Log = true, wait, stopping, l.add
	prepared(t, s.Store.Dir, "v2.6.0", true)
	return s, restarts, stopping, l
}

// prepared writes the helper's answer for version, as engine-sync does.
func prepared(t *testing.T, dir, version string, ok bool) {
	t.Helper()
	b, _ := json.Marshal(ImageResult{Version: version, ID: strings.Repeat("a", 32), OK: ok})
	if err := os.WriteFile(filepath.Join(dir, ImageResultFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Activation must not wait on a pull the helper has not finished: the
// watchdog would give up after two minutes and the update would not take.
func TestManagedActivateNeedsThePreparedImage(t *testing.T) {
	s, _, _, _ := managedService(t, time.Minute)
	call(s, http.MethodPost, meshapi.PathUpdateStage, `{"version":"v2.6.0"}`)
	for _, c := range []struct {
		version string
		ok      bool
		remove  bool
	}{{"v2.6.1", true, false}, {"v2.6.0", false, false}, {"", false, true}} {
		if c.remove {
			os.Remove(filepath.Join(s.Store.Dir, ImageResultFile))
		} else {
			prepared(t, s.Store.Dir, c.version, c.ok)
		}
		code, body := call(s, http.MethodPost, meshapi.PathUpdateActivate, `{"version":"v2.6.0"}`)
		if code != 409 || !strings.Contains(body, "not prepared") {
			t.Errorf("%+v: %d %s", c, code, body)
		}
	}
	if st, _ := LoadState(s.Store.Dir); st.Current != Builtin {
		t.Errorf("state changed: %+v", st)
	}
	prepared(t, s.Store.Dir, "v2.6.0", true)
	if code, body := call(s, http.MethodPost, meshapi.PathUpdateActivate, `{"version":"v2.6.0"}`); code != 202 {
		t.Errorf("prepared: %d %s", code, body)
	}
}

type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) add(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logs) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// waitState polls state.json until ok, for a watchdog running in the
// background.
func waitState(t *testing.T, dir string, ok func(State) bool) State {
	t.Helper()
	var st State
	for i := 0; i < 200; i++ {
		st, _ = LoadState(dir)
		if ok(st) {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("state never reached: %+v", st)
	return st
}

func TestManagedActivateLeavesTheRestartToTheHelper(t *testing.T) {
	s, restarts, _, l := managedService(t, 50*time.Millisecond)
	if code, body := call(s, http.MethodPost, meshapi.PathUpdateStage, `{"version":"v2.6.0"}`); code != 200 {
		t.Fatalf("stage: %d %s", code, body)
	}
	if code, body := call(s, http.MethodPost, meshapi.PathUpdateActivate, `{"version":"v2.6.0"}`); code != 202 {
		t.Fatalf("activate: %d %s", code, body)
	}
	st, _ := LoadState(s.Store.Dir)
	if st.Current != "v2.6.0" || st.Pending == nil || st.Pending.Version != "v2.6.0" {
		t.Fatalf("state after activate: %+v", st)
	}
	// The helper did not act: the node is still running the old release
	// when the wait is over, so it goes back to the state it had.
	st = waitState(t, s.Store.Dir, func(st State) bool { return st.Pending == nil })
	if st.Current != Builtin || st.LastGood != Builtin {
		t.Errorf("restored: %+v", st)
	}
	if restarts.Load() != 0 {
		t.Error("a managed node restarted itself")
	}
	if !strings.Contains(l.text(), "viiwork-engine") {
		t.Errorf("the log does not say why:\n%s", l.text())
	}
	// Nothing blocks the next write.
	if code, body := call(s, http.MethodPost, meshapi.PathUpdateActivate, `{"version":"v2.6.0"}`); code != 202 {
		t.Errorf("activate again: %d %s", code, body)
	}
}

func TestManagedWatchdogStandsDown(t *testing.T) {
	// The helper's compose up is stopping this node: nothing is restored.
	s, _, stopping, _ := managedService(t, 50*time.Millisecond)
	call(s, http.MethodPost, meshapi.PathUpdateStage, `{"version":"v2.6.0"}`)
	call(s, http.MethodPost, meshapi.PathUpdateActivate, `{"version":"v2.6.0"}`)
	close(stopping)
	time.Sleep(150 * time.Millisecond)
	if st, _ := LoadState(s.Store.Dir); st.Current != "v2.6.0" || st.Pending == nil {
		t.Errorf("restored while stopping: %+v", st)
	}

	// A later change is not undone by an earlier activation's watchdog.
	s, _, _, _ = managedService(t, 50*time.Millisecond)
	call(s, http.MethodPost, meshapi.PathUpdateStage, `{"version":"v2.6.0"}`)
	call(s, http.MethodPost, meshapi.PathUpdateActivate, `{"version":"v2.6.0"}`)
	SaveState(s.Store.Dir, State{Current: "v2.6.0", LastGood: "v2.6.0"}) // confirmed meanwhile
	time.Sleep(150 * time.Millisecond)
	if st, _ := LoadState(s.Store.Dir); st.Current != "v2.6.0" || st.LastGood != "v2.6.0" {
		t.Errorf("a later state was undone: %+v", st)
	}
}

func TestManagedRollback(t *testing.T) {
	s, restarts, _, _ := managedService(t, 50*time.Millisecond)
	s.Running = "v2.6.1"
	SaveState(s.Store.Dir, State{Current: "v2.6.1", LastGood: "v2.6.1", Previous: Builtin})
	if code, body := call(s, http.MethodPost, meshapi.PathUpdateRollback, `{}`); code != 202 {
		t.Fatalf("rollback: %d %s", code, body)
	}
	if st, _ := LoadState(s.Store.Dir); st.Current != Builtin || st.LastGood != Builtin {
		t.Errorf("after rollback: %+v", st)
	}
	st := waitState(t, s.Store.Dir, func(st State) bool { return st.Current == "v2.6.1" })
	if st.LastGood != "v2.6.1" || st.Previous != Builtin || restarts.Load() != 0 {
		t.Errorf("restored: %+v, restarts %d", st, restarts.Load())
	}
}

// A node that starts with a pending release it is not running — restarted
// before the helper acted — gives the helper the same time, then goes back
// to last good.
func TestManagedWatchPendingAtStart(t *testing.T) {
	s, _, _, _ := managedService(t, 50*time.Millisecond)
	SaveState(s.Store.Dir, State{Current: "v2.6.0", LastGood: "v2.4.0", Previous: "v2.3.0", Pending: &Pending{Version: "v2.6.0"}})
	s.WatchPending()
	st := waitState(t, s.Store.Dir, func(st State) bool { return st.Pending == nil })
	if st.Current != "v2.4.0" || st.LastGood != "v2.4.0" || st.Previous != "v2.3.0" {
		t.Errorf("after the wait: %+v", st)
	}

	// Running the pending release: the confirmer's business, not this.
	s, _, _, _ = managedService(t, 20*time.Millisecond)
	s.Running = "v2.6.0"
	SaveState(s.Store.Dir, State{Current: "v2.6.0", LastGood: "v2.4.0", Pending: &Pending{Version: "v2.6.0"}})
	s.WatchPending()
	time.Sleep(100 * time.Millisecond)
	if st, _ := LoadState(s.Store.Dir); st.Pending == nil {
		t.Errorf("the running pending release was abandoned: %+v", st)
	}
}

func TestManagedConfirmerRollbackDoesNotRestart(t *testing.T) {
	_, c, _, restarts := pendingState(t, "m/0")
	c.Managed = true
	c.Backends = backends(map[string]string{"m/0": meshapi.StatusDead}, "")
	c.Step()
	if *restarts != 0 {
		t.Error("a managed node restarted itself to roll back")
	}
	if st, _ := LoadState(c.Store.Dir); st.Current != "v2.5.0" || st.Pending != nil {
		t.Errorf("state: %+v", st)
	}
	if c.Store.Restarting() {
		t.Error("the store refuses writes, waiting for a restart that will not come")
	}
}
