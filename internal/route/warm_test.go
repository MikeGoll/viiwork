package route

import (
	"slices"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/mesh/capacity"
	"github.com/janit/viiwork/v2/meshapi"
)

// keyRanking returns a session key that SessionRank orders as names.
func keyRanking(t *testing.T, names ...string) uint64 {
	t.Helper()
	for k := uint64(1); k < 100000; k++ {
		key := k * 0x9e3779b97f4a7c15
		if slices.Equal(SessionRank(key, names), names) {
			return key
		}
	}
	t.Fatalf("no key ranks %v", names)
	return 0
}

func grouped(mc meshapi.ModelCapacity, group string, importMs int) meshapi.ModelCapacity {
	mc.KVShareGroup, mc.KVShareImportMsPer1k = group, importMs
	return mc
}

// warmFleet: a and c are fast, b is four times slower at a cold prefill.
// The session ranks a, b, c: a is its home and b its replica host.
func warmFleet(group string, aBusy int) *fakeReports {
	return &fakeReports{reports: []capacity.Report{
		report("a", t0, time.Millisecond, grouped(scoredModel("m", 1, aBusy, 100, 1000), group, 0)),
		report("b", t0, time.Millisecond, grouped(scoredModel("m", 1, 0, 100, 4000), group, 40)),
		report("c", t0, time.Millisecond, grouped(scoredModel("m", 1, 0, 100, 1000), group, 0)),
	}}
}

func pickNode(t *testing.T, r *Router, req Request) string {
	t.Helper()
	l, err := r.Pick(req)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if l.Target().Local {
		return "self"
	}
	return l.Target().Node
}

// The case kv_share exists for: the session's home is full and its replica
// is on a host that is slow at a cold prefill. Priced cold, the spill goes
// to the fast host c and prefills 50k tokens; priced warm, it goes to b,
// which imports the replica.
func TestSpillGoesToTheReplicaHost(t *testing.T) {
	key := keyRanking(t, "a", "b", "c")
	reps := warmFleet("g", 0)
	r := newScored(newFakeLocal(), reps, noScore, seq(0.5))
	req := Request{Model: "m", EstK: 50, SessionKey: key}
	if got := pickNode(t, r, req); got != "a" {
		t.Fatalf("turn 1 went to %s, want the session's home a", got)
	}
	*reps = *warmFleet("g", 1) // a is full
	if got := pickNode(t, r, req); got != "b" {
		t.Fatalf("the spill went to %s, want b, which holds the replica", got)
	}
	// b ran the last turn; a, free again, is b's replica host now
	*reps = *warmFleet("g", 0)
	if got := pickNode(t, r, req); got != "b" {
		t.Fatalf("the next turn went to %s, want b, where the session last ran", got)
	}
}

func TestNoGroupNoWarmPricing(t *testing.T) {
	key := keyRanking(t, "a", "b", "c")
	reps := warmFleet("", 0)
	r := newScored(newFakeLocal(), reps, noScore, seq(0.5))
	req := Request{Model: "m", EstK: 50, SessionKey: key}
	pickNode(t, r, req)
	*reps = *warmFleet("", 1)
	if got := pickNode(t, r, req); got != "c" {
		t.Fatalf("without kv_share the spill went to %s, want the fast c", got)
	}
}

func TestFirstTurnIsPricedCold(t *testing.T) {
	key := keyRanking(t, "a", "b", "c")
	r := newScored(newFakeLocal(), warmFleet("g", 1), noScore, seq(0.5))
	if got := pickNode(t, r, Request{Model: "m", EstK: 50, SessionKey: key}); got != "c" {
		t.Fatalf("a first turn with its home full went to %s: nothing is warm yet, want the fast c", got)
	}
}

// Another group's member never counts as the replica host: the sender places
// replicas within its own group.
func TestReplicaHostIsInTheLastHostsGroup(t *testing.T) {
	key := keyRanking(t, "a", "b", "c")
	reps := warmFleet("g", 0)
	reps.reports[1].Models[0].KVShareGroup = "other"
	r := newScored(newFakeLocal(), reps, noScore, seq(0.5))
	req := Request{Model: "m", EstK: 50, SessionKey: key}
	pickNode(t, r, req)
	reps.reports[0].Models[0].Busy = 1
	if got := pickNode(t, r, req); got != "c" {
		t.Fatalf("the spill went to %s, want c: b is in another group, c is a's replica host", got)
	}
}

