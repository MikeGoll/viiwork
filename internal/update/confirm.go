package update

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

// Confirmer is the node running a pending release deciding whether to keep
// it. The baseline is the backends that were healthy when the release was
// activated: once every one of them is healthy again the release becomes last
// good. A backend that was already down neither blocks confirmation nor
// counts against the release — or a host with one failed card could never
// confirm anything. A baseline backend going dead, or the window passing,
// returns the node to its last good release.
type Confirmer struct {
	Store    *Store
	Running  string
	Window   time.Duration // config.ConfirmWindow
	Backends func() []meshapi.ModelStatus
	Restart  func()
	Now      func() time.Time
	Log      func(format string, args ...any)
	// PruneEngines removes engine builds no release kept in the confirmed
	// state is pinned to; nil removes none. It runs after the releases are
	// pruned, and only on a confirm: a rollback must find its engine there.
	PruneEngines func(confirmed State)
	// Managed: a rollback saves the state and leaves the restart to the
	// host's engine helper, which swaps the last good image back in.
	Managed bool

	mu       sync.Mutex
	deadline time.Time
	last     time.Time
	lastErr  string // the last state read error logged
}

// Step checks once and reports whether the confirmer is finished.
func (c *Confirmer) Step() bool {
	st, err := c.Store.Load()
	if err != nil {
		// Keep polling: a hand edit or a transient read error must not
		// leave a pending release neither confirmed nor rolled back for the
		// rest of the run. Each distinct error is logged once.
		if msg := err.Error(); msg != c.lastErr {
			c.Log("update: %v", err)
			c.lastErr = msg
		}
		return false
	}
	c.lastErr = ""
	p := st.Pending
	if p == nil || p.Version != st.Current || p.Version != c.Running {
		return true
	}
	now := c.Now()
	byID := map[string]meshapi.BackendStatus{}
	fetching := false
	for _, m := range c.Backends() {
		for _, b := range m.Backends {
			byID[b.ID] = b
			if b.Phase == "fetching" {
				fetching = true
			}
		}
	}
	c.mu.Lock()
	if c.deadline.IsZero() {
		c.deadline, c.last = now.Add(c.Window), now
	}
	// Time a backend spends downloading its weights from viiwork-parrot does
	// not count, as it does not count against startup_timeout.
	if fetching {
		c.deadline = c.deadline.Add(now.Sub(c.last))
	}
	c.last = now
	deadline := c.deadline
	c.mu.Unlock()

	healthy := true
	for _, id := range p.Baseline {
		b, ok := byID[id]
		switch {
		case ok && b.Status == meshapi.StatusDead:
			c.rollback("backend " + id + " is dead")
			return true
		case !ok || b.Status != meshapi.StatusHealthy:
			healthy = false
		}
	}
	if healthy {
		var confirmed State
		err := c.Store.Update(func(st *State) (bool, error) {
			if !c.owns(st) {
				return false, errNotOwned
			}
			// The release it replaces as last good becomes the way back.
			if st.LastGood != st.Current {
				st.Previous = st.LastGood
			}
			st.LastGood, st.Pending = st.Current, nil
			confirmed = *st
			return false, nil
		})
		switch {
		case errors.Is(err, errNotOwned), errors.Is(err, ErrRestarting):
			return true // an operator's rollback or activate got there first
		case err != nil:
			c.Log("update: confirming %s: %v", c.Running, err)
			return true
		}
		if err := Prune(c.Store.Dir, confirmed); err != nil {
			c.Log("update: pruning staged releases: %v", err)
		}
		if c.PruneEngines != nil {
			c.PruneEngines(confirmed)
		}
		c.Log("update: %s confirmed: every backend healthy before the update is healthy again", c.Running)
		return true
	}
	if now.After(deadline) {
		c.rollback("not every backend healthy before the update was healthy again by " + deadline.Format(time.RFC3339))
		return true
	}
	return false
}

// errNotOwned is a state the confirmer no longer owns: the pending release
// changed between reading the backends and deciding.
var errNotOwned = errors.New("the pending release changed")

// owns reports whether st is still the pending release this process runs. It
// is checked again under the store's lock, because an operator's rollback or
// activate may have landed since the confirmer first looked.
func (c *Confirmer) owns(st *State) bool {
	p := st.Pending
	return p != nil && p.Version == st.Current && p.Version == c.Running
}

func (c *Confirmer) rollback(why string) {
	var to string
	err := c.Store.Update(func(st *State) (bool, error) {
		if !c.owns(st) {
			return false, errNotOwned
		}
		st.Current, st.Pending = st.LastGood, nil
		to = st.Current
		return !c.Managed, nil
	})
	switch {
	case errors.Is(err, errNotOwned), errors.Is(err, ErrRestarting):
		return
	case err != nil:
		c.Log("update: rolling back %s: %v", c.Running, err)
		return
	}
	if c.Managed {
		c.Log("update: rolling %s back to %s: %s; the host's engine helper swaps its image in", c.Running, to, why)
		return
	}
	c.Log("update: rolling %s back to %s: %s", c.Running, to, why)
	c.Restart()
}

// Run steps every interval until finished or ctx ends.
func (c *Confirmer) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for !c.Step() {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Deadline is when a pending release rolls back, once the confirmer has
// started watching it.
func (c *Confirmer) Deadline() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deadline, !c.deadline.IsZero()
}
