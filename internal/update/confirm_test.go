package update

import (
	"testing"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func backends(states map[string]string, phase string) func() []meshapi.ModelStatus {
	return func() []meshapi.ModelStatus {
		var bs []meshapi.BackendStatus
		for id, st := range states {
			bs = append(bs, meshapi.BackendStatus{ID: id, Status: st, Phase: phase})
		}
		return []meshapi.ModelStatus{{Name: "m", Backends: bs}}
	}
}

func pendingState(t *testing.T, baseline ...string) (string, *Confirmer, *fakeClock, *int) {
	t.Helper()
	dir := t.TempDir()
	SaveState(dir, State{Current: "v2.6.0", LastGood: "v2.5.0", Pending: &Pending{Version: "v2.6.0", Baseline: baseline}})
	clock := &fakeClock{t: time.Unix(1790000000, 0)}
	restarts := 0
	c := &Confirmer{Store: NewStore(dir), Running: "v2.6.0", Window: 10 * time.Minute, Now: clock.now,
		Restart: func() { restarts++ }, Log: func(string, ...any) {}}
	return dir, c, clock, &restarts
}

func TestConfirmWhenTheBaselineIsBack(t *testing.T) {
	dir, c, clock, restarts := pendingState(t, "m/0", "m/1")
	states := map[string]string{"m/0": meshapi.StatusStarting, "m/1": meshapi.StatusHealthy, "m/2": meshapi.StatusDead}
	c.Backends = backends(states, "")
	if c.Step() {
		t.Fatal("confirmed while m/0 was still starting")
	}
	if _, ok := c.Deadline(); !ok {
		t.Error("no deadline published")
	}
	states["m/0"] = meshapi.StatusHealthy // m/2 was dead before; it does not count
	clock.t = clock.t.Add(time.Minute)
	if !c.Step() {
		t.Fatal("did not confirm")
	}
	s, _ := LoadState(dir)
	if s.LastGood != "v2.6.0" || s.Pending != nil || *restarts != 0 {
		t.Errorf("after confirm: %+v, restarts %d", s, *restarts)
	}
}

func TestRollBackOnADeadBaselineBackend(t *testing.T) {
	dir, c, _, restarts := pendingState(t, "m/0")
	c.Backends = backends(map[string]string{"m/0": meshapi.StatusDead}, "")
	if !c.Step() || *restarts != 1 {
		t.Fatalf("restarts = %d", *restarts)
	}
	if s, _ := LoadState(dir); s.Current != "v2.5.0" || s.Pending != nil {
		t.Errorf("after rollback: %+v", s)
	}
}

func TestRollBackAtTheDeadlineButNotWhileFetching(t *testing.T) {
	dir, c, clock, restarts := pendingState(t, "m/0")
	fetching := backends(map[string]string{"m/0": meshapi.StatusStarting}, "fetching")
	starting := backends(map[string]string{"m/0": meshapi.StatusStarting}, "")
	c.Backends = starting
	c.Step() // deadline = t0 + 10m
	c.Backends = fetching
	clock.t = clock.t.Add(5 * time.Minute)
	c.Step() // 5 minutes of fetching: deadline moves to t0 + 15m
	c.Backends = starting
	clock.t = clock.t.Add(7 * time.Minute) // t0 + 12m
	if c.Step() || *restarts != 0 {
		t.Fatal("rolled back although fetching time does not count")
	}
	clock.t = clock.t.Add(4 * time.Minute) // t0 + 16m
	if !c.Step() || *restarts != 1 {
		t.Fatal("did not roll back after the deadline")
	}
	if s, _ := LoadState(dir); s.Current != "v2.5.0" {
		t.Errorf("state = %+v", s)
	}
}

func TestNothingToConfirm(t *testing.T) {
	_, c, _, restarts := pendingState(t)
	c.Backends = backends(nil, "")
	if !c.Step() || *restarts != 0 {
		t.Error("an empty baseline confirms at once")
	}
	dir, c2, _, _ := pendingState(t, "m/0")
	c2.Running = "v2.5.0" // this process is not the pending release
	c2.Backends = backends(map[string]string{"m/0": meshapi.StatusDead}, "")
	if !c2.Step() {
		t.Error("kept watching a release it is not running")
	}
	if s, _ := LoadState(dir); s.Pending == nil {
		t.Error("changed a state it does not own")
	}
}
