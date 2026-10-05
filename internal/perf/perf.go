// Package perf keeps this node's measured time to first token per model: at
// most ten one-minute buckets of samples, blended with a baseline that
// survives quiet spells and restarts. It is pure and clock-injected; the
// proxy records into it and the capacity report publishes its Score.
// Spec: docs/superpowers/specs/2026-09-29-performance-routing-design.md §2.
package perf

import (
	"math"
	"sort"
	"sync"
	"time"
)

const (
	// Buckets is the window in one-minute buckets: a score is at most ten
	// minutes old, so old traffic never outweighs what the host does now.
	Buckets = 10
	// K is the baseline's weight in samples. 20 never converged at the
	// fleet's usual traffic (spike §3): a host rarely collects 20 samples in
	// ten minutes, so its score sat at the fleet median forever.
	K = 5
	// BigTokens is the smallest uncached prompt that feeds the rate; smaller
	// ones measure the fixed overhead.
	BigTokens = 512
	// maxPerBucket bounds memory on a very busy minute.
	maxPerBucket = 256
	// MinSmall is how many small samples the window needs before its overhead
	// replaces the baseline's: one slow request is not a host's overhead.
	MinSmall = 3
	// fitRounds refines overhead and rate against each other; three rounds
	// settle well inside the two significant figures a score is published at.
	fitRounds = 3
)

type sample struct {
	uncached int64
	ttftMs   float64
}

type bucket struct {
	minute  int64
	samples []sample
}

type baseline struct {
	OverheadMs float64   `json:"overhead_ms"`
	MsPer1k    float64   `json:"ms_per_1k"`
	At         time.Time `json:"at"`
	Key        string    `json:"key"`
}

type model struct {
	buckets []bucket
	base    *baseline
	key     string
	small   []sample // window's buffers, reused
	big     []sample
	scratch []float64

	// The computed score, valid while cached is true and the minute is
	// cacheMin: the window only changes on a Record, on SetKeys or Load, and
	// when a minute rolls over and a bucket may expire. The router asks for
	// this node's score on every scored pick, so a hit must not scan, sort or
	// allocate.
	cached   bool
	cacheMin int64
	cacheHas bool
	cache    Score // BaselineAge is filled in per call
	rebase   bool  // the window is the baseline: refresh its time per call
}

// Score is what a host publishes for one model. Values are rounded to two
// significant figures and never below 1, so a published 0 cannot happen and
// omitempty keeps meaning "cannot say".
type Score struct {
	OverheadMs  int
	MsPer1k     int
	Samples     int // in the current window; 0 = the baseline alone
	Baseline    bool
	BaselineAge time.Duration
}

type Tracker struct {
	mu     sync.Mutex
	now    func() time.Time
	models map[string]*model
	dirty  bool // a baseline changed since the last Save
}

func New(now func() time.Time) *Tracker {
	return &Tracker{now: now, models: map[string]*model{}}
}

func (t *Tracker) get(name string) *model {
	m := t.models[name]
	if m == nil {
		m = &model{}
		t.models[name] = m
	}
	return m
}

// Record adds one sample: uncached prompt tokens and service TTFT, measured
// from local slot admission to the first content chunk.
func (t *Tracker) Record(name string, at time.Time, uncached int64, ttft time.Duration) {
	if uncached < 0 || ttft <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	m := t.get(name)
	m.cached = false
	b := m.bucketFor(at.Unix() / 60)
	if len(b.samples) < maxPerBucket {
		b.samples = append(b.samples, sample{uncached, float64(ttft) / float64(time.Millisecond)})
	}
	m.expire(at)
}

// bucketFor finds or inserts the bucket for a minute, keeping the buckets in
// minute order: long generations finish out of order, and expire trims a
// prefix, so an old minute appended after a newer one would outlive the
// window.
func (m *model) bucketFor(minute int64) *bucket {
	i := len(m.buckets)
	for i > 0 && m.buckets[i-1].minute > minute {
		i--
	}
	if i > 0 && m.buckets[i-1].minute == minute {
		return &m.buckets[i-1]
	}
	m.buckets = append(m.buckets, bucket{})
	copy(m.buckets[i+1:], m.buckets[i:])
	m.buckets[i] = bucket{minute: minute}
	return &m.buckets[i]
}

func (m *model) expire(now time.Time) {
	oldest := now.Unix()/60 - Buckets
	i := 0
	for i < len(m.buckets) && m.buckets[i].minute <= oldest {
		i++
	}
	m.buckets = m.buckets[i:]
}

