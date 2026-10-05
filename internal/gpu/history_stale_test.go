package gpu

import (
	"testing"
	"time"
)

// A card whose collector stopped recording must drop out of Latest rather than
// have its last sample stand as the current state.
func TestLatestDropsStaleSamples(t *testing.T) {
	now := int64(1_800_000_000)
	h := NewHistory(10)
	h.now = func() time.Time { return time.Unix(now, 0) }

	for _, ts := range []int64{now - 10, now - 5, now} {
		h.Record(GPUSample{GPUID: 0, PowerW: 100, Timestamp: ts})
		h.Record(GPUSample{GPUID: 1, PowerW: 50, Timestamp: ts})
	}
	// Card 1 keeps reporting, card 0 stops.
	for _, step := range []int64{5, 10, 15, 20} {
		h.Record(GPUSample{GPUID: 1, PowerW: 50, Timestamp: now + step})
	}

	now += 15 // card 0's newest sample is exactly three 5s periods old
	if got := h.Latest(); len(got) != 2 {
		t.Fatalf("at 3 periods both cards should be current, got %+v", got)
	}
	now += 1
	got := h.Latest()
	if len(got) != 1 || got[0].GPUID != 1 {
		t.Fatalf("card 0 is stale and must be absent, got %+v", got)
	}

	// The period follows the smallest recent gap, so a long outage followed by
	// one good sample does not stretch the window.
	h.Record(GPUSample{GPUID: 0, PowerW: 100, Timestamp: now})
	now += 16
	for _, s := range h.Latest() {
		if s.GPUID == 0 {
			t.Fatalf("card 0 is 16s old at a 5s period, want it absent")
		}
	}
}

// Until two samples exist the period is the default, so a node's first sample
// is not declared stale before its second is due.
func TestLatestSingleSampleUsesDefaultPeriod(t *testing.T) {
	now := int64(1_800_000_000)
	h := NewHistory(10)
	h.now = func() time.Time { return time.Unix(now, 0) }
	h.Record(GPUSample{GPUID: 0, Timestamp: now})
	now += int64(staleIntervals * defaultPeriod / time.Second)
	if len(h.Latest()) != 1 {
		t.Error("a lone sample within the default window must be current")
	}
	now++
	if len(h.Latest()) != 0 {
		t.Error("a lone sample past the default window must be absent")
	}
}
