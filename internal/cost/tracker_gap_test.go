package cost

import (
	"context"
	"math"
	"testing"
	"time"
)

type testClock struct{ t time.Time }

func (c *testClock) now() time.Time { return c.t }

func gapTracker(pw *mockPower, clk *testClock) *Tracker {
	cfg := CostConfig{
		Transfer:               TransferConfig{Summer: SummerTransferConfig{FlatCentsKWh: 2.49}},
		ElectricityTaxCentsKWh: 2.253,
		VATPercent:             25.5,
		Timezone:               "UTC",
	}
	start := clk.t.Truncate(time.Hour)
	fetcher := &SpotFetcher{prices: []PricePoint{
		{Time: start, CentsKWh: 5.0},
		{Time: start.Add(time.Hour), CentsKWh: 5.0},
		{Time: start.Add(2 * time.Hour), CentsKWh: 5.0},
	}}
	tr := NewTracker(fetcher, cfg, pw)
	tr.now = clk.now
	return tr
}

// An outage must not be billed when power readings come back: the gap is
// unmeasured, not a period at the current rate.
func TestTrackerDoesNotBillAnOutage(t *testing.T) {
	clk := &testClock{t: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)}
	pw := &mockPower{watts: 1000, available: true}
	tr := gapTracker(pw, clk)

	step := func(d time.Duration) { clk.t = clk.t.Add(d); tr.Update(context.Background()) }
	tr.Update(context.Background())
	step(5 * time.Second)
	perSecond := tr.TodayEUR() / 5
	if perSecond <= 0 {
		t.Fatal("expected accumulation over the first 5 s")
	}

	pw.available = false
	step(5 * time.Second)
	step(30 * time.Minute)
	pw.available = true
	step(5 * time.Second) // first update back: nothing billed
	step(5 * time.Second) // one normal interval billed

	if got, want := tr.TodayEUR(), 10*perSecond; math.Abs(got-want) > want*1e-9 {
		t.Errorf("TodayEUR = %v, want %v (10 s billed, the outage none)", got, want)
	}
}

// A stall between two available updates is billed for at most two update
// intervals.
func TestTrackerCapsTheBilledGap(t *testing.T) {
	clk := &testClock{t: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)}
	pw := &mockPower{watts: 1000, available: true}
	tr := gapTracker(pw, clk)

	tr.Update(context.Background())
	clk.t = clk.t.Add(5 * time.Second)
	tr.Update(context.Background())
	perSecond := tr.TodayEUR() / 5

	clk.t = clk.t.Add(45 * time.Minute)
	tr.Update(context.Background())
	if got, want := tr.TodayEUR(), 15*perSecond; math.Abs(got-want) > want*1e-9 {
		t.Errorf("TodayEUR = %v, want %v (5 s + a 10 s cap)", got, want)
	}
}
