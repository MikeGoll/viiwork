package perf

import (
	"testing"
	"time"
)

// A changed key means the hardware or the command line changed: the samples
// in the window were measured under the old one and must go with the
// baseline, or the "new" score is the old host's for up to ten minutes.
func TestSetKeysChangeClearsTheWindow(t *testing.T) {
	c := &clock{t0}
	tr := New(c.now)
	tr.SetKeys(map[string]string{"m": "k1"})
	feed(tr, c, "m", 5, 2000, 0, 5000)
	if _, ok := tr.Score("m"); !ok {
		t.Fatal("no score before the key change")
	}
	tr.SetKeys(map[string]string{"m": "k2"})
	if s, ok := tr.Score("m"); ok {
		t.Fatalf("after a key change got %+v: the old window must not survive", s)
	}
}

// Long generations finish out of order: a sample from an older minute can
// arrive after a newer one. It must land in its own minute's place, so the
// window's prefix trim still expires it on time.
func TestOutOfOrderRecordsExpireOnTime(t *testing.T) {
	c := &clock{t0.Add(5 * time.Minute)}
	tr := New(c.now)
	tr.Record("m", t0.Add(5*time.Minute), 2000, 5*time.Second)
	tr.Record("m", t0.Add(2*time.Minute), 2000, 9*time.Second) // arrives late
	tr.Record("m", t0.Add(5*time.Minute), 2000, 5*time.Second)
	c.t = t0.Add(12*time.Minute + time.Second) // minute 2 is out of the window, minute 5 is not
	s, ok := tr.Score("m")
	if !ok || s.Samples != 2 || s.MsPer1k != 2500 {
		t.Errorf("score = %+v %v, want the two minute-5 samples only (2500 ms/1k)", s, ok)
	}
}

// The router asks for this node's score on every scored pick, under its own
// mutex: between records and within a minute, Score is a cache hit and costs
// no allocation.
func TestScoreCachedDoesNotAllocate(t *testing.T) {
	c := &clock{t0}
	tr := New(c.now)
	feed(tr, c, "m", 4, 100, 300, 5000)
	feed(tr, c, "m", 4, 4000, 300, 5000)
	want, _ := tr.Score("m")
	if n := testing.AllocsPerRun(100, func() {
		if s, _ := tr.Score("m"); s != want {
			t.Fatalf("cached score %+v, want %+v", s, want)
		}
	}); n != 0 {
		t.Errorf("Score allocates %v times per call between records", n)
	}
}

func TestScoreReflectsANewRecord(t *testing.T) {
	c := &clock{t0}
	tr := New(c.now)
	feed(tr, c, "m", 2, 2000, 0, 5000)
	if s, _ := tr.Score("m"); s.Samples != 2 {
		t.Fatalf("samples = %d, want 2", s.Samples)
	}
	feed(tr, c, "m", 1, 2000, 0, 2000)
	s, _ := tr.Score("m")
	if s.Samples != 3 || s.MsPer1k != 4000 {
		t.Errorf("after a record: %+v, want 3 samples at 4000", s)
	}
}

// No record for eleven minutes: the minute rollover alone must expire the
// window, even though nothing invalidated the cached score.
func TestScoreExpiresOnMinuteRollover(t *testing.T) {
	c := &clock{t0}
	tr := New(c.now)
	feed(tr, c, "m", 2, 2000, 0, 5000)
	if s, _ := tr.Score("m"); s.Samples != 2 {
		t.Fatalf("samples = %d, want 2", s.Samples)
	}
	c.t = c.t.Add(10*time.Minute + time.Second)
	if s, ok := tr.Score("m"); ok {
		t.Errorf("window expired without a baseline, got %+v", s)
	}
}

// A cached score still reports the baseline's age as of now.
func TestCachedScoreBaselineAgeTicks(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t0}
	tr := New(c.now)
	tr.SetKeys(map[string]string{"m": "k"})
	feed(tr, c, "m", 5, 2000, 0, 5000)
	tr.Score("m")
	if err := tr.Save(dir); err != nil {
		t.Fatal(err)
	}
	c2 := &clock{t0.Add(time.Hour)}
	back := New(c2.now)
	if err := back.Load(dir); err != nil {
		t.Fatal(err)
	}
	back.SetKeys(map[string]string{"m": "k"})
	a, _ := back.Score("m")
	c2.t = c2.t.Add(30 * time.Second)
	b, _ := back.Score("m")
	if a.BaselineAge != time.Hour || b.BaselineAge != time.Hour+30*time.Second {
		t.Errorf("baseline ages %v then %v, want 1h then 1h30s", a.BaselineAge, b.BaselineAge)
	}
}
