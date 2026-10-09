package node

import (
	"testing"
	"time"

	"github.com/janit/viiwork/v2/energy"
)

// A node reading that goes unavailable mid-minute — a stale BMC — must shrink
// that minute's covered seconds rather than be recorded as a full minute, and a
// chassis store must not take the GPU-sum fallback as chassis draw.
func TestRecorderRecordsLessCoverageWhenPowerGoesStale(t *testing.T) {
	store, err := energy.Open(energy.Config{Dir: t.TempDir(), GPUIDs: []int{0}, Location: time.UTC}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	pw := &fakePower{available: true, source: "dcmi", chassis: true, watts: 400}
	readings := &fakeReadings{}
	readings.set(energy.GPUReading{GPUID: 0, Watts: 100, Model: "a"})
	rec := newRecorder(store, 30*time.Second, pw, readings.get)

	t0 := time.Now().Truncate(time.Minute).Add(-10 * time.Minute)
	rec.Sample(t0.Add(1 * time.Second))
	// The BMC goes stale: NodePower is still "available" through the GPU sum,
	// but no longer a chassis reading.
	pw.mu.Lock()
	pw.chassis, pw.watts = false, 100
	pw.mu.Unlock()
	rec.Sample(t0.Add(31 * time.Second))
	rec.Sample(t0.Add(61 * time.Second))
	rec.Sample(t0.Add(91 * time.Second))
	rec.Flush(t0.Add(119 * time.Second))

	recs := store.ReadNode(energy.TierMinute, t0.Add(-time.Minute), t0.Add(3*time.Minute))
	if len(recs) != 1 {
		t.Fatalf("want only the first minute recorded, got %+v", recs)
	}
	if recs[0].TS != t0.Unix() || recs[0].Watts != 400 || recs[0].CoveredS != 30 {
		t.Errorf("first minute = %+v, want 400 W over 30 covered seconds", recs[0])
	}
}
