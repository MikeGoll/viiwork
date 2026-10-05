package perf

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// feed records n samples at the clock's current time: uncached tokens and TTFT
// follow overhead + rate*uncached/1000 exactly.
func feed(tr *Tracker, c *clock, model string, n int, uncached int64, overheadMs, msPer1k float64) {
	ttft := overheadMs + msPer1k*float64(uncached)/1000
	for i := 0; i < n; i++ {
		tr.Record(model, c.t, uncached, time.Duration(ttft*float64(time.Millisecond)))
	}
}

func TestNoSamplesNoScore(t *testing.T) {
	c := &clock{t0}
	tr := New(c.now)
	if _, ok := tr.Score("m"); ok {
		t.Fatal("a model never measured must have no score: absent means cannot say")
	}
}

func TestSmallSamplesAloneGiveNoScore(t *testing.T) {
	c := &clock{t0}
	tr := New(c.now)
	feed(tr, c, "m", 10, 100, 300, 0)
	if _, ok := tr.Score("m"); ok {
		t.Fatal("without a sample of >= 512 tokens and no baseline, the rate is unknown; publishing a rate of 0 would make the host look infinitely fast")
	}
}

func TestWindowEstimate(t *testing.T) {
	c := &clock{t0}
	tr := New(c.now)
	feed(tr, c, "m", 3, 100, 400, 5000)  // small: overhead ~ 400 + 500
	feed(tr, c, "m", 4, 4000, 400, 5000) // big
	s, ok := tr.Score("m")
	if !ok {
		t.Fatal("no score")
	}
	if s.Samples != 7 {
		t.Errorf("samples = %d, want 7", s.Samples)
	}
	// The small samples' 100 tokens of prefill (500 ms) are not overhead: the
	// fit takes them off, so it recovers what feed put in, 400 and 5000.
	if s.OverheadMs != 400 || s.MsPer1k != 5000 {
		t.Errorf("score = %+v, want overhead 400, rate 5000", s)
	}
}

func TestBaselineRefreshAndBlend(t *testing.T) {
	c := &clock{t0}
	tr := New(c.now)
	feed(tr, c, "m", 5, 2000, 0, 5000)
	if s, _ := tr.Score("m"); !s.Baseline {
		t.Fatal("a window of 5 samples must become the baseline")
	}
	c.t = c.t.Add(11 * time.Minute) // the window empties
	s, ok := tr.Score("m")
	if !ok || s.Samples != 0 || s.MsPer1k != 5000 {
		t.Fatalf("empty window: got %+v %v, want the baseline alone (5000, samples 0)", s, ok)
	}
	// One fast sample moves the score by n/(n+k) = 1/6 of the way.
	feed(tr, c, "m", 1, 2000, 0, 2000)
	s, _ = tr.Score("m")
	if s.MsPer1k != 4500 { // (1*2000 + 5*5000)/6 = 4500
		t.Errorf("blend: rate = %d, want 4500", s.MsPer1k)
	}
}

func TestBucketsExpireAfterTenMinutes(t *testing.T) {
	c := &clock{t0}
	tr := New(c.now)
	feed(tr, c, "m", 3, 2000, 0, 9000)
	c.t = c.t.Add(10*time.Minute + time.Second)
	feed(tr, c, "m", 3, 2000, 0, 1000)
	s, _ := tr.Score("m")
	if s.Samples != 3 {
		t.Errorf("samples = %d, want 3: the first minute's bucket is older than 10 minutes", s.Samples)
	}
}

func TestDegradedHostCaughtWithinTheWindow(t *testing.T) {
	c := &clock{t0}
	tr := New(c.now)
	for m := 0; m < 10; m++ { // ten busy minutes at 5000
		feed(tr, c, "m", 6, 2000, 0, 5000)
		tr.Score("m")
		c.t = c.t.Add(time.Minute)
	}
	for m := 0; m < 10; m++ { // it slows threefold
		feed(tr, c, "m", 6, 2000, 0, 15000)
		c.t = c.t.Add(time.Minute)
	}
	s, _ := tr.Score("m")
	if s.MsPer1k < 14000 {
		t.Errorf("after a full window of slow samples rate = %d, want ~15000", s.MsPer1k)
	}
}

