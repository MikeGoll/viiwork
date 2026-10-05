package energy

import (
	"math"
	"testing"
	"time"
)

// A day fully covered at 1 kW is 24 kWh. Summed from the day tier it would be
// ~18.2 kWh, because a day bucket's CoveredS saturates at 65535 s; KWh30d must
// read the hour tier and get the whole day.
func TestKWh30dCountsAFullyCoveredDay(t *testing.T) {
	s := testStore(t, 0)
	// Yesterday, whole: a window ending now straddles midnight on most runs,
	// and two partial day buckets never reach the cap this test pins.
	end := time.Now().UTC().Truncate(24 * time.Hour)
	start := end.Add(-24 * time.Hour)
	for ts := start; ts.Before(end); ts = ts.Add(time.Minute) {
		node := NodeRecord{TS: ts.Unix(), Watts: 1000, CoveredS: 60}
		if err := s.WriteMinute(node, nil); err != nil {
			t.Fatal(err)
		}
	}

	if got := s.KWh30d(); math.Abs(got-24) > 1e-6 {
		t.Errorf("KWh30d = %.4f kWh, want 24", got)
	}
	// The reason for the tier choice, pinned: the day tier cannot say 24.
	if got := s.NodeKWh(TierDay, start.Add(-48*time.Hour), end.Add(time.Minute)); got >= 24-1e-6 {
		t.Errorf("day-tier total = %.4f; expected the documented underestimate", got)
	}
}
