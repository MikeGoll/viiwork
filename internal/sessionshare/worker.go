package sessionshare

import (
	"context"
	"sync"
	"time"
)

// job is one session to save and send once its turn has settled.
type job struct {
	model, key string
	hash       uint64
	raw        string
	backend    Backend
	due        time.Time
}

// worker runs one backend's saves one at a time: a save pauses every slot of
// a Strata backend, so two in a row must not overlap, and a session's later
// turn replaces its waiting job instead of queueing a second save.
type worker struct {
	mu   sync.Mutex
	jobs map[string]job // by model + key
	wake chan struct{}
}

func newWorker() *worker {
	return &worker{jobs: map[string]job{}, wake: make(chan struct{}, 1)}
}

func (w *worker) add(j job) {
	w.mu.Lock()
	w.jobs[j.model+"\x00"+j.key] = j
	w.mu.Unlock()
	w.poke()
}

// drop forgets a waiting job: a new turn of the session has started.
func (w *worker) drop(model, key string) {
	w.mu.Lock()
	delete(w.jobs, model+"\x00"+key)
	w.mu.Unlock()
}

func (w *worker) poke() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// next takes the job due first, if it is due by now, and says how long to
// wait otherwise (0 = nothing waiting).
func (w *worker) next(now time.Time) (job, bool, time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var first job
	var firstKey string
	for k, j := range w.jobs {
		if firstKey == "" || j.due.Before(first.due) {
			first, firstKey = j, k
		}
	}
	if firstKey == "" {
		return job{}, false, 0
	}
	if wait := first.due.Sub(now); wait > 0 {
		return job{}, false, wait
	}
	delete(w.jobs, firstKey)
	return first, true, 0
}

func (w *worker) run(ctx context.Context, do func(context.Context, job)) {
	for {
		j, ok, wait := w.next(time.Now())
		if ok {
			do(ctx, j)
			if ctx.Err() != nil {
				return
			}
			continue
		}
		var t *time.Timer
		var timer <-chan time.Time
		if wait > 0 {
			t = time.NewTimer(wait)
			timer = t.C
		}
		select {
		case <-ctx.Done():
		case <-w.wake:
		case <-timer:
		}
		if t != nil {
			t.Stop()
		}
		if ctx.Err() != nil {
			return
		}
	}
}
