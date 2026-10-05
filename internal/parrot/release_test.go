package parrot

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var fast = Await{Stall: 150 * time.Millisecond, Poll: 10 * time.Millisecond}

// sequence answers from a list, repeating the last entry.
func sequence(answers ...http.HandlerFunc) http.HandlerFunc {
	var mu sync.Mutex
	i := 0
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		h := answers[min(i, len(answers)-1)]
		i++
		mu.Unlock()
		h(w, r)
	}
}

func TestAwaitRelease(t *testing.T) {
	cases := map[string]struct {
		h       http.HandlerFunc
		wantDir string
		wantErr string
	}{
		"ready at once": {h: reply(200, `{"path":"/r/v2.7.0"}`), wantDir: "/r/v2.7.0"},
		"pending then ready": {h: sequence(
			reply(202, `{"status":{"state":"downloading","percent":10}}`),
			reply(202, `{"status":{"state":"downloading","percent":60}}`),
			reply(200, `{"path":"/r/v2.7.0"}`)), wantDir: "/r/v2.7.0"},
		"stalled":     {h: reply(202, `{"status":{"state":"downloading","percent":42}}`), wantErr: "no progress"},
		"refused":     {h: reply(404, `{"error":"unknown repo"}`), wantErr: "unknown repo"},
		"unavailable": {h: reply(503, ``), wantErr: "no progress"},
		"vanishes while pending": {h: sequence(
			reply(202, `{"status":{"state":"downloading","percent":10}}`),
			func(w http.ResponseWriter, r *http.Request) {
				hj, _ := w.(http.Hijacker)
				conn, _, _ := hj.Hijack()
				conn.Close()
			}), wantErr: "not from viiwork-parrot"},
	}
	for name, c := range cases {
		cl := serve(t, c.h)
		dir, err := cl.AwaitRelease(context.Background(), "janit/viiwork", "v2.7.0", nil, fast)
		if c.wantErr == "" {
			if err != nil || dir != c.wantDir {
				t.Errorf("%s: %q, %v", name, dir, err)
			}
			continue
		}
		if !errors.Is(err, ErrNotFromParrot) || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: %q, %v (want %q)", name, dir, err, c.wantErr)
		}
	}
}

// Steady progress is never cut off, however long it takes in total.
func TestAwaitReleaseSlowButMoving(t *testing.T) {
	var mu sync.Mutex
	pct := 0
	cl := serve(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		pct++
		p := pct
		mu.Unlock()
		if p >= 40 { // 40 polls * 10ms = 400ms, well past the 150ms stall window
			reply(200, `{"path":"/r/v2.7.0"}`)(w, r)
			return
		}
		reply(202, `{"status":{"state":"downloading","percent":`+strconv.Itoa(p)+`}}`)(w, r)
	})
	dir, err := cl.AwaitRelease(context.Background(), "janit/viiwork", "v2.7.0", nil, fast)
	if err != nil || dir != "/r/v2.7.0" {
		t.Fatalf("%q, %v", dir, err)
	}
}

func TestAwaitReleaseContextCancelled(t *testing.T) {
	cl := serve(t, reply(202, `{"status":{"state":"downloading","percent":1}}`))
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	start := time.Now()
	_, err := cl.AwaitRelease(ctx, "janit/viiwork", "v2.7.0", nil, Await{Stall: time.Hour, Poll: 10 * time.Millisecond})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("returned after %v", time.Since(start))
	}
}

// 503 means parrot has no feed yet — it may while GitHub is down — so the
// wait goes on within the stall window. No connection at all means there is
// no parrot, and the wait ends at once.
func TestAwaitReleaseWaitsOutA503(t *testing.T) {
	cl := serve(t, sequence(reply(503, ``), reply(503, ``), reply(200, `{"path":"/r/v2.7.0"}`)))
	if dir, err := cl.AwaitRelease(context.Background(), "janit/viiwork", "v2.7.0", nil, fast); err != nil || dir != "/r/v2.7.0" {
		t.Fatalf("%q, %v", dir, err)
	}
	start := time.Now()
	_, err := New("127.0.0.1:1").AwaitRelease(context.Background(), "janit/viiwork", "v2.7.0", nil, Await{Stall: time.Hour, Poll: 10 * time.Millisecond})
	if !errors.Is(err, ErrNotFromParrot) || time.Since(start) > time.Second {
		t.Errorf("no parrot: %v after %v", err, time.Since(start))
	}
}

// A download that crawls — progress, but too slowly — is abandoned at Max,
// leaving the stage time to fetch the archive from GitHub instead.
func TestAwaitReleaseGivesUpAtMax(t *testing.T) {
	var mu sync.Mutex
	pct := 0
	cl := serve(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		pct++
		p := pct
		mu.Unlock()
		reply(202, `{"status":{"state":"downloading","percent":`+strconv.Itoa(p)+`}}`)(w, r)
	})
	start := time.Now()
	_, err := cl.AwaitRelease(context.Background(), "janit/viiwork", "v2.7.0", nil,
		Await{Stall: time.Hour, Poll: 10 * time.Millisecond, Max: 200 * time.Millisecond})
	if !errors.Is(err, ErrNotFromParrot) || !strings.Contains(err.Error(), "gave up") {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("returned after %v", d)
	}
}
