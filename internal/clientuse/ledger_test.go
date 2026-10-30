package clientuse

import (
	"testing"
	"time"
)

// base is the start of a minute.
const base = int64(1790000040)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }
func (c *clock) at(s int64)     { c.t = time.Unix(base+s, 0) }

func newTestLedger() (*Ledger, *clock) {
	c := &clock{}
	c.at(0)
	return NewLedger(c.now), c
}

func only(t *testing.T, got []Usage) Usage {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("want one usage, got %+v", got)
	}
	return got[0]
}

func TestRequestInsideOneMinute(t *testing.T) {
	l, c := newTestLedger()
	c.at(5)
	u := l.Begin("factory-a", "m")
	c.at(25)
	u.End(40)
	c.at(61)
	if got := only(t, l.Take(base)); got != (Usage{Client: "factory-a", Model: "m", BusyS: 20, Tokens: 40}) {
		t.Errorf("got %+v", got)
	}
}

// A request is cut at the minute boundary, and its tokens belong to the
// minute it ends in, whichever of End and Take comes first.
func TestRequestCrossingABoundary(t *testing.T) {
	for _, endFirst := range []bool{false, true} {
		l, c := newTestLedger()
		c.at(50)
		u := l.Begin("factory-a", "m")
		if endFirst {
			c.at(70)
			u.End(9)
		}
		c.at(75)
		first := only(t, l.Take(base))
		if !endFirst {
			u.End(9) // at +75
		}
		c.at(121)
		second := only(t, l.Take(base+60))
		wantSecond := 10.0
		if !endFirst {
			wantSecond = 15
		}
		if first.BusyS != 10 || first.Tokens != 0 || second.BusyS != wantSecond || second.Tokens != 9 {
			t.Errorf("endFirst=%v: first %+v, second %+v", endFirst, first, second)
		}
	}
}

func TestTwoClientsInTwoSlots(t *testing.T) {
	l, c := newTestLedger()
	c.at(0)
	a := l.Begin("factory-a", "m")
	c.at(30)
	b := l.Begin("batch-b", "m")
	c.at(40)
	a.End(1)
	c.at(50)
	b.End(2)
	c.at(61)
	got := l.Take(base)
	if len(got) != 2 || got[0] != (Usage{"batch-b", "m", 20, 2}) || got[1] != (Usage{"factory-a", "m", 40, 1}) {
		t.Errorf("got %+v, want batch-b 20 s and factory-a 40 s, sorted by client", got)
	}
}

// Review Focus 1: a long generation is charged in every one of its minutes.
func TestTwentyMinuteRequestIsChargedEveryMinute(t *testing.T) {
	l, c := newTestLedger()
	c.at(10)
	u := l.Begin("factory-a", "m")
	for i := int64(0); i < 20; i++ {
		c.at(60*(i+1) + 1)
		got := only(t, l.Take(base+60*i))
		want := 60.0
		if i == 0 {
			want = 50
		}
		if got.BusyS != want {
			t.Fatalf("minute %d: %v busy seconds, want %v", i, got.BusyS, want)
		}
	}
	u.End(500) // at 20:01, in a minute not taken yet
	c.at(60*21 + 1)
	if got := only(t, l.Take(base+60*20)); got.BusyS != 1 || got.Tokens != 500 {
		t.Errorf("the last minute: %+v", got)
	}
}

// Minutes nobody took (the recorder had no power reading) do not pile up.
func TestUntakenMinutesAreDroppedAfterThree(t *testing.T) {
	l, c := newTestLedger()
	for i := int64(0); i < 5; i++ {
		c.at(60*i + 1)
		u := l.Begin("factory-a", "m")
		c.at(60*i + 11)
		u.End(1)
	}
	if n := len(l.done); n != 3 {
		t.Fatalf("the ledger holds %d minutes, want 3", n)
	}
	c.at(301)
	if got := l.Take(base + 60); got != nil {
		t.Errorf("a dropped minute returned %+v", got)
	}
	if got := only(t, l.Take(base+240)); got.BusyS != 10 {
		t.Errorf("the newest minute: %+v", got)
	}
}

// Review Focus 2: off until there is a reason to count, and from then on a
// request with no label is counted as other.
func TestUnlabelledRequests(t *testing.T) {
	l, c := newTestLedger()
	c.at(1)
	if u := l.Begin("", "m"); u.Active() {
		t.Fatal("an idle ledger counted a request with no label")
	}
	labelled := l.Begin("factory-a", "m")
	plain := l.Begin("", "m")
	if !labelled.Active() || !plain.Active() {
		t.Fatal("after a labelled request, both must be counted")
	}
	c.at(31)
	labelled.End(0)
	plain.End(0)
	c.at(61)
	got := l.Take(base)
	if len(got) != 2 || got[0].Client != "factory-a" || got[1].Client != Other || got[0].BusyS != 30 || got[1].BusyS != 30 {
		t.Errorf("got %+v, want factory-a and other with 30 s each", got)
	}

	l2, _ := newTestLedger()
	l2.Activate()
	if !l2.Begin("", "m").Active() {
		t.Error("an activated ledger ignored a request with no label")
	}
}

func TestNilAndZeroAreInert(t *testing.T) {
	var l *Ledger
	l.Activate()
	u := l.Begin("factory-a", "m")
	if u.Active() {
		t.Error("a nil ledger returned an active use")
	}
	u.End(5)
	Use{}.End(5)
	if l.Take(base) != nil {
		t.Error("a nil ledger returned usage")
	}
}

func TestAMinuteIsTakenOnce(t *testing.T) {
	l, c := newTestLedger()
	c.at(1)
	u := l.Begin("factory-a", "m")
	c.at(61)
	if got := only(t, l.Take(base)); got.BusyS != 59 {
		t.Fatalf("got %+v", got)
	}
	if got := l.Take(base); got != nil {
		t.Errorf("the second take returned %+v", got)
	}
	u.End(0)
}

// The ledger is on the per-request path.
func TestBeginEndDoNotAllocate(t *testing.T) {
	idle, _ := newTestLedger()
	if n := testing.AllocsPerRun(1000, func() { idle.Begin("", "m").End(0) }); n != 0 {
		t.Errorf("an idle ledger allocates %v per request", n)
	}
	busy, _ := newTestLedger()
	busy.Begin("factory-a", "m").End(0) // the minute's entry now exists
	if n := testing.AllocsPerRun(1000, func() { busy.Begin("factory-a", "m").End(1) }); n != 0 {
		t.Errorf("a counting ledger allocates %v per request", n)
	}
}

func BenchmarkLedgerBeginEnd(b *testing.B) {
	l := NewLedger(nil)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		l.Begin("factory-a", "m").End(1)
	}
}
