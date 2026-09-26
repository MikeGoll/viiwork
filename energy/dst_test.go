package energy

import (
	"testing"
	"time"
)

// dayRecord writes one covered minute per hour, from the first watts slice
// entry at startLocal onward, and returns the day-tier record of day.
func dstStore(t *testing.T) (*Store, *time.Location) {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Helsinki")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	s, err := Open(Config{Dir: t.TempDir(), GPUIDs: []int{0}, Location: loc}, quiet())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, loc
}

func writeHourly(t *testing.T, s *Store, from time.Time, hours int, watts float32) {
	t.Helper()
	for h := range hours {
		ts := from.Add(time.Duration(h) * time.Hour)
		if err := s.WriteMinute(NodeRecord{TS: ts.Unix(), Watts: watts, CoveredS: 60}, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func dayRecord(t *testing.T, s *Store, midnight time.Time) NodeRecord {
	t.Helper()
	for _, r := range s.ReadNode(TierDay, midnight.Add(-time.Hour), midnight.Add(time.Hour)) {
		if r.TS == midnight.Unix() {
			return r
		}
	}
	t.Fatalf("no day record at %v", midnight)
	return NodeRecord{}
}

// 2026-10-25 in Helsinki is 25 hours long. Its last hour must be in the day,
// not dropped because the window was a fixed 24 hours.
func TestDayRollUpCoversA25HourDay(t *testing.T) {
	s, loc := dstStore(t)
	day := time.Date(2026, 10, 25, 0, 0, 0, 0, loc)
	next := time.Date(2026, 10, 26, 0, 0, 0, 0, loc)
	if got := next.Sub(day); got != 25*time.Hour {
		t.Fatalf("test premise: day is %v", got)
	}
	writeHourly(t, s, day, 24, 1000)
	writeHourly(t, s, day.Add(24*time.Hour), 1, 5000) // the 25th hour, still 2026-10-25 locally

	rec := dayRecord(t, s, day)
	if rec.CoveredS != 25*60 {
		t.Errorf("covered = %d s, want %d (25 hours)", rec.CoveredS, 25*60)
	}
	if want := float32(24*1000+5000) / 25; rec.Watts != want {
		t.Errorf("watts = %v, want %v", rec.Watts, want)
	}
}

// 2026-03-29 in Helsinki is 23 hours long. The next day's first hour must not
// be rolled into it.
func TestDayRollUpStopsAtA23HourDaysEnd(t *testing.T) {
	s, loc := dstStore(t)
	day := time.Date(2026, 3, 29, 0, 0, 0, 0, loc)
	next := time.Date(2026, 3, 30, 0, 0, 0, 0, loc)
	if got := next.Sub(day); got != 23*time.Hour {
		t.Fatalf("test premise: day is %v", got)
	}
	// The next day first, so that the DST day's roll-up runs with the next
	// day's hours already on disk.
	writeHourly(t, s, next, 2, 5000)
	writeHourly(t, s, day, 23, 1000)

	rec := dayRecord(t, s, day)
	if rec.CoveredS != 23*60 || rec.Watts != 1000 {
		t.Errorf("day = %+v, want 1000 W over %d s", rec, 23*60)
	}
	if nxt := dayRecord(t, s, next); nxt.Watts != 5000 {
		t.Errorf("next day = %+v, want 5000 W", nxt)
	}
}
