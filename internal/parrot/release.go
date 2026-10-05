package parrot

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// ErrNotFromParrot wraps every reason AwaitRelease gives up: the caller
// falls back to downloading the release itself.
var ErrNotFromParrot = errors.New("not from viiwork-parrot")

// Await bounds AwaitRelease. Max is well inside the stage's own budget
// (15 min), so a download that crawls is abandoned while the stage still has
// time to fetch the archive from GitHub.
type Await struct {
	Stall time.Duration // give up after this long without progress; 0 = 2m
	Poll  time.Duration // between polls; 0 = 2s
	Max   time.Duration // give up after this long in all; 0 = 5m
}

// AwaitRelease asks for a release and polls until viiwork-parrot answers
// with its directory, waiting out 202 and 503 alike. Progress is a new state
// or a higher percent; a
// download that shows none for Stall is abandoned. A cancelled ctx returns
// ctx's error, not ErrNotFromParrot, so the caller can tell "give up" from
// "stop".
func (c *Client) AwaitRelease(ctx context.Context, repo, version string, peers []string, a Await) (string, error) {
	if a.Stall <= 0 {
		a.Stall = 2 * time.Minute
	}
	if a.Poll <= 0 {
		a.Poll = 2 * time.Second
	}
	if a.Max <= 0 {
		a.Max = 5 * time.Minute
	}
	deadline := time.Now().Add(a.Max)
	var (
		state    string
		percent  = -1.0
		progress = time.Now()
	)
	for {
		r := c.EnsureRelease(ctx, repo, version, peers)
		if err := ctx.Err(); err != nil {
			return "", err
		}
		switch r.Kind {
		case Ready:
			return r.Path, nil
		case Pending, Unavailable:
			// A 503 is a parrot with no feed yet — likely while GitHub is
			// down — so it is waited out like a download. No answer at all
			// (Code 0) is no parrot: give up at once.
			if r.Kind == Unavailable && r.Code != http.StatusServiceUnavailable {
				return "", fmt.Errorf("%w: %s", ErrNotFromParrot, r.Message)
			}
			if r.State != state || r.Percent > percent {
				state, percent, progress = r.State, r.Percent, time.Now()
			}
			if time.Since(progress) >= a.Stall {
				return "", fmt.Errorf("%w: no progress for %v (%s, %.0f%%)", ErrNotFromParrot, a.Stall, state, percent)
			}
			if time.Now().After(deadline) {
				return "", fmt.Errorf("%w: gave up after %v (%s, %.0f%%)", ErrNotFromParrot, a.Max, state, percent)
			}
		default:
			return "", fmt.Errorf("%w: %s (HTTP %d)", ErrNotFromParrot, r.Message, r.Code)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(a.Poll):
		}
	}
}
