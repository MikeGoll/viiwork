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

// v2.7.0-beta3: before the stickiness band, a draw at the very bottom could
// land on a local host twice as slow as the peer (pred 8100 vs 4101 ms). That
// host is now outside the band (> 1.25x the best) and is never drawn.
func TestScoredNeverDrawsAHostOutsideTheBand(t *testing.T) {
	local := newFakeLocal()
	local.add("m", newFakeBackend("m/0", 2))
	reps := &fakeReports{reports: []capacity.Report{report("fast", t0, time.Millisecond, scoredModel("m", 2, 0, 100, 1000))}}
	for _, draw := range []float64{0, 0.001, 0.5, 0.999} {
		r := newScored(local, reps, func(string) (int, int, bool) { return 100, 2000, true }, seq(draw))
		l, err := r.Pick(Request{Model: "m", EstK: 4})
		if err != nil {
			t.Fatal(err)
		}
		if l.Target().Local || l.Target().Node != "fast" {
			t.Fatalf("draw %v picked %+v; local pred 8100 ms is beyond 1.25x fast's 4101 ms", draw, l.Target())
		}
		l.Release()
	}
}

func TestScoredKeepsLocalWithinTheBand(t *testing.T) {
	local := newFakeLocal()
	local.add("m", newFakeBackend("m/0", 2))
	reps := &fakeReports{reports: []capacity.Report{report("fast", t0, time.Millisecond, scoredModel("m", 2, 0, 100, 1000))}}
	// local pred 100+1200*4 = 4900 ms, 1.19x the peer's 4101 ms. No session:
	// a request with one follows its session's host instead (below).
	for _, draw := range []float64{0, 0.5, 0.999} {
		r := newScored(local, reps, func(string) (int, int, bool) { return 100, 1200, true }, seq(draw))
		l, err := r.Pick(Request{Model: "m", EstK: 4})
		if err != nil {
			t.Fatal(err)
		}
		if !l.Target().Local {
			t.Fatalf("draw %v picked %+v; a local host within the band keeps its cache-warm request", draw, l.Target())
		}
		l.Release()
	}
}

func TestScoredWeightedDrawStaysInTheBand(t *testing.T) {
	local := newFakeLocal()
	reps := &fakeReports{reports: []capacity.Report{
		report("a", t0, time.Millisecond, scoredModel("m", 2, 0, 100, 1000)),
		report("b", t0, time.Millisecond, scoredModel("m", 2, 0, 100, 1050)),
		report("slow", t0, time.Millisecond, scoredModel("m", 2, 0, 100, 1500)), // 6101 ms > 1.25 x 4101
	}}
	seen := map[string]int{}
	for i := 0; i < 100; i++ {
		r := newScored(local, reps, noScore, seq(float64(i)/100))
		l, err := r.Pick(Request{Model: "m", EstK: 4})
		if err != nil {
			t.Fatal(err)
		}
		seen[l.Target().Node]++
		l.Release()
	}
	if seen["slow"] > 0 || seen["a"] == 0 || seen["b"] == 0 {
		t.Fatalf("picks %v; the draw covers the band (a, b) and never the host outside it", seen)
	}
}

func equalPeers(names ...string) *fakeReports {
	reps := &fakeReports{}
	for _, n := range names {
		reps.reports = append(reps.reports, report(n, t0, time.Millisecond, scoredModel("m", 2, 0, 100, 1000)))
	}
	return reps
}

func TestSessionKeyReturnsToTheSameHost(t *testing.T) {
	reps := equalPeers("gb1", "gb2", "gb3")
	var first string
	for i := 0; i < 20; i++ {
		// A different draw every time: the session, not the dice, decides.
		r := newScored(newFakeLocal(), reps, noScore, seq(float64(i)/20))
		l, err := r.Pick(Request{Model: "m", EstK: 4, SessionKey: 0xfeedface})
		if err != nil {
			t.Fatal(err)
		}
		if first == "" {
			first = l.Target().Node
		} else if l.Target().Node != first {
			t.Fatalf("pick %d went to %s, the session's earlier picks to %s", i, l.Target().Node, first)
		}
		l.Release()
	}
}

func TestSessionKeysSpreadAcrossTheBand(t *testing.T) {
	reps := equalPeers("gb1", "gb2", "gb3")
	r := newScored(newFakeLocal(), reps, noScore, seq(0.5))
	seen := map[string]int{}
	for k := uint64(1); k <= 64; k++ {
		l, err := r.Pick(Request{Model: "m", EstK: 4, SessionKey: k * 0x9e3779b97f4a7c15})
		if err != nil {
			t.Fatal(err)
		}
		seen[l.Target().Node]++
		l.Release()
	}
	for _, n := range []string{"gb1", "gb2", "gb3"} {
		if seen[n] == 0 {
			t.Fatalf("spread %v; 64 sessions over three equal hosts must reach every one", seen)
		}
	}
}

func TestSessionMovesWhenItsHostIsFullAndReturns(t *testing.T) {
	reps := equalPeers("gb1", "gb2", "gb3")
	r := newScored(newFakeLocal(), reps, noScore, seq(0.5))
	req := Request{Model: "m", EstK: 4, SessionKey: 7}
	l, _ := r.Pick(req)
	home := l.Target().Node
	l.Release()

	full := &fakeReports{}
	for _, rep := range reps.reports {
		if rep.Node == home {
			rep = report(home, t0, time.Millisecond, scoredModel("m", 2, 2, 100, 1000))
		}
		full.reports = append(full.reports, rep)
	}
	r = newScored(newFakeLocal(), full, noScore, seq(0.5))
	l, err := r.Pick(req)
	if err != nil {
		t.Fatal(err)
	}
	if l.Target().Node == home {
		t.Fatalf("picked the full host %s", home)
	}
	l.Release()

	r = newScored(newFakeLocal(), reps, noScore, seq(0.5))
	l, _ = r.Pick(req)
	if l.Target().Node != home {
		t.Fatalf("picked %s; once %s frees up the session returns to it", l.Target().Node, home)
	}
}

