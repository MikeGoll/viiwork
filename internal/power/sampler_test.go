package power

import (
	"context"
	"testing"
	"time"
)

// fakeClock is a settable time source for staleness tests.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// A BMC that stops answering must not leave its last wattage standing as a
// measurement: after staleIntervals sample periods without a good read the
// sampler reports unavailable, and it recovers on the first good read.
func TestSamplerGoesStaleWhenReadsKeepFailing(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	fail := false
	s := &Sampler{now: clk.now}
	s.available.Store(true)
	s.cmdFactory = func(ctx context.Context) ([]byte, error) {
		if fail {
			return nil, context.DeadlineExceeded
		}
		return []byte("PS1 Input Power    | 64h | ok  | 10.1 | 280 Watts\n"), nil
	}

	// Two good samples 5s apart establish the period.
	s.Sample(context.Background())
	clk.advance(5 * time.Second)
	s.Sample(context.Background())
	if !s.Available() || s.Watts() != 280 {
		t.Fatalf("fresh: available=%v watts=%v", s.Available(), s.Watts())
	}

	fail = true
	for i := 1; i <= 3; i++ {
		clk.advance(5 * time.Second)
		s.Sample(context.Background())
		if !s.Available() {
			t.Fatalf("after %d failed reads (%ds) the reading should still count", i, 5*i)
		}
	}
	clk.advance(5 * time.Second)
	s.Sample(context.Background())
	if s.Available() {
		t.Fatal("a reading 20s old at a 5s period must be stale")
	}
	if s.Watts() != 0 {
		t.Errorf("stale Watts = %v, want 0 (absent)", s.Watts())
	}

	fail = false
	clk.advance(5 * time.Second)
	s.Sample(context.Background())
	if !s.Available() || s.Watts() != 280 {
		t.Errorf("after recovery: available=%v watts=%v", s.Available(), s.Watts())
	}
}

// Staleness is judged against the caller's own tick, so a slow health
// interval does not flap between samples.
func TestSamplerStalenessFollowsTheObservedPeriod(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	s := &Sampler{now: clk.now}
	s.available.Store(true)
	s.cmdFactory = func(ctx context.Context) ([]byte, error) {
		return []byte("PS1 Input Power    | 64h | ok  | 10.1 | 280 Watts\n"), nil
	}
	s.Sample(context.Background())
	clk.advance(60 * time.Second)
	s.Sample(context.Background())
	clk.advance(59 * time.Second)
	if !s.Available() {
		t.Error("59s after a read at a 60s period must still be fresh")
	}
	clk.advance(2*time.Minute + 2*time.Second)
	if s.Available() {
		t.Error("three periods without a read must be stale")
	}
}

func newTestSampler(available bool, watts float64) *Sampler {
	s := &Sampler{}
	s.available.Store(available)
	s.lastWatts = watts
	return s
}

func TestSamplerWithMockCommand(t *testing.T) {
	s := newTestSampler(true, 0)
	s.cmdFactory = func(ctx context.Context) ([]byte, error) {
		return []byte("PS1 Input Power    | 64h | ok  | 10.1 | 280 Watts\n"), nil
	}
	s.Sample(context.Background())

	if s.Watts() != 280.0 {
		t.Errorf("expected 280.0, got %f", s.Watts())
	}
}

func TestSamplerUnavailable(t *testing.T) {
	s := newTestSampler(false, 0)
	s.Sample(context.Background())
	if s.Watts() != 0.0 {
		t.Errorf("expected 0.0 when unavailable, got %f", s.Watts())
	}
	if s.Available() {
		t.Error("expected Available() = false")
	}
}

func TestSamplerKeepsLastValueOnError(t *testing.T) {
	s := newTestSampler(true, 300.0)
	s.cmdFactory = func(ctx context.Context) ([]byte, error) {
		return nil, context.DeadlineExceeded
	}
	s.Sample(context.Background())
	if s.Watts() != 300.0 {
		t.Errorf("expected 300.0 (last value), got %f", s.Watts())
	}
}

func TestSamplerThreadSafe(t *testing.T) {
	s := newTestSampler(true, 0)
	s.cmdFactory = func(ctx context.Context) ([]byte, error) {
		return []byte("PS1 Input Power    | 64h | ok  | 10.1 | 100 Watts\n"), nil
	}

	done := make(chan struct{})
	go func() {
		for range 100 {
			s.Sample(context.Background())
		}
		close(done)
	}()
	for range 100 {
		_ = s.Watts()
		_ = s.Available()
	}
	<-done
}