// window is the estimate over the current buckets. hasRate is false when no
// sample of BigTokens or more exists; nBig counts those samples, and ovOK says
// the overhead came from MinSmall or more of the window's own small samples.
//
// Overhead is the median small-sample TTFT after taking off each sample's own
// prefill, and rate the median big-sample rate after taking off the overhead;
// each needs the other, so they are refined together from the baseline (or
// zero). Until 2026-10-03 overhead was the raw median of small samples, which
// counted up to 511 tokens of prefill as overhead: two identical gb pairs
// published 2.1 s and 5.1 s by the size of their short prompts alone, and the
// band then ruled on that. Medians rather than sums, because one request that
// waited 130 s (gb1, the same day) must not set a host's score.
func (m *model) window() (overhead, rate float64, n, nBig int, hasRate, ovOK bool) {
	small, big := m.small[:0], m.big[:0]
	for _, b := range m.buckets {
		for _, s := range b.samples {
			n++
			if s.uncached < BigTokens {
				small = append(small, s)
			} else {
				big = append(big, s)
			}
		}
	}
	m.small, m.big = small[:0], big[:0]
	nBig, hasRate = len(big), len(big) > 0
	if m.base != nil {
		overhead, rate = m.base.OverheadMs, m.base.MsPer1k
	}
	ovOK = len(small) >= MinSmall || (m.base == nil && len(small) > 0)
	for round := 0; round < fitRounds; round++ {
		if ovOK {
			xs := m.scratch[:0]
			for _, s := range small {
				xs = append(xs, math.Max(s.ttftMs-rate*float64(s.uncached)/1000, 0))
			}
			overhead = median(xs)
			m.scratch = xs[:0]
		}
		if hasRate {
			xs := m.scratch[:0]
			for _, s := range big {
				xs = append(xs, math.Max(s.ttftMs-overhead, 0)/(float64(s.uncached)/1000))
			}
			rate = median(xs)
			m.scratch = xs[:0]
		}
		if !ovOK || !hasRate {
			break // nothing left to refine against
		}
	}
	if !hasRate && m.base == nil {
		rate = 0
	}
	ovOK = ovOK && len(small) >= MinSmall
	return overhead, rate, n, nBig, hasRate, ovOK
}

// median sorts xs in place.
func median(xs []float64) float64 {
	sort.Float64s(xs)
	return xs[len(xs)/2]
}

// Score is the published score for a model; false when there is nothing to
// say. A window of K or more samples with a rate becomes the new baseline.
// Between records and within a minute it returns the cached computation.
func (t *Tracker) Score(name string) (Score, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	m := t.models[name]
	if m == nil {
		return Score{}, false
	}
	now := t.now()
	if minute := now.Unix() / 60; !m.cached || m.cacheMin != minute {
		m.compute(now)
		m.cached, m.cacheMin = true, minute
	}
	if m.rebase {
		// The window is the baseline, refreshed as of every Score, exactly as
		// if it were recomputed: same values, so only its time moves.
		m.base.At = now
		t.dirty = true
	}
	if !m.cacheHas {
		return Score{}, false
	}
	s := m.cache
	if m.base != nil {
		s.Baseline, s.BaselineAge = true, now.Sub(m.base.At)
	}
	return s, true
}

// compute recomputes the cached score as of now.
func (m *model) compute(now time.Time) {
	m.expire(now)
	ov, rate, n, nBig, hasRate, ovOK := m.window()
	// Only K big samples make a new baseline, and its overhead is the old
	// one's unless MinSmall small samples measured a new one.
	m.rebase = hasRate && nBig >= K
	if m.rebase {
		bov := ov
		if !ovOK && m.base != nil {
			bov = m.base.OverheadMs
		}
		m.base = &baseline{OverheadMs: bov, MsPer1k: rate, At: now, Key: m.key}
	}
	m.cacheHas = true
	switch {
	case m.base == nil && !hasRate:
		m.cacheHas = false
		m.cache = Score{}
		return
	case m.base == nil:
		// window alone
	case !hasRate:
		w := float64(n) / float64(n+K)
		ov, rate = w*ov+(1-w)*m.base.OverheadMs, m.base.MsPer1k
	default:
		w := float64(n) / float64(n+K)
		ov, rate = w*ov+(1-w)*m.base.OverheadMs, w*rate+(1-w)*m.base.MsPer1k
	}
	m.cache = Score{OverheadMs: round2(ov), MsPer1k: round2(rate), Samples: n}
}

// SetKeys names the models configured now, each with a hash of what makes
// its speed. A model whose key changed loses its baseline and its window,
// both measured under the old key; a model no longer listed is forgotten.
func (t *Tracker) SetKeys(keys map[string]string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for name := range t.models {
		if _, ok := keys[name]; !ok {
			delete(t.models, name)
			t.dirty = true
		}
	}
	for name, key := range keys {
		m := t.get(name)
		if m.key != key {
			m.buckets = nil
		}
		m.key = key
		m.cached = false
		if m.base != nil && m.base.Key != key {
			m.base = nil
			t.dirty = true
		}
	}
}

// round2 rounds to two significant figures, never below 1.
func round2(x float64) int {
	if x < 1 {
		return 1
	}
	p := math.Pow(10, math.Floor(math.Log10(x))-1)
	return int(math.Round(math.Round(x/p) * p))
}
