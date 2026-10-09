package cost

import (
	"context"
	"log"
	"os"
	"sync"
	"time"
)

type PowerReader interface {
	Watts() float64
	Available() bool
}

type Tracker struct {
	fetcher  *SpotFetcher
	cfg      CostConfig
	power    PowerReader
	location *time.Location
	logger   *log.Logger

	mu         sync.RWMutex
	available  bool
	breakdown  CostBreakdown
	todayEUR   float64
	lastUpdate time.Time // last billed update; zero after an unavailable one
	lastDate   string    // "2006-01-02" for midnight reset detection
	lastCall   time.Time // last Update call, available or not
	minGap     time.Duration

	now func() time.Time // injectable for tests; nil is time.Now
}

func NewTracker(fetcher *SpotFetcher, cfg CostConfig, power PowerReader) *Tracker {
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		loc = time.UTC
	}
	return &Tracker{
		fetcher: fetcher, cfg: cfg, power: power,
		location: loc, logger: log.New(os.Stdout, "[cost] ", log.LstdFlags),
	}
}

// Billing gaps. An update bills the time since the previous billed update at
// the current rate, so that gap must be one update interval and never an
// outage: an unavailable update forgets lastUpdate, and as a second guard the
// billed gap is capped at maxBilledPeriods times the shortest interval seen
// between updates (floored at minUpdatePeriod), so a stalled process or a
// clock jump cannot bill minutes at one rate either.
const (
	maxBilledPeriods = 2
	minUpdatePeriod  = 5 * time.Second
)

func (t *Tracker) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// billable is the part of elapsed that may be billed. Called with t.mu held.
func (t *Tracker) billable(elapsed time.Duration) time.Duration {
	period := t.minGap
	if period < minUpdatePeriod {
		period = minUpdatePeriod
	}
	return min(elapsed, maxBilledPeriods*period)
}

func (t *Tracker) Update(ctx context.Context) {
	now := t.clock()

	t.mu.Lock()
	if !t.lastCall.IsZero() {
		if gap := now.Sub(t.lastCall); gap > 0 && (t.minGap == 0 || gap < t.minGap) {
			t.minGap = gap
		}
	}
	t.lastCall = now
	t.mu.Unlock()

	if t.fetcher.NeedsFetch(now.UTC()) {
		t.fetcher.Fetch(ctx)
	}

	spot, ok := t.fetcher.PriceAt(now.UTC())
	if !ok || !t.power.Available() {
		t.mu.Lock()
		t.available = false
		t.breakdown = CostBreakdown{}
		// Nothing was measured from here until the next available update,
		// so none of that gap may be billed when readings return.
		t.lastUpdate = time.Time{}
		t.mu.Unlock()
		return
	}

	localNow := now.In(t.location)
	bd := Calculate(spot, t.power.Watts(), t.cfg, localNow)

	t.mu.Lock()
	defer t.mu.Unlock()

	today := localNow.Format("2006-01-02")
	if t.lastDate != "" && t.lastDate != today {
		t.logger.Printf("midnight reset: accumulated %.4f EUR for %s", t.todayEUR, t.lastDate)
		t.todayEUR = 0
	}
	t.lastDate = today

	if !t.lastUpdate.IsZero() {
		if elapsed := now.Sub(t.lastUpdate); elapsed > 0 {
			t.todayEUR += bd.CostEURPerHour * t.billable(elapsed).Seconds() / 3600
		}
	}

	t.available = true
	t.breakdown = bd
	t.lastUpdate = now
}

func (t *Tracker) Available() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.available
}

func (t *Tracker) EURPerHour() float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.breakdown.CostEURPerHour
}

func (t *Tracker) TodayEUR() float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.todayEUR
}

// CostReader interface methods (individual field accessors for cross-package use)
func (t *Tracker) SpotCentsKWh() float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.breakdown.SpotCentsKWh
}
func (t *Tracker) TransferCentsKWh() float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.breakdown.TransferCentsKWh
}
func (t *Tracker) TaxCentsKWh() float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.breakdown.TaxCentsKWh
}
func (t *Tracker) VATPercent() float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.breakdown.VATPercent
}
func (t *Tracker) TotalCentsKWh() float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.breakdown.TotalCentsKWh
}