func TestSessionRespectsExclude(t *testing.T) {
	reps := equalPeers("gb1", "gb2")
	r := newScored(newFakeLocal(), reps, noScore, seq(0.5))
	req := Request{Model: "m", EstK: 4, SessionKey: 99}
	l, _ := r.Pick(req)
	home := l.Target()
	l.Release()
	req.Exclude = map[string]bool{home.Key(): true}
	l, err := r.Pick(req)
	if err != nil || l.Target().Node == home.Node {
		t.Fatalf("got %+v %v; a retry excludes the host that failed, session or not", l, err)
	}
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

// sessionKeyFavouring returns a session key whose rendezvous ranks want above
// every other node in others.
func sessionKeyFavouring(t *testing.T, want string, others ...string) uint64 {
	t.Helper()
	for k := uint64(1); k < 1000; k++ {
		key := k * 0x9e3779b97f4a7c15
		top := true
		for _, o := range others {
			if rendezvous(key, o) > rendezvous(key, want) {
				top = false
			}
		}
		if top {
			return key
		}
	}
	t.Fatalf("no key favours %s", want)
	return 0
}

// 2026-10-03, gb2 + gb3: sixteen sessions entered through gb2. Local-first
// came before the session's host, so every free gb2 slot pulled back a
// session whose cache was on gb3, cold, and 12% of turns crossed hosts. A
// session goes to its own host in the band, this node included.
func TestSessionHostWinsOverLocalInTheBand(t *testing.T) {
	local := newFakeLocal()
	local.add("m", newFakeBackend("m/0", 2))
	reps := equalPeers("gb3")
	same := func(string) (int, int, bool) { return 100, 1000, true }

	away := sessionKeyFavouring(t, "gb3", "self")
	r := newScored(local, reps, same, seq(0.5))
	l, err := r.Pick(Request{Model: "m", EstK: 4, SessionKey: away})
	if err != nil {
		t.Fatal(err)
	}
	if l.Target().Local || l.Target().Node != "gb3" {
		t.Fatalf("picked %+v; the session's host is gb3 and it has a free slot", l.Target())
	}
	l.Release()

	home := sessionKeyFavouring(t, "self", "gb3")
	l, err = r.Pick(Request{Model: "m", EstK: 4, SessionKey: home})
	if err != nil {
		t.Fatal(err)
	}
	if !l.Target().Local {
		t.Fatalf("picked %+v; the session's host is this node", l.Target())
	}
	l.Release()
}

// 2026-10-03, beta5 on gb2 + gb3: after a flag change neither host had a score,
// so routing fell back to local-first and 22% of turns crossed hosts in the
// first five minutes. A session follows its host with no scores too.
func TestSessionHostWithoutScores(t *testing.T) {
	local := newFakeLocal()
	local.add("m", newFakeBackend("m/0", 2))
	reps := &fakeReports{reports: []capacity.Report{report("gb3", t0, time.Millisecond, meshapi.ModelCapacity{Name: "m", Slots: 2, HealthyBackends: 1})}}
	r := newScored(local, reps, noScore, seq(0.5))

	away := sessionKeyFavouring(t, "gb3", "self")
	l, err := r.Pick(Request{Model: "m", EstK: 4, SessionKey: away})
	if err != nil {
		t.Fatal(err)
	}
	if l.Target().Local || l.Target().Node != "gb3" {
		t.Fatalf("picked %+v; with no scores the session still goes to its host gb3", l.Target())
	}
	l.Release()

	home := sessionKeyFavouring(t, "self", "gb3")
	l, err = r.Pick(Request{Model: "m", EstK: 4, SessionKey: home})
	if err != nil || !l.Target().Local {
		t.Fatalf("got %+v %v; the session's host is this node", l, err)
	}
	l.Release()

	// Its host full, the session takes the other one rather than waiting.
	full := &fakeReports{reports: []capacity.Report{report("gb3", t0, time.Millisecond, meshapi.ModelCapacity{Name: "m", Slots: 2, Busy: 2, HealthyBackends: 1})}}
	r = newScored(local, full, noScore, seq(0.5))
	l, err = r.Pick(Request{Model: "m", EstK: 4, SessionKey: away})
	if err != nil || !l.Target().Local {
		t.Fatalf("got %+v %v; with gb3 full the session is served here", l, err)
	}
}

// routing.performance false is the old routing exactly, session or not.
func TestPerformanceOffIgnoresTheSession(t *testing.T) {
	local := newFakeLocal()
	local.add("m", newFakeBackend("m/0", 2))
	reps := &fakeReports{reports: []capacity.Report{report("gb3", t0, time.Millisecond, meshapi.ModelCapacity{Name: "m", Slots: 2, HealthyBackends: 1})}}
	r := New(Config{Self: "self", Local: local, Remote: reps, StaleAfter: time.Hour, QueueMax: 8, QueueTimeout: time.Second, Now: func() time.Time { return t0 }})
	l, _ := r.Pick(Request{Model: "m", EstK: 4, SessionKey: sessionKeyFavouring(t, "gb3", "self")})
	if !l.Target().Local {
		t.Fatal("routing.performance false keeps local-first")
	}
}
