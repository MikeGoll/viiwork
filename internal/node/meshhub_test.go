package node

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

// countingMemberStream is a fake member activity stream that counts the
// connections followers open to it.
type countingMemberStream struct {
	srv    *httptest.Server
	events chan string
	conns  atomic.Int64 // opened, ever
	active atomic.Int64 // open now
}

func newCountingMemberStream(t *testing.T) *countingMemberStream {
	t.Helper()
	m := &countingMemberStream{events: make(chan string, 16)}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != meshapi.PathActivityStream {
			http.NotFound(w, r)
			return
		}
		m.conns.Add(1)
		m.active.Add(1)
		defer m.active.Add(-1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case msg := <-m.events:
				fmt.Fprintf(w, "data: %s\n\n", msg)
				w.(http.Flusher).Flush()
			}
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func withMember(t *testing.T, fx *streamFx, name string, s *httptest.Server) {
	fx.set(func() {
		fx.members = append(fx.members, memberAt(t, name, s, meshapi.MemberAlive, meshapi.RoleNode, false))
		fx.cluster.Members = append(fx.cluster.Members, meshapi.Member{Node: name, State: meshapi.MemberAlive, Role: meshapi.RoleNode,
			Status: &meshapi.NodeStatus{Node: name, NodeID: "viiwork-" + name}})
	})
}

func TestMeshHubSharesOneFollowerPerMember(t *testing.T) {
	d, _, fx := newStreamDeps(t)
	gb2 := newCountingMemberStream(t)
	withMember(t, fx, "gb2", gb2.srv)
	h := NewServer(d)

	first, stopFirst := openStream(t, h)
	second, stopSecond := openStream(t, h)
	eventually(t, 3*time.Second, "a follower connects", func() bool { return gb2.active.Load() == 1 })
	time.Sleep(1500 * time.Millisecond) // more than one snapshot tick
	if n := gb2.conns.Load(); n != 1 {
		t.Fatalf("two viewers opened %d streams to one member, want 1", n)
	}

	gb2.events <- `{"t":1,"type":"request","message":"m → m/1","rid":9}`
	for i, events := range []<-chan sseEvent{first, second} {
		ev := waitEvent(t, events, 3*time.Second, func(e sseEvent) bool {
			return e.name == meshapi.SSEActivity && strings.Contains(e.data, `"rid":9`)
		})
		var got meshapi.MeshEvent
		if json.Unmarshal([]byte(ev.data), &got) != nil || got.Hostname != "gb2" || got.NodeID != "viiwork-gb2" {
			t.Errorf("viewer %d: %s", i, ev.data)
		}
	}

	stopFirst()
	time.Sleep(200 * time.Millisecond)
	if gb2.active.Load() != 1 {
		t.Error("the follower must outlive a viewer while another still watches")
	}
	stopSecond()
	eventually(t, 3*time.Second, "the hub stops following once nobody watches", func() bool { return gb2.active.Load() == 0 })
}

// stalledWriter is a ResponseWriter whose writes block until released: a
// connected client that stopped reading.
type stalledWriter struct {
	hdr     http.Header
	release chan struct{}
}

func (s *stalledWriter) Header() http.Header         { return s.hdr }
func (s *stalledWriter) WriteHeader(int)             {}
func (s *stalledWriter) Flush()                      {}
func (s *stalledWriter) Write(b []byte) (int, error) { <-s.release; return len(b), nil }

func TestMeshHubStalledViewerDoesNotBlockOthers(t *testing.T) {
	d, _, _ := newStreamDeps(t)
	h := NewServer(d)

	stalled := &stalledWriter{hdr: http.Header{}, release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stalledDone := make(chan struct{})
	go func() {
		h.ServeHTTP(stalled, httptest.NewRequest(http.MethodGet, meshapi.PathMeshStream, nil).WithContext(ctx))
		close(stalledDone)
	}()
	released := false
	defer func() {
		if !released {
			close(stalled.release)
		}
	}()

	healthy, _ := openStream(t, h)
	waitEvent(t, healthy, 3*time.Second, func(e sseEvent) bool { return e.name == meshapi.SSECluster })

	// Far more than one viewer's buffer, paced so the local log's own
	// subscriber buffer never overflows.
	const n = 2 * viewerBuffer
	go func() {
		for i := 1; i <= n; i++ {
			d.Activity.EmitRequest(int64(1000+i), -1, "burst")
			if i%16 == 0 {
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()
	waitEvent(t, healthy, 5*time.Second, func(e sseEvent) bool {
		return e.name == meshapi.SSEActivity && strings.Contains(e.data, fmt.Sprintf(`"rid":%d`, 1000+n))
	})

	close(stalled.release)
	released = true
	select {
	case <-stalledDone:
	case <-time.After(3 * time.Second):
		t.Fatal("the stalled viewer was not dropped: its handler is still running")
	}
}

func TestMeshHubNewViewerGetsSnapshotAndReplay(t *testing.T) {
	d, _, fx := newStreamDeps(t)
	gb2 := newCountingMemberStream(t)
	withMember(t, fx, "gb2", gb2.srv)
	resolved := "m"
	fx.aliases = meshapi.AliasesResponse{Aliases: []meshapi.AliasInfo{{Name: "stable", Target: "m", Fallbacks: []string{}, Resolved: &resolved, State: "ok"}}}
	d.Activity.EmitRequest(7, -1, "m → m/0")
	h := NewServer(d)

	first, _ := openStream(t, h)
	eventually(t, 3*time.Second, "a follower connects", func() bool { return gb2.active.Load() == 1 })
	gb2.events <- `{"t":1,"type":"request","message":"m → m/1","rid":9}`
	waitEvent(t, first, 3*time.Second, func(e sseEvent) bool {
		return e.name == meshapi.SSEActivity && strings.Contains(e.data, `"rid":9`)
	})

	// The second viewer joins a running hub: no new member connection, and
	// everything a fresh stream used to deliver arrives at once.
	second, _ := openStream(t, h)
	got := collect(second, 500*time.Millisecond)
	var local, remote, cluster, aliases bool
	for _, e := range got {
		switch e.name {
		case meshapi.SSECluster:
			cluster = cluster || strings.Contains(e.data, `"gb2"`)
		case sseAliases:
			aliases = aliases || strings.Contains(e.data, "stable")
		case meshapi.SSEActivity:
			var ev meshapi.MeshEvent
			if json.Unmarshal([]byte(e.data), &ev) != nil {
				t.Fatalf("bad activity %s", e.data)
			}
			if ev.RequestID == 7 && ev.Hostname == "gb1" && ev.Replay {
				local = true
			}
			if ev.RequestID == 9 && ev.Hostname == "gb2" && ev.NodeID == "viiwork-gb2" && ev.Replay {
				remote = true
			}
		}
	}
	if !local || !remote || !cluster || !aliases {
		t.Errorf("new viewer: local replay %v, member replay %v, cluster %v, aliases %v; got %v", local, remote, cluster, aliases, got)
	}
	if n := gb2.conns.Load(); n != 1 {
		t.Errorf("a second viewer opened another member stream: %d connections", n)
	}
}

func TestMeshHubStopsWithStreamCtx(t *testing.T) {
	d, sf, fx := newStreamDeps(t)
	gb2 := newCountingMemberStream(t)
	withMember(t, fx, "gb2", gb2.srv)
	events, _ := openStream(t, NewServer(d))
	eventually(t, 3*time.Second, "a follower connects", func() bool { return gb2.active.Load() == 1 })
	sf.cancel()
	collect(events, time.Second) // drains until the stream ends
	eventually(t, 3*time.Second, "the follower is cancelled at shutdown", func() bool { return gb2.active.Load() == 0 })
}

func TestReconnectBackoff(t *testing.T) {
	var b reconnectBackoff
	var got []time.Duration
	for i := 0; i < 7; i++ {
		got = append(got, b.after(false, 0))
	}
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30}
	for i := range want {
		if got[i] != want[i]*time.Second {
			t.Fatalf("failing reconnects waited %v, want %v seconds", got, want)
		}
	}
	// A stream that was accepted but died at once is still a failure.
	if d := b.after(true, time.Second); d != followerMaxBackoff {
		t.Errorf("a short-lived stream reset the backoff to %v", d)
	}
	// One that lived long enough is healthy: the next attempt starts over.
	if d := b.after(true, followerStableAfter); d != followerMinBackoff {
		t.Errorf("after a stable stream the backoff is %v, want %v", d, followerMinBackoff)
	}
	if d := b.after(false, 0); d != 2*followerMinBackoff {
		t.Errorf("then doubles again: %v", d)
	}
}

func TestMirrorReplayRule(t *testing.T) {
	now := time.Now()
	old, recent := now.Add(-time.Minute), now.Add(-time.Second)
	ev := func(at time.Time, rid int64, msg string) mirroredEvent {
		return mirroredEvent{at: at, ev: meshapi.MeshEvent{Event: meshapi.Event{RequestID: rid, Message: msg}}}
	}
	events := []mirroredEvent{
		ev(old, 0, "backend healthy"), // old, not a request: dropped
		ev(old, 1, "m → m/0"),         // old, finished below: dropped
		ev(old, 1, meshapi.RequestDone("m", "m/0", time.Second)),
		ev(old, 2, "m → m/1"),            // old but still running: kept
		ev(recent, 0, "backend loading"), // recent: kept
	}
	got := replayable(events, now)
	if len(got) != 2 || got[0].ev.RequestID != 2 || got[1].ev.Message != "backend loading" {
		t.Errorf("replayable = %+v", got)
	}

	m := &memberMirror{}
	for i := 0; i < mirrorCap+10; i++ {
		m.add(now, meshapi.MeshEvent{Event: meshapi.Event{RequestID: int64(i + 1), Message: "m → m/0"}})
	}
	if len(m.events) != mirrorCap || m.events[len(m.events)-1].ev.RequestID != mirrorCap+10 {
		t.Errorf("mirror holds %d events ending at %d, want the newest %d", len(m.events), m.events[len(m.events)-1].ev.RequestID, mirrorCap)
	}
}
