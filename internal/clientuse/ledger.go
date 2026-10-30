package clientuse

import (
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// keepMinutes is how many finished minutes the ledger holds for a recorder
// that has not taken them. A recorder with no power reading takes none.
const keepMinutes = 3

// Usage is one client's use of one model in one minute.
type Usage struct {
	Client string
	Model  string
	BusyS  float64 // slot-seconds held in the minute
	Tokens int64   // completion tokens of requests that ended in it
}

type pairKey struct{ client, model string }

type usage struct {
	busy   time.Duration
	tokens int64
}

type openUse struct {
	key   pairKey
	start time.Time
}

// Ledger measures how long each client held a slot of each model on this
// node, minute by minute. It is memory only: a restart loses the open
// requests, as it loses the requests themselves.
//
// It counts nothing until it is active. A node becomes active when it has a
// reason to count: Activate, or the first request that carries a label. From
// then on a request with no label is counted as Other, because leaving it out
// of a minute it shared with a labelled request would charge that client for
// both.
type Ledger struct {
	now    func() time.Time
	active atomic.Bool

	mu     sync.Mutex
	nextID uint64
	open   map[uint64]openUse
	done   map[int64]map[pairKey]*usage // minute bucket, then pair
	taken  int64                        // newest bucket handed out by Take
}

// NewLedger builds an idle ledger. A nil now is time.Now.
func NewLedger(now func() time.Time) *Ledger {
	if now == nil {
		now = time.Now
	}
	return &Ledger{now: now, open: map[uint64]openUse{}, done: map[int64]map[pairKey]*usage{}, taken: math.MinInt64}
}

// Activate makes the ledger count every request from now on.
func (l *Ledger) Activate() {
	if l != nil {
		l.active.Store(true)
	}
}

// Use is one request holding a slot. The zero Use is inert.
type Use struct {
	l  *Ledger
	id uint64
}

// Begin records that a slot was leased for client's request to model on this
// node. client "" is a request with no label. On a nil or idle ledger a
// request with no label costs one atomic load and returns the zero Use.
func (l *Ledger) Begin(client, model string) Use {
	if l == nil {
		return Use{}
	}
	if client == "" {
		if !l.active.Load() {
			return Use{}
		}
		client = Other
	} else if !l.active.Load() {
		l.active.Store(true)
	}
	start := l.now()
	l.mu.Lock()
	l.nextID++
	id := l.nextID
	l.open[id] = openUse{key: pairKey{client, model}, start: start}
	l.mu.Unlock()
	return Use{l: l, id: id}
}

// Active reports whether the request is being counted.
func (u Use) Active() bool { return u.l != nil }

// End records that the slot was released, whatever the outcome. The request
// is charged for each of its minutes that Take has not handed out, and its
// tokens go to the minute it ends in.
func (u Use) End(completionTokens int64) {
	if u.l == nil {
		return
	}
	l := u.l
	end := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	o, ok := l.open[u.id]
	if !ok {
		return
	}
	delete(l.open, u.id)
	first, last := bucketOf(o.start), bucketOf(end)
	if first <= l.taken {
		first = l.taken + 1
	}
	for b := first; b <= last; b++ {
		l.entry(b, o.key).busy += overlap(o.start, end, b)
	}
	if last > l.taken {
		l.entry(last, o.key).tokens += completionTokens
	}
	for b := range l.done {
		if b <= last-keepMinutes {
			delete(l.done, b)
		}
	}
}

// Take returns one finished minute's use, sorted by client then model, and
// forgets it. A request still open is charged for the part of the minute it
// was open. A minute already taken, or dropped, returns nil.
func (l *Ledger) Take(bucketStart int64) []Usage {
	if l == nil {
		return nil
	}
	b := bucketStart / 60
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if b <= l.taken {
		return nil
	}
	sum := map[pairKey]usage{}
	for key, u := range l.done[b] {
		sum[key] = *u
	}
	for _, o := range l.open {
		if d := overlap(o.start, now, b); d > 0 {
			u := sum[o.key]
			u.busy += d
			sum[o.key] = u
		}
	}
	for old := range l.done {
		if old <= b {
			delete(l.done, old)
		}
	}
	l.taken = b
	if len(sum) == 0 {
		return nil
	}
	out := make([]Usage, 0, len(sum))
	for key, u := range sum {
		out = append(out, Usage{Client: key.client, Model: key.model, BusyS: u.busy.Seconds(), Tokens: u.tokens})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Client != out[j].Client {
			return out[i].Client < out[j].Client
		}
		return out[i].Model < out[j].Model
	})
	return out
}

func (l *Ledger) entry(bucket int64, key pairKey) *usage {
	m := l.done[bucket]
	if m == nil {
		m = map[pairKey]*usage{}
		l.done[bucket] = m
	}
	u := m[key]
	if u == nil {
		u = &usage{}
		m[key] = u
	}
	return u
}

func bucketOf(t time.Time) int64 { return t.Unix() / 60 }

// overlap is how much of [start, end) lies in minute bucket. A clock that
// stepped backwards gives zero, never a negative time.
func overlap(start, end time.Time, bucket int64) time.Duration {
	lo, hi := time.Unix(bucket*60, 0), time.Unix(bucket*60+60, 0)
	if start.After(lo) {
		lo = start
	}
	if end.Before(hi) {
		hi = end
	}
	if hi.After(lo) {
		return hi.Sub(lo)
	}
	return 0
}