func TestRounding(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want int
	}{{5123, 5100}, {15, 15}, {7, 7}, {0.2, 1}, {0, 1}, {98765, 99000}} {
		if got := round2(tc.in); got != tc.want {
			t.Errorf("round2(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestPersistRoundTripAndKeyChange(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t0}
	tr := New(c.now)
	tr.SetKeys(map[string]string{"m": "k1", "n": "k1"})
	feed(tr, c, "m", 5, 2000, 0, 5000)
	feed(tr, c, "n", 5, 2000, 0, 7000)
	tr.Score("m")
	tr.Score("n")
	if err := tr.Save(dir); err != nil {
		t.Fatal(err)
	}

	c2 := &clock{t0.Add(time.Hour)}
	back := New(c2.now)
	if err := back.Load(dir); err != nil {
		t.Fatal(err)
	}
	back.SetKeys(map[string]string{"m": "k1", "n": "k2"}) // n's hardware changed
	if s, ok := back.Score("m"); !ok || s.MsPer1k != 5000 || s.Samples != 0 {
		t.Errorf("m after restart = %+v %v, want the saved baseline", s, ok)
	}
	if _, ok := back.Score("n"); ok {
		t.Error("n's key changed, so its baseline must be dropped, not inherited")
	}
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	if err := New(time.Now).Load(t.TempDir()); err != nil {
		t.Fatalf("no file yet is the normal first start: %v", err)
	}
}

func TestLoadCorruptFileErrorsAndTrackerStillWorks(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &clock{t0}
	tr := New(c.now)
	if err := tr.Load(dir); err == nil {
		t.Fatal("a corrupt file must be reported so the node can log it")
	}
	feed(tr, c, "m", 1, 2000, 0, 5000)
	if _, ok := tr.Score("m"); !ok {
		t.Fatal("the tracker must keep working after a failed load")
	}
}

func TestSetKeysForgetsRemovedModels(t *testing.T) {
	c := &clock{t0}
	tr := New(c.now)
	tr.SetKeys(map[string]string{"m": "k"})
	feed(tr, c, "m", 5, 2000, 0, 5000)
	tr.SetKeys(map[string]string{})
	if _, ok := tr.Score("m"); ok {
		t.Fatal("a model no longer configured keeps no score")
	}
}

// 2026-10-03: gb2 and gb3, identical pairs, published 5.1 s and 2.1 s of
// overhead because their short prompts differed in size. Two hosts with the
// same overhead and rate publish the same score whatever their prompt mix.
func TestOverheadDoesNotCountSmallPromptPrefill(t *testing.T) {
	score := func(smallTokens int64) Score {
		c := &clock{t0}
		tr := New(c.now)
		feed(tr, c, "m", 6, smallTokens, 2000, 7000)
		feed(tr, c, "m", 6, 20000, 2000, 7000)
		s, ok := tr.Score("m")
		if !ok {
			t.Fatal("no score")
		}
		return s
	}
	a, b := score(50), score(450)
	if a.OverheadMs != 2000 || b.OverheadMs != 2000 || a.MsPer1k != 7000 || b.MsPer1k != 7000 {
		t.Errorf("50-token prompts give %+v, 450-token prompts %+v; both hosts are 2000 + 7000/1k", a, b)
	}
}

// 2026-10-03: one request on gb1 waited 130 s with a short uncached prompt,
// and with few samples it became gb1's overhead and then its baseline.
func TestOneSlowRequestDoesNotSetTheScore(t *testing.T) {
	c := &clock{t0}
	tr := New(c.now)
	feed(tr, c, "m", 5, 100, 4000, 6000)
	feed(tr, c, "m", 5, 20000, 4000, 6000)
	tr.Score("m")                   // the window becomes the baseline
	c.t = c.t.Add(11 * time.Minute) // the window empties; the baseline stays
	if s, _ := tr.Score("m"); s.OverheadMs != 4000 || s.MsPer1k != 6000 {
		t.Fatalf("baseline %+v, want 4000 + 6000/1k", s)
	}

	feed(tr, c, "m", 1, 100, 130000, 0) // one small sample, 130 s
	if s, _ := tr.Score("m"); s.OverheadMs != 4000 {
		t.Errorf("overhead %d after one slow small sample; fewer than %d small samples keep the baseline's", s.OverheadMs, MinSmall)
	}

	// A big outlier among normal big samples moves the median, not the score.
	feed(tr, c, "m", 6, 20000, 4000, 6000)
	feed(tr, c, "m", 1, 20000, 130000, 6000)
	s, _ := tr.Score("m")
	if s.MsPer1k != 6000 {
		t.Errorf("rate %d with one 130 s outlier among seven; want 6000", s.MsPer1k)
	}
	if s.OverheadMs != 4000 {
		t.Errorf("overhead %d; the new baseline keeps the old overhead without %d small samples", s.OverheadMs, MinSmall)
	}
}

// Five samples used to replace the baseline whatever their size. Only K big
// samples measure a rate worth keeping.
func TestSmallSamplesDoNotReplaceTheBaseline(t *testing.T) {
	c := &clock{t0}
	tr := New(c.now)
	feed(tr, c, "m", 5, 20000, 0, 5000)
	tr.Score("m")
	c.t = c.t.Add(11 * time.Minute)
	feed(tr, c, "m", 4, 100, 0, 5000)
	feed(tr, c, "m", 1, 20000, 0, 9000)
	tr.Score("m")
	c.t = c.t.Add(11 * time.Minute) // only the baseline is left
	if s, _ := tr.Score("m"); s.MsPer1k != 5000 {
		t.Errorf("baseline rate %d; one big sample among five must not replace 5000", s.MsPer1k)
	}
}
