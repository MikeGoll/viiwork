package route

import (
	"testing"
	"time"

	"github.com/janit/viiwork/v2/mesh/capacity"
	"github.com/janit/viiwork/v2/meshapi"
)

// seq returns the given values in turn, then repeats the last.
func seq(vals ...float64) func() float64 {
	i := 0
	return func() float64 {
		v := vals[min(i, len(vals)-1)]
		i++
		return v
	}
}

func scoredModel(name string, slots, busy, ov, rate int) meshapi.ModelCapacity {
	return meshapi.ModelCapacity{Name: name, Slots: slots, Busy: busy, HealthyBackends: 1, TTFTOverheadMs: ov, PrefillMsPer1k: rate}
}

func newScored(local *fakeLocal, reps *fakeReports, localScore func(string) (int, int, bool), rnd func() float64) *Router {
	return New(Config{Self: "self", Local: local, Remote: reps, StaleAfter: time.Hour, QueueMax: 8, QueueTimeout: time.Second,
		Now: func() time.Time { return t0 }, Performance: true, LocalScore: localScore, Rand: rnd})
}

func noScore(string) (int, int, bool) { return 0, 0, false }

func TestScoredPrefersTheFasterPeerOverLocal(t *testing.T) {
	local := newFakeLocal()
	local.add("m", newFakeBackend("m/0", 2))
	reps := &fakeReports{reports: []capacity.Report{report("fast", t0, time.Millisecond, scoredModel("m", 2, 0, 100, 1000))}}
	slowLocal := func(string) (int, int, bool) { return 100, 5000, true }
	// Draw 0.9 skips the trickle (no learning host anyway); 0.5 picks by weight.
	r := newScored(local, reps, slowLocal, seq(0.5))
	l, err := r.Pick(Request{Model: "m", EstK: 4})
	if err != nil {
		t.Fatal(err)
	}
	if l.Target().Local || l.Target().Node != "fast" {
		t.Fatalf("picked %+v; pred local 20100 ms vs fast 4100 ms: weight ratio ~118:1", l.Target())
	}
	l.Release()
}

func TestScoredCanStillPickTheSlowerHost(t *testing.T) {
	local := newFakeLocal()
	local.add("m", newFakeBackend("m/0", 2))
	reps := &fakeReports{reports: []capacity.Report{report("fast", t0, time.Millisecond, scoredModel("m", 2, 0, 100, 1000))}}
	r := newScored(local, reps, func(string) (int, int, bool) { return 100, 2000, true }, seq(0.001))
	// local first in the candidate list; a draw at the very bottom lands on it.
	l, err := r.Pick(Request{Model: "m", EstK: 4})
	if err != nil {
		t.Fatal(err)
	}
	if !l.Target().Local {
		t.Fatalf("picked %+v, want local: weighted random keeps slower hosts measured", l.Target())
	}
	l.Release()
}

func TestNoScoresAnywhereIsTodaysRouting(t *testing.T) {
	local := newFakeLocal()
	local.add("m", newFakeBackend("m/0", 1))
	reps := &fakeReports{reports: []capacity.Report{report("p", t0, time.Millisecond, meshapi.ModelCapacity{Name: "m", Slots: 8, HealthyBackends: 1})}}
	r := newScored(local, reps, noScore, seq(0.99))
	l, err := r.Pick(Request{Model: "m", EstK: 4})
	if err != nil || !l.Target().Local {
		t.Fatalf("got %+v %v; with no scores the local backend comes first, as today", l, err)
	}
}

