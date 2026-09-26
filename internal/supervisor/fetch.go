package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/logging"
	"github.com/janit/viiwork/v2/internal/parrot"
)

// fetch resolves a model's viiwork-parrot source to a local path, once for all
// of the model's backends.
//
// It runs before any of them joins the load gate, so a download that takes
// hours holds up no other model's load, and before any health ladder exists,
// so it never counts against startup_timeout. viiwork downloads nothing
// itself: viiwork-parrot does, and seeds what it downloaded.
type fetch struct {
	source string // "viiwork-parrot:<id>", as the operator wrote it
	id     string
	deps   Deps
	out    io.Writer

	once sync.Once
	path string
	err  error
}

// newFetch returns nil for a model with a path.
func newFetch(m config.Model, deps Deps) *fetch {
	id, ok := m.ParrotID()
	if !ok {
		return nil
	}
	return &fetch{source: m.Source, id: id, deps: deps, out: logging.NewPrefixWriter(deps.Log, "["+m.Name+"] ")}
}

// wait returns the resolved path, resolving on the first call. Every backend
// of the model calls it with the model's loop context; the later ones block
// until the first returns. An ended context ends the resolve and is returned
// as the error — the model is then being drained, and never resolves again.
func (f *fetch) wait(ctx context.Context) (string, error) {
	f.once.Do(func() { f.path, f.err = f.resolve(ctx) })
	return f.path, f.err
}

func (f *fetch) logf(format string, args ...any) {
	fmt.Fprintf(f.out, format+"\n", args...)
}

func (f *fetch) resolve(ctx context.Context) (string, error) {
	if f.deps.Resolver == nil {
		return "", errors.New("no viiwork-parrot resolver is configured on this node")
	}
	t := f.deps.Timing
	backoff := t.FetchPoll
	// lastErr is seeded to a sentinel no real message can equal, so the first
	// Unavailable answer always logs — even one with an empty message (a 503
	// with no error text is otherwise silent forever: see fetch_test.go).
	lastStep, lastLog, lastErr := -1, time.Time{}, "\x00"
	for {
		r := f.deps.Resolver.Ensure(ctx, f.id)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		wait := t.FetchPoll
		switch r.Kind {
		case parrot.Ready:
			if _, err := os.Stat(r.Path); err != nil {
				return "", fmt.Errorf("viiwork-parrot returned %s for %s, which does not exist here: mount viiwork-parrot's data directory at the same path inside the container: %v", r.Path, f.source, err)
			}
			f.logf("fetched %s: %s", f.source, r.Path)
			return r.Path, nil
		case parrot.Pending:
			backoff, lastErr = t.FetchPoll, ""
			now := f.deps.Now()
			if step := int(r.Percent) / 10; step != lastStep || now.Sub(lastLog) >= t.FetchLogEvery {
				lastStep, lastLog = step, now
				f.logf("fetching %s: %s %.1f%% at %.1f MB/s", f.source, r.State, r.Percent, float64(r.DownRate)/1e6)
			}
		case parrot.Unavailable:
			if r.Message != lastErr {
				lastErr = r.Message
				f.logf("waiting for viiwork-parrot: %s", r.Message)
			}
			wait = backoff
			backoff = min(backoff*2, t.FetchRetryMax)
		default:
			return "", refusal(f.source, r)
		}
		if !sleepCtx(ctx, wait) {
			return "", ctx.Err()
		}
	}
}

// refusal is the cause a refused model's backends die with.
func refusal(source string, r parrot.Result) error {
	msg := fmt.Sprintf("viiwork-parrot refused %s (HTTP %d): %s", source, r.Code, r.Message)
	if r.Code == http.StatusConflict {
		msg += " (add it to want, or drop seed_only_existing, in viiwork-parrot's config)"
	}
	return errors.New(msg)
}

// sleepCtx waits d, returning false if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
