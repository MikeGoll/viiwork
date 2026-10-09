package route

import (
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/supervisor"
	"github.com/janit/viiwork/v2/meshapi"
)

func scoredCap(slots, busy, overheadMs, msPer1k int) meshapi.ModelCapacity {
	m := capM(slots, busy, 1)
	m.TTFTOverheadMs, m.PrefillMsPer1k = overheadMs, msPer1k
	return m
}

// ?prefer= is an ordered wish, not a pin: the first named host with a free
// slot takes the request, and with none free the request routes as if the
// list were absent.
func TestRouterPrefer(t *testing.T) {
	node := func(t *testing.T, l *Lease) string {
		t.Helper()
		defer l.Release()
		return l.Target().Node
	}

	t.Run("a preferred peer before this node", func(t *testing.T) {
		f := newFixture()
		f.local.add("m", newFakeBackend("m/0", 1))
		f.reports.set(report("A", t0, time.Millisecond, capM(1, 0, 1)), report("B", t0, time.Millisecond, capM(5, 0, 1)))
		if got := node(t, f.pick(t, Request{Prefer: []string{"A", "B"}})); got != "A" {
			t.Errorf("node = %q, want A", got)
		}
	})

	t.Run("list order, not most free", func(t *testing.T) {
		f := newFixture()
		f.reports.set(report("A", t0, time.Millisecond, capM(5, 0, 1)), report("B", t0, time.Millisecond, capM(1, 0, 1)))
		if got := node(t, f.pick(t, Request{Prefer: []string{"B", "A"}})); got != "B" {
			t.Errorf("node = %q, want B", got)
		}
	})

	t.Run("a full host is skipped for the next name", func(t *testing.T) {
		f := newFixture()
		f.local.add("m", newFakeBackend("m/0", 1))
		f.reports.set(report("A", t0, time.Millisecond, capM(1, 1, 1)), report("B", t0, time.Millisecond, capM(1, 0, 1)))
		if got := node(t, f.pick(t, Request{Prefer: []string{"A", "B"}})); got != "B" {
			t.Errorf("node = %q, want B", got)
		}
	})

	t.Run("this node when it is named", func(t *testing.T) {
		f := newFixture()
		f.local.add("m", newFakeBackend("m/0", 1))
		f.reports.set(report("A", t0, time.Millisecond, capM(1, 1, 1)), report("B", t0, time.Millisecond, capM(5, 0, 1)))
		l := f.pick(t, Request{Prefer: []string{"A", "self", "B"}})
		defer l.Release()
		if !l.Target().Local {
			t.Errorf("target = %+v, want local", l.Target())
		}
	})

	t.Run("this node named but full", func(t *testing.T) {
		f := newFixture()
		b := newFakeBackend("m/0", 1)
		b.Acquire()
		f.local.add("m", b)
		f.reports.set(report("B", t0, time.Millisecond, capM(1, 0, 1)))
		if got := node(t, f.pick(t, Request{Prefer: []string{"self", "B"}})); got != "B" {
			t.Errorf("node = %q, want B", got)
		}
	})

	t.Run("unknown, stale and unhealthy names are skipped", func(t *testing.T) {
		f := newFixture()
		f.reports.set(
			report("stale", t0.Add(-time.Minute), time.Millisecond, capM(5, 0, 1)),
			report("sick", t0, time.Millisecond, capM(5, 0, 0)),
			report("other", t0, time.Millisecond, meshapi.ModelCapacity{Name: "x", Slots: 5, Backends: 1, HealthyBackends: 1}),
			report("B", t0, time.Millisecond, capM(1, 0, 1)),
		)
		if got := node(t, f.pick(t, Request{Prefer: []string{"nosuch", "stale", "sick", "other", "B"}})); got != "B" {
			t.Errorf("node = %q, want B", got)
		}
	})

	t.Run("none free routes as without the list", func(t *testing.T) {
		f := newFixture()
		f.local.add("m", newFakeBackend("m/0", 1))
		f.reports.set(report("A", t0, time.Millisecond, capM(1, 1, 1)), report("B", t0, time.Millisecond, capM(5, 0, 1)))
		l := f.pick(t, Request{Prefer: []string{"A", "nosuch"}})
		defer l.Release()
		if !l.Target().Local {
			t.Errorf("target = %+v, want the local-first pick", l.Target())
		}
	})

	t.Run("nothing free anywhere is no free slot, never host not serving", func(t *testing.T) {
		f := newFixture()
		f.reports.set(report("A", t0, time.Millisecond, capM(1, 1, 1)))
		if err := f.pickErr(Request{Prefer: []string{"nosuch", "A"}}); err != ErrNoFreeSlot {
			t.Errorf("err = %v, want ErrNoFreeSlot", err)
		}
	})

	t.Run("a refused peer is skipped until a newer report", func(t *testing.T) {
		f := newFixture()
		f.reports.set(report("A", t0, time.Millisecond, capM(2, 0, 1)), report("B", t0, time.Millisecond, capM(2, 0, 1)))
		l := f.pick(t, Request{Prefer: []string{"A", "B"}})
		l.Refused()
		l.Release()
		if got := node(t, f.pick(t, Request{Prefer: []string{"A", "B"}})); got != "B" {
			t.Errorf("node = %q, want B after A refused", got)
		}
	})

	t.Run("reservations count against a preferred peer", func(t *testing.T) {
		f := newFixture()
		f.reports.set(report("A", t0, time.Millisecond, capM(1, 0, 1)), report("B", t0, time.Millisecond, capM(1, 0, 1)))
		f.clock.set(t0.Add(time.Second))
		first := f.pick(t, Request{Prefer: []string{"A", "B"}})
		defer first.Release()
		if got := node(t, f.pick(t, Request{Prefer: []string{"A", "B"}})); first.Target().Node != "A" || got != "B" {
			t.Errorf("first = %q, second = %q, want A then B", first.Target().Node, got)
		}
	})

	t.Run("a retry's exclusion holds", func(t *testing.T) {
		f := newFixture()
		f.reports.set(report("A", t0, time.Millisecond, capM(2, 0, 1)), report("B", t0, time.Millisecond, capM(2, 0, 1)))
		if got := node(t, f.pick(t, Request{Prefer: []string{"A", "B"}, Exclude: map[string]bool{"peer:A": true}})); got != "B" {
			t.Errorf("node = %q, want B", got)
		}
	})

	t.Run("the pin beats the list", func(t *testing.T) {
		f := newFixture()
		f.reports.set(report("A", t0, time.Millisecond, capM(2, 0, 1)), report("B", t0, time.Millisecond, capM(2, 0, 1)))
		if got := node(t, f.pick(t, Request{Host: "B", Prefer: []string{"A"}})); got != "B" {
			t.Errorf("node = %q, want the pinned B", got)
		}
	})

	t.Run("a forward ignores the list", func(t *testing.T) {
		f := newFixture()
		f.local.add("m", newFakeBackend("m/0", 1))
		f.reports.set(report("A", t0, time.Millisecond, capM(2, 0, 1)))
		l := f.pick(t, Request{Forwarded: true, Prefer: []string{"A"}})
		defer l.Release()
		if !l.Target().Local {
			t.Errorf("target = %+v, want local", l.Target())
		}
	})

	t.Run("a draining local model is not preferred", func(t *testing.T) {
		f := newFixture()
		b := newFakeBackend("m/0", 1)
		b.set(supervisor.StateUnhealthy)
		f.local.add("m", b)
		f.reports.set(report("B", t0, time.Millisecond, capM(1, 0, 1)))
		if got := node(t, f.pick(t, Request{Prefer: []string{"self", "B"}})); got != "B" {
			t.Errorf("node = %q, want B", got)
		}
	})
}

