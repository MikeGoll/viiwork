// Package stream follows a node's /v1/mesh/stream: the cluster snapshots and
// activity events the /mesh dashboard is built from, reconnecting with
// backoff when the stream drops.
package stream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

// Message is one thing the stream said. Exactly one field is set.
type Message struct {
	Cluster   *meshapi.ClusterResponse
	Event     *meshapi.MeshEvent
	Connected bool  // the stream opened (again)
	Lost      error // the stream dropped; in-flight state must be rebuilt
}

// ErrNoStream is a node that does not serve the stream at all: too old.
var ErrNoStream = errors.New("node does not serve " + meshapi.PathMeshStream)

const (
	firstBackoff = time.Second
	maxBackoff   = 10 * time.Second
	maxFrame     = 16 << 20
	// defaultIdle is how long the stream may say nothing before it counts as
	// dead. The entry node's own snapshot changes every second (its uptime
	// ticks), so a live stream is never silent this long.
	defaultIdle = 10 * time.Second
)

// Follower reads one node's stream until its context ends.
type Follower struct {
	Client *http.Client
	Node   string // host:port
	// Sleep waits between attempts; nil waits on the clock.
	Sleep func(context.Context, time.Duration) error
	// Idle is how long the stream may be silent, headers included, before
	// the attempt is abandoned as Lost. Zero means 10 s. Without it a hung
	// node or a powered-off host freezes the view while it still says
	// connected, until TCP keepalive gives up minutes later.
	Idle time.Duration
}

// Run follows the stream, sending every message to out. It returns ErrNoStream
// when the node answers 404, and the context's error when it ends; every other
// failure is reported as Lost and retried.
func (f *Follower) Run(ctx context.Context, out chan<- Message) error {
	sleep := f.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	backoff := firstBackoff
	for {
		connected, err := f.once(ctx, out)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, ErrNoStream) {
			return err
		}
		if connected {
			backoff = firstBackoff
		}
		if !send(ctx, out, Message{Lost: err}) {
			return ctx.Err()
		}
		if err := sleep(ctx, backoff); err != nil {
			return err
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (f *Follower) once(ctx context.Context, out chan<- Message) (connected bool, err error) {
	idle := f.Idle
	if idle <= 0 {
		idle = defaultIdle
	}
	reqCtx, cancelReq := context.WithCancel(ctx)
	defer cancelReq()
	var silent atomic.Bool
	timer := time.AfterFunc(idle, func() { silent.Store(true); cancelReq() })
	defer timer.Stop()
	defer func() {
		if silent.Load() && ctx.Err() == nil {
			err = fmt.Errorf("no data from %s for %s", f.Node, idle)
		}
	}()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "http://"+f.Node+meshapi.PathMeshStream, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := f.Client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	body := &idleReader{r: resp.Body, timer: timer, idle: idle}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, ErrNoStream
	case resp.StatusCode != http.StatusOK:
		return false, fmt.Errorf("%s: HTTP %d", meshapi.PathMeshStream, resp.StatusCode)
	}
	if !send(ctx, out, Message{Connected: true}) {
		return true, ctx.Err()
	}
	err = parse(body, func(event string, data []byte) bool {
		var m Message
		switch event {
		case meshapi.SSECluster:
			var c meshapi.ClusterResponse
			if json.Unmarshal(data, &c) != nil {
				return true // a frame we cannot read is skipped, not fatal
			}
			m.Cluster = &c
		case meshapi.SSEActivity:
			var e meshapi.MeshEvent
			if json.Unmarshal(data, &e) != nil {
				return true
			}
			m.Event = &e
		default:
			return true // aliases, and any event added later
		}
		return send(ctx, out, m)
	})
	if err == nil {
		err = errors.New("stream closed")
	}
	return true, err
}

// parse calls frame for each complete server-sent event until the body ends or
// frame returns false.
func parse(r io.Reader, frame func(event string, data []byte) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), maxFrame)
	var event string
	var data []byte
	for sc.Scan() {
		line := sc.Bytes()
		switch {
		case len(line) == 0:
			if data != nil && !frame(event, data) {
				return context.Canceled
			}
			event, data = "", nil
		case line[0] == ':':
		case bytes.HasPrefix(line, []byte("event:")):
			event = strings.TrimSpace(string(line[len("event:"):]))
		case bytes.HasPrefix(line, []byte("data:")):
			d := bytes.TrimPrefix(line[len("data:"):], []byte(" "))
			if data != nil {
				data = append(data, '\n')
			}
			data = append(data, d...) // copies: the scanner reuses its buffer
		}
	}
	return sc.Err()
}

// idleReader restarts the silence timer whenever bytes arrive.
type idleReader struct {
	r     io.Reader
	timer *time.Timer
	idle  time.Duration
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.timer.Reset(r.idle)
	}
	return n, err
}

func send(ctx context.Context, out chan<- Message, m Message) bool {
	select {
	case out <- m:
		return true
	case <-ctx.Done():
		return false
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
