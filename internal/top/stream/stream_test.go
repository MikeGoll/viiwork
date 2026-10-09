package stream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

func frame(event string, v any) string {
	b, _ := json.Marshal(v)
	return fmt.Sprintf("event: %s\ndata: %s\n\n", event, b)
}

// server writes frames to each connection. Connections after the first get
// later[i]; a connection with no frames left blocks until the client leaves.
func server(t *testing.T, first string, closeFirst bool, later ...string) (*httptest.Server, *atomic.Int64) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != meshapi.PathMeshStream {
			http.NotFound(w, r)
			return
		}
		i := n.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		body := first
		if i > 1 {
			body = ""
			if int(i-2) < len(later) {
				body = later[i-2]
			}
		}
		fmt.Fprint(w, body)
		w.(http.Flusher).Flush()
		if i == 1 && closeFirst {
			return
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func collect(ctx context.Context, f *Follower) (<-chan Message, <-chan error) {
	out := make(chan Message, 64)
	errc := make(chan error, 1)
	go func() { errc <- f.Run(ctx, out) }()
	return out, errc
}

func next(t *testing.T, ch <-chan Message) Message {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("no message")
	}
	return Message{}
}

func noSleep(context.Context, time.Duration) error { return nil }

func TestFollowDecodesNamedEvents(t *testing.T) {
	c := meshapi.ClusterResponse{View: "node-a", Mesh: meshapi.MeshOpen}
	e := meshapi.MeshEvent{Event: meshapi.Event{Type: meshapi.EventRequest, RequestID: 7, Message: "m → m/0"}, Hostname: "node-a"}
	body := ": comment\n\n" + frame(meshapi.SSEActivity, e) + frame("aliases", map[string]any{}) + frame(meshapi.SSECluster, c)
	srv, _ := server(t, body, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, errc := collect(ctx, &Follower{Client: srv.Client(), Node: strings.TrimPrefix(srv.URL, "http://"), Sleep: noSleep})

	if m := next(t, out); !m.Connected {
		t.Fatalf("first message = %+v; want Connected", m)
	}
	if m := next(t, out); m.Event == nil || m.Event.RequestID != 7 {
		t.Fatalf("second = %+v; want the activity event", m)
	}
	if m := next(t, out); m.Cluster == nil || m.Cluster.View != "node-a" {
		t.Fatalf("third = %+v; want the cluster (the aliases frame is skipped)", m)
	}
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v; want context.Canceled", err)
	}
}

func TestFollowReconnectsWithBackoff(t *testing.T) {
	c := meshapi.ClusterResponse{View: "node-a"}
	srv, conns := server(t, frame(meshapi.SSECluster, c), true, "", frame(meshapi.SSECluster, c))
	var mu sync.Mutex
	var slept []time.Duration
	f := &Follower{Client: srv.Client(), Node: strings.TrimPrefix(srv.URL, "http://"), Sleep: func(_ context.Context, d time.Duration) error {
		mu.Lock()
		slept = append(slept, d)
		mu.Unlock()
		return nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, _ := collect(ctx, f)

	next(t, out) // connected
	next(t, out) // cluster
	if m := next(t, out); m.Lost == nil {
		t.Fatalf("after the server closed: %+v; want Lost", m)
	}
	if m := next(t, out); !m.Connected {
		t.Fatalf("want Connected again, got %+v", m)
	}
	if conns.Load() < 2 {
		t.Fatalf("connections = %d", conns.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(slept) == 0 || slept[0] != time.Second {
		t.Fatalf("backoff = %v; want 1s first", slept)
	}
}

func TestBackoffDoublesToTenSeconds(t *testing.T) {
	var slept []time.Duration
	f := &Follower{Client: &http.Client{}, Node: "127.0.0.1:1", Sleep: func(ctx context.Context, d time.Duration) error {
		slept = append(slept, d)
		if len(slept) == 6 {
			return context.Canceled
		}
		return nil
	}}
	out := make(chan Message, 64)
	if err := f.Run(context.Background(), out); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second}
	for i := range want {
		if slept[i] != want[i] {
			t.Fatalf("backoff = %v; want %v", slept, want)
		}
	}
}

func TestNoStreamOn404(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	f := &Follower{Client: srv.Client(), Node: strings.TrimPrefix(srv.URL, "http://"), Sleep: noSleep}
	if err := f.Run(context.Background(), make(chan Message, 8)); !errors.Is(err, ErrNoStream) {
		t.Fatalf("Run = %v; want ErrNoStream", err)
	}
}

// A node that hangs, or a host that loses power, sends nothing and closes
// nothing. The entry node's snapshot changes every second (its uptime ticks),
// so silence past the idle limit means the stream is dead, not quiet.
func TestSilentStreamIsLost(t *testing.T) {
	srv, _ := server(t, frame(meshapi.SSECluster, meshapi.ClusterResponse{View: "node-a"}), false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &Follower{Client: srv.Client(), Node: strings.TrimPrefix(srv.URL, "http://"), Sleep: noSleep, Idle: 200 * time.Millisecond}
	out, _ := collect(ctx, f)
	next(t, out) // connected
	next(t, out) // cluster
	start := time.Now()
	m := next(t, out)
	if m.Lost == nil || !strings.Contains(m.Lost.Error(), "no data") {
		t.Fatalf("got %+v; want Lost for silence", m)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("silence noticed after %v", d)
	}
}

// A node that accepts the connection but never answers is caught by the same
// limit, before any header arrives.
func TestNoHeadersIsLost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, _ := collect(ctx, &Follower{Client: srv.Client(), Node: strings.TrimPrefix(srv.URL, "http://"), Sleep: noSleep, Idle: 200 * time.Millisecond})
	if m := next(t, out); m.Lost == nil {
		t.Fatalf("got %+v; want Lost", m)
	}
}

func TestLargeFrame(t *testing.T) {
	// A big fleet's cluster snapshot is far over bufio.Scanner's 64 KiB default.
	c := meshapi.ClusterResponse{View: strings.Repeat("x", 1<<20)}
	srv, _ := server(t, frame(meshapi.SSECluster, c), false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, _ := collect(ctx, &Follower{Client: srv.Client(), Node: strings.TrimPrefix(srv.URL, "http://"), Sleep: noSleep})
	next(t, out)
	if m := next(t, out); m.Cluster == nil || len(m.Cluster.View) != 1<<20 {
		t.Fatalf("large frame not decoded: %+v", m.Lost)
	}
}