// With scores, the list still comes first: a preferred host is taken though
// its own score says another is far faster, and though the session's
// rendezvous host is another.
func TestPreferBeatsScoreAndSession(t *testing.T) {
	f := newFixture()
	f.router = New(Config{Self: "self", Local: f.local, Remote: f.reports, StaleAfter: 3 * time.Second, QueueMax: 4, QueueTimeout: time.Second,
		Now: f.clock.Now, Performance: true, Rand: func() float64 { return 0.5 }})
	f.reports.set(
		report("slow", t0, time.Millisecond, scoredCap(4, 0, 5000, 9000)),
		report("fast1", t0, time.Millisecond, scoredCap(4, 0, 100, 1000)),
		report("fast2", t0, time.Millisecond, scoredCap(4, 0, 100, 1000)),
	)
	for key := uint64(1); key <= 20; key++ {
		l := f.pick(t, Request{EstK: 10, SessionKey: key, Prefer: []string{"slow"}})
		if l.Target().Node != "slow" {
			t.Fatalf("session %d went to %q, want the preferred host", key, l.Target().Node)
		}
		l.Release()
	}
	// The same requests without the list never reach it: the band excludes it.
	for key := uint64(1); key <= 20; key++ {
		l := f.pick(t, Request{EstK: 10, SessionKey: key})
		if l.Target().Node == "slow" {
			t.Fatalf("session %d went to the slow host without a preference", key)
		}
		l.Release()
	}
}

// A queued request walks its list again when a slot frees.
func TestQueuedRequestKeepsItsPreference(t *testing.T) {
	f := newFixture()
	b := newFakeBackend("m/0", 1)
	f.local.add("m", b)
	held := f.pick(t, Request{})
	f.reports.set(report("A", t0, time.Millisecond, capM(1, 1, 1)))
	ctx := t.Context()
	go f.router.Run(ctx)
	got := make(chan *Lease, 1)
	go func() {
		l, _, err := f.router.Acquire(ctx, Request{Model: "m", Prefer: []string{"A"}})
		if err != nil {
			t.Error(err)
		}
		got <- l
	}()
	deadline := time.Now().Add(2 * time.Second)
	for f.router.QueueLen("m") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the request never queued")
		}
		time.Sleep(time.Millisecond)
	}
	// Both free at once: the preferred peer wins over this node.
	f.reports.set(report("A", t0.Add(time.Second), time.Millisecond, capM(1, 0, 1)))
	f.clock.set(t0.Add(time.Second))
	held.Release()
	select {
	case l := <-got:
		defer l.Release()
		if l.Target().Node != "A" {
			t.Errorf("node = %q, want A", l.Target().Node)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the queued request was never served")
	}
}