// Review Focus 3: a peer one version behind publishes no score. It is learning,
// priced at the fleet median, never as a free host.
func TestUnscoredPeerIsPricedAtTheMedianNotZero(t *testing.T) {
	local := newFakeLocal()
	reps := &fakeReports{reports: []capacity.Report{
		report("scored", t0, time.Millisecond, scoredModel("m", 2, 0, 100, 1000)),
		report("old", t0, time.Millisecond, meshapi.ModelCapacity{Name: "m", Slots: 2, HealthyBackends: 1}),
	}}
	// 0.9 skips the trickle; 0.4 lands in the first half of equal weights.
	r := newScored(local, reps, noScore, seq(0.9, 0.4))
	l, _ := r.Pick(Request{Model: "m", EstK: 4})
	if l.Target().Node != "scored" {
		t.Fatalf("picked %s; equal predictions split 50/50 and draw 0.4 is the first", l.Target().Node)
	}
	l.Release()
	r = newScored(local, reps, noScore, seq(0.9, 0.6))
	l, _ = r.Pick(Request{Model: "m", EstK: 4})
	if l.Target().Node != "old" {
		t.Fatalf("picked %s; draw 0.6 is the second of two equal weights", l.Target().Node)
	}
}

func TestTrickleGoesToALearningHost(t *testing.T) {
	local := newFakeLocal()
	reps := &fakeReports{reports: []capacity.Report{
		report("fast", t0, time.Millisecond, scoredModel("m", 2, 0, 100, 1000)),
		report("new", t0, time.Millisecond, meshapi.ModelCapacity{Name: "m", Slots: 2, HealthyBackends: 1}),
	}}
	r := newScored(local, reps, noScore, seq(0.01, 0.0)) // 0.01 < 0.05: trickle
	l, _ := r.Pick(Request{Model: "m", EstK: 4})
	if l.Target().Node != "new" {
		t.Fatalf("picked %s, want the learning host", l.Target().Node)
	}
}

func TestScoredRespectsExistingFilters(t *testing.T) {
	local := newFakeLocal()
	reps := &fakeReports{reports: []capacity.Report{
		report("full", t0, time.Millisecond, scoredModel("m", 2, 2, 10, 10)),
		report("stale", t0.Add(-2*time.Hour), time.Millisecond, scoredModel("m", 2, 0, 10, 10)),
		report("ok", t0, time.Millisecond, scoredModel("m", 2, 0, 100, 5000)),
	}}
	r := newScored(local, reps, noScore, seq(0.9, 0.0))
	l, err := r.Pick(Request{Model: "m", EstK: 4})
	if err != nil || l.Target().Node != "ok" {
		t.Fatalf("got %+v %v; full and stale hosts are never candidates, however fast", l, err)
	}
	l.Refused()
	l.Release()
	if _, err := r.Pick(Request{Model: "m", EstK: 4}); err == nil {
		t.Fatal("a refused peer stays out until a newer report")
	}
}

func TestScoredIgnoredForPinsAndForwards(t *testing.T) {
	local := newFakeLocal()
	local.add("m", newFakeBackend("m/0", 2))
	reps := &fakeReports{reports: []capacity.Report{report("fast", t0, time.Millisecond, scoredModel("m", 2, 0, 1, 1))}}
	r := newScored(local, reps, func(string) (int, int, bool) { return 100, 9000, true }, seq(0.5))
	l, _ := r.Pick(Request{Model: "m", EstK: 4, Host: "self"})
	if !l.Target().Local {
		t.Fatal("a pin to this node must be honoured")
	}
	l.Release()
	l, _ = r.Pick(Request{Model: "m", EstK: 4, Forwarded: true})
	if !l.Target().Local {
		t.Fatal("a forward is admitted locally or refused, never re-routed")
	}
}

func TestPerformanceOffIsTodaysRouting(t *testing.T) {
	local := newFakeLocal()
	local.add("m", newFakeBackend("m/0", 2))
	reps := &fakeReports{reports: []capacity.Report{report("fast", t0, time.Millisecond, scoredModel("m", 2, 0, 1, 1))}}
	r := New(Config{Self: "self", Local: local, Remote: reps, StaleAfter: time.Hour, QueueMax: 8, QueueTimeout: time.Second, Now: func() time.Time { return t0 }})
	l, _ := r.Pick(Request{Model: "m", EstK: 4})
	if !l.Target().Local {
		t.Fatal("routing.performance false keeps local-first")
	}
}