// The trickle sends a learning host a share of cold requests; never a turn
// that a warm host has room for.
func TestNoTrickleAwayFromAWarmHost(t *testing.T) {
	key := keyRanking(t, "a", "b", "c", "new")
	reps := warmFleet("g", 0)
	reps.reports = append(reps.reports, report("new", t0, time.Millisecond, grouped(meshapi.ModelCapacity{Name: "m", Slots: 1, HealthyBackends: 1}, "g", 0)))
	r := newScored(newFakeLocal(), reps, noScore, seq(0.5))
	req := Request{Model: "m", EstK: 50, SessionKey: key}
	if got := pickNode(t, r, req); got != "a" {
		t.Fatalf("turn 1 went to %s, want a", got)
	}
	r.c.Rand = seq(0.0, 0.0) // would trickle
	got := pickNode(t, r, req)
	if got == "new" {
		t.Fatal("a warm session's turn trickled to a learning host")
	}
}

// Before any score exists, a session still goes to its last host, else to
// that host's replica host.
func TestSessionPickWithoutScoresFollowsTheState(t *testing.T) {
	key := keyRanking(t, "a", "b", "c")
	unscored := func(busyA int) *fakeReports {
		return &fakeReports{reports: []capacity.Report{
			report("a", t0, time.Millisecond, grouped(meshapi.ModelCapacity{Name: "m", Slots: 1, Busy: busyA, HealthyBackends: 1}, "g", 0)),
			report("b", t0, time.Millisecond, grouped(meshapi.ModelCapacity{Name: "m", Slots: 1, HealthyBackends: 1}, "g", 0)),
			report("c", t0, time.Millisecond, grouped(meshapi.ModelCapacity{Name: "m", Slots: 1, HealthyBackends: 1}, "g", 0)),
		}}
	}
	reps := unscored(0)
	r := newScored(newFakeLocal(), reps, noScore, seq(0.5))
	req := Request{Model: "m", EstK: 50, SessionKey: key}
	if got := pickNode(t, r, req); got != "a" {
		t.Fatalf("turn 1 went to %s", got)
	}
	*reps = *unscored(1)
	if got := pickNode(t, r, req); got != "b" {
		t.Fatalf("the spill went to %s, want b", got)
	}
}

func TestOnlySharedModelsAreRemembered(t *testing.T) {
	key := keyRanking(t, "a", "b", "c")
	r := newScored(newFakeLocal(), warmFleet("", 0), noScore, seq(0.5))
	pickNode(t, r, Request{Model: "m", EstK: 50, SessionKey: key})
	if _, ok := r.lastHosts.get(sessionRef{"m", key}); ok {
		t.Fatal("a model without kv_share was remembered")
	}
	r = newScored(newFakeLocal(), warmFleet("g", 0), noScore, seq(0.5))
	pickNode(t, r, Request{Model: "m", EstK: 50, SessionKey: key})
	if _, ok := r.lastHosts.get(sessionRef{"m", key}); !ok {
		t.Fatal("a kv_share model's session was not remembered")
	}
}

func TestLastHostsIsBounded(t *testing.T) {
	var l lastHosts
	for i := range sessionMemory + 10 {
		l.set(sessionRef{"m", uint64(i)}, "a")
	}
	if len(l.byRef) != sessionMemory || l.order.Len() != sessionMemory {
		t.Fatalf("%d remembered", len(l.byRef))
	}
	if _, ok := l.get(sessionRef{"m", 0}); ok {
		t.Fatal("the oldest session must go first")
	}
	if n, ok := l.get(sessionRef{"m", sessionMemory + 9}); !ok || n != "a" {
		t.Fatal("the newest session must stay")
	}
}

func TestWarmImportPrice(t *testing.T) {
	key := keyRanking(t, "a", "b", "c")
	reps := warmFleet("g", 0)
	r := newScored(newFakeLocal(), reps, noScore, seq(0.5))
	r.lastHosts.set(sessionRef{"m", key}, "a")
	w := r.warmLocked(Request{Model: "m", SessionKey: key}, false, reps.reports, t0)
	if !w.ok || w.last != "a" || w.replica != "b" || w.importMsPer1k != 40 {
		t.Fatalf("%+v; want a, b and b's own 40 ms per 1k", w)
	}
	reps.reports[1].Models[0].KVShareImportMsPer1k = 0
	if w := r.warmLocked(Request{Model: "m", SessionKey: key}, false, reps.reports, t0); w.importMsPer1k != defaultImportMsPer1k {
		t.Fatalf("no measurement anywhere: %v, want the default", w.importMsPer1k)
	}
}
