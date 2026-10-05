package update

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/janit/viiwork/v2/meshapi"
)

// Every fleet host's launcher is v2.6.0-beta4, whose LoadState refused
// unknown fields. It reads state.json on every start, so the release before
// last good must never be written there.
func TestPreviousIsNotInStateJSON(t *testing.T) {
	dir := t.TempDir()
	s := State{Current: "v2.6.0", LastGood: "v2.6.0", Previous: "v2.5.0",
		Launcher: &Launcher{Path: "/usr/local/bin/viiwork", SHA256: strings.Repeat("a", 64)}}
	if err := SaveState(dir, s); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, stateFile))
	var fields map[string]json.RawMessage
	json.Unmarshal(b, &fields)
	var keys []string
	for k := range fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"current", "last_good", "launcher"}) {
		t.Errorf("state.json fields %v: a beta4 launcher refuses any it does not know", keys)
	}
	if got, err := LoadState(dir); err != nil || got.Previous != "v2.5.0" {
		t.Errorf("previous did not round trip: %+v, %v", got, err)
	}
}

// The previous file is remembered for one last good release only. Anything
// else — a missing, torn or hostile file, or one written before last good
// changed without it (a floor reset, a crash between the two writes, an older
// release that never heard of it) — means nothing to roll back to, never an
// error that stops the node.
func TestPreviousOnlyForItsLastGood(t *testing.T) {
	for name, content := range map[string]string{
		"missing":           "",
		"torn":              `{"version":"v2.5`,
		"not a version":     `{"version":"../../bin/sh","before":"v2.6.0"}`,
		"another last good": `{"version":"v2.5.0","before":"v2.5.9"}`,
		"is last good":      `{"version":"v2.6.0","before":"v2.6.0"}`,
	} {
		dir := t.TempDir()
		SaveState(dir, State{Current: "v2.6.0", LastGood: "v2.6.0"})
		if content != "" {
			os.WriteFile(filepath.Join(dir, previousFile), []byte(content), 0o644)
		}
		s, err := LoadState(dir)
		if err != nil || s.Previous != "" {
			t.Errorf("%s: previous %q, %v", name, s.Previous, err)
		}
	}
}

func TestPruneKeepsPrevious(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []string{"v2.3.0", "v2.4.0", "v2.5.0", "v2.6.0", "v2.7.0"} {
		stage(t, dir, v, v, false)
	}
	if err := Prune(dir, State{Current: "v2.5.0", LastGood: "v2.5.0", Previous: "v2.3.0"}); err != nil {
		t.Fatal(err)
	}
	if got := Staged(dir); !slices.Equal(got, []string{"v2.7.0", "v2.5.0", "v2.3.0"}) {
		t.Errorf("after prune: %v", got)
	}
}

func TestConfirmRemembersThePreviousRelease(t *testing.T) {
	dir, c, _, _ := pendingState(t, "m/0")
	c.Backends = backends(map[string]string{"m/0": meshapi.StatusHealthy}, "")
	if !c.Step() {
		t.Fatal("did not confirm")
	}
	if s, _ := LoadState(dir); s.LastGood != "v2.6.0" || s.Previous != "v2.5.0" {
		t.Errorf("after confirm: %+v", s)
	}
}

// The known issue of v2.6.0's betas: once a release confirmed, rollback had
// nothing to go back to.
func TestRollbackAfterConfirmGoesToPrevious(t *testing.T) {
	s, c, restarts := sharedPending(t)
	stage(t, s.Store.Dir, "v2.5.0", "OLD", false)
	c.Backends = backends(map[string]string{"m/0": meshapi.StatusHealthy}, "")
	if !c.Step() {
		t.Fatal("did not confirm")
	}
	code, body := call(s, http.MethodGet, meshapi.PathUpdate, "")
	var us meshapi.UpdateStatus
	json.Unmarshal([]byte(body), &us)
	if code != 200 || us.Previous != "v2.5.0" {
		t.Errorf("status after confirm: %d %s", code, body)
	}
	if code, body := call(s, http.MethodPost, meshapi.PathUpdateRollback, `{}`); code != 202 || !strings.Contains(body, "v2.5.0") {
		t.Fatalf("rollback after confirm: %d %s", code, body)
	}
	waitRestarts(t, restarts, 1)
	st, _ := LoadState(s.Store.Dir)
	if st.Current != "v2.5.0" || st.LastGood != "v2.5.0" || st.Pending != nil || st.Previous != "" {
		t.Errorf("after rollback: %+v", st)
	}
}

// builtin needs no staged binary; a version must still verify, and one that
// does not leaves the state as it was.
func TestRollbackToPreviousOnlyWhenRunnable(t *testing.T) {
	s, restarts := service(t, true, allow(true))
	SaveState(s.Store.Dir, State{Current: "v2.6.0", LastGood: "v2.6.0", Previous: "v2.5.0"})
	if code, body := call(s, http.MethodPost, meshapi.PathUpdateRollback, `{}`); code != 409 || !strings.Contains(body, "v2.5.0") {
		t.Errorf("previous not staged: %d %s", code, body)
	}
	stage(t, s.Store.Dir, "v2.5.0", "OLD", true)
	if code, _ := call(s, http.MethodPost, meshapi.PathUpdateRollback, `{}`); code != 409 {
		t.Errorf("previous edited since staging: %d", code)
	}
	if st, _ := LoadState(s.Store.Dir); st.Current != "v2.6.0" || st.Previous != "v2.5.0" {
		t.Errorf("a refused rollback changed the state: %+v", st)
	}
	SaveState(s.Store.Dir, State{Current: "v2.6.0", LastGood: "v2.6.0", Previous: Builtin})
	if code, body := call(s, http.MethodPost, meshapi.PathUpdateRollback, `{}`); code != 202 {
		t.Fatalf("rollback to builtin: %d %s", code, body)
	}
	waitRestarts(t, restarts, 1)
	if st, _ := LoadState(s.Store.Dir); st.Current != Builtin || st.LastGood != Builtin {
		t.Errorf("after rollback: %+v", st)
	}
}

// A newer floor resets the state to builtin and forgets the staged releases;
// the release before the forgotten last good must go with them, or a rollback
// would send the node to something the floor has replaced.
func TestFloorResetForgetsPrevious(t *testing.T) {
	stateDir := t.TempDir()
	dir := ReleasesDir(stateDir)
	stage(t, dir, "v2.5.0", "OLD", false)
	stage(t, dir, "v2.6.0", "BIN", false)
	SaveState(dir, State{Current: "v2.6.0", LastGood: "v2.6.0", Previous: "v2.5.0"})
	self := func() (Launcher, error) {
		return Launcher{Path: "/usr/local/bin/viiwork", SHA256: strings.Repeat("b", 64)}, nil
	}
	if exec, _, err := Startup(StartupEnv{StateDir: stateDir, Running: "v2.7.0", Self: self}); err != nil || exec != "" {
		t.Fatalf("Startup = %q, %v", exec, err)
	}
	s := &Service{Enabled: true, Running: "v2.7.0", Store: NewStore(dir), Auth: allow(true), Log: func(string, ...any) {}, Restart: func() {}}
	if st, _ := s.Store.Load(); st.Current != Builtin || st.Previous != "" {
		t.Errorf("after the floor reset: %+v", st)
	}
	if code, body := call(s, http.MethodPost, meshapi.PathUpdateRollback, `{}`); code != 409 {
		t.Errorf("rollback after a floor reset: %d %s", code, body)
	}
}
