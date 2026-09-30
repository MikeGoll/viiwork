package node

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/janit/viiwork/v2/internal/activity"
	"github.com/janit/viiwork/v2/internal/logging"
	"github.com/janit/viiwork/v2/mesh"
	"github.com/janit/viiwork/v2/mesh/meshclient"
	"github.com/janit/viiwork/v2/meshapi"
)

// clusterPushInterval is how often the snapshot is rebuilt and compared. It is
// a server-side heartbeat shared by all viewers, not a per-client poll: at one
// second the view tracks in-flight changes closely while an idle mesh sends
// nothing at all, because identical snapshots are suppressed.
const clusterPushInterval = time.Second

// viewerBuffer is how many frames the hub may queue for one viewer whose
// handler is still writing earlier ones. A viewer that falls this far behind
// is dropped rather than waited for: the hub serves every viewer, and one
// stalled browser must not hold up the rest. The dropped browser reconnects
// and rebuilds from the replay, which is exactly what it would do after any
// other disconnect.
const viewerBuffer = 256

// mirrorCap bounds the events the hub keeps per member to replay to a viewer
// that connects later. Older events past the replay window are pruned first.
const mirrorCap = 1024

// Follower reconnect backoff. A member that is down is retried with a delay
// that doubles up to followerMaxBackoff; a stream that was accepted and lived
// at least followerStableAfter counts as healthy, so its next reconnect starts
// from followerMinBackoff again instead of from wherever an outage long ago
// left the delay.
const (
	followerMinBackoff  = time.Second
	followerMaxBackoff  = 30 * time.Second
	followerStableAfter = 10 * time.Second
)

// followerClient carries the long-lived member activity streams. It has no
// overall timeout, which would cut every stream off; the dial and the wait for
// headers are bounded, and ctx cancellation ends a stream. The member's
// /v1/activity/stream answers headers as soon as its backlog is written.
var followerClient = meshclient.New(meshclient.Options{DialTimeout: 2 * time.Second, ResponseHeaderTimeout: 10 * time.Second})

// sseAliases is the third named event on the mesh stream (P6 Decision 2),
// carrying a meshapi.AliasesResponse. The frozen meshapi names only the first
// two, so the name lives here.
const sseAliases = "aliases"

// sseFrame is one encoded event on its way to a viewer's single writer.
type sseFrame struct {
	event string
	data  []byte
}

// handleMeshStream serves everything the mesh view needs over ONE held-open
// connection: activity events from this node and every alive member, plus
// cluster and alias snapshots pushed when they change.
//
// Why SSE rather than WebSockets: viiwork is deliberately close to stdlib-only,
// and Go has no WebSocket in the standard library, so that would mean taking on
// a dependency for a strictly one-way feed. SSE is the same held-open TCP socket
// with server push, needs no handshake library, and reconnects on its own in
// the browser.
//
// Events are NAMED so one connection can carry every kind:
//
//	event: activity  -> a MeshEvent
//	event: cluster   -> a full ClusterResponse snapshot
//	event: aliases   -> an AliasesResponse
//
// Why aggregate server-side rather than let the page open one EventSource per
// host: the browser viewing /mesh may not be able to reach every member
// directly, and EventSource is subject to CORS, which the per-node activity
// endpoint does not set. Fanning out here keeps the mesh view working from any
// single reachable node.
//
// The fan-out itself is shared: every viewer of this node subscribes to one
// meshHub, which holds one follower per member and one snapshot loop. Were it
// per viewer, each open dashboard would hold a stream to every member, and a
// member caps its activity subscribers — past a dozen viewers fleet-wide the
// followers would evict one another and replay their backlogs in a loop.
func (s *server) handleMeshStream(w http.ResponseWriter, r *http.Request) {
	if s.d.Activity == nil {
		http.Error(w, "activity log unavailable", http.StatusServiceUnavailable)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// The stream ends when the client leaves or the node shuts down (P6
	// Decision 7).
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stopOnShutdown := context.AfterFunc(s.d.StreamCtx, cancel)
	defer stopOnShutdown()

	// Every byte of this response is written by this goroutine and no other.
	//
	// The hub's producers -- the member followers, the local subscription and
	// the snapshot loop -- used to write to w directly under a mutex, which
	// made those writes mutually exclusive but said nothing about when they
	// *stop*. The handler can return while a producer is still inside Fprintf,
	// and writing to an http.ResponseWriter after ServeHTTP returns is a data
	// race and a violation of net/http's contract. It reproduced under -race.
	//
	// Waiting for the producers instead would deadlock. An SSE response must not
	// carry a WriteTimeout, so a connected-but-stalled client can block a write
	// indefinitely. So producers hand already-encoded frames to this handler over
	// a bounded channel and never hold a reference to w; the hub never blocks on
	// a viewer either, and drops one whose channel is full.
	v, initial := s.joinHub()
	defer s.leaveHub(v)

	write := func(f sseFrame) bool {
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", f.event, f.data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// The replay and the current snapshots, captured atomically with the
	// subscription: anything later is in v.frames.
	for _, f := range initial {
		if !write(f) {
			return
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-v.dropped:
			// The hub dropped this viewer for falling viewerBuffer frames
			// behind. The connection may be healthy, but its view now has a
			// gap; ending it makes the browser reconnect and rebuild from the
			// replay, which repairs the gap.
			return
		case f := <-v.frames:
			if !write(f) {
				return
			}
		}
	}
}

// hubs holds the running meshHub of each server. A hub is started by the first
// viewer and stopped when the last one leaves, so an unwatched node follows
// nobody.
var hubs = struct {
	mu sync.Mutex
	m  map[*server]*meshHub
}{m: map[*server]*meshHub{}}

// viewer is one mesh stream's subscription to the hub.
type viewer struct {
	hub     *meshHub
	frames  chan sseFrame
	dropped chan struct{} // closed when the hub drops this viewer
}

func (s *server) joinHub() (*viewer, []sseFrame) {
	hubs.mu.Lock()
	defer hubs.mu.Unlock()
	h := hubs.m[s]
	if h == nil || h.ctx.Err() != nil {
		h = startMeshHub(s)
		hubs.m[s] = h
	}
	v := &viewer{hub: h, frames: make(chan sseFrame, viewerBuffer), dropped: make(chan struct{})}
	return v, h.add(v)
}

func (s *server) leaveHub(v *viewer) {
	hubs.mu.Lock()
	defer hubs.mu.Unlock()
	if v.hub.remove(v) == 0 {
		v.hub.cancel()
		if hubs.m[s] == v.hub {
			delete(hubs.m, s)
		}
	}
}

// meshHub is the node's one fan-out behind /v1/mesh/stream: one subscription to
// the local activity log, one follower per alive member, and one snapshot loop
// that diffs the cluster and alias snapshots. Each produces encoded frames
// once, which the hub hands to every viewer's channel without blocking.
//
// Its context derives from the server's StreamCtx, so the node's shutdown ends
// the hub along with every stream, in the documented order.
type meshHub struct {
	s                  *server
	ctx                context.Context
	cancel             context.CancelFunc
	localID, localHost string

	mu      sync.Mutex
	viewers map[*viewer]struct{}
	cluster []byte // last pushed snapshot; nil until the first
	aliases []byte
	mirrors map[string]*memberMirror // member name -> its recent events
}

func startMeshHub(s *server) *meshHub {
	ctx, cancel := context.WithCancel(s.d.StreamCtx)
	h := &meshHub{
		s: s, ctx: ctx, cancel: cancel, localHost: s.d.Self,
		viewers: map[*viewer]struct{}{}, mirrors: map[string]*memberMirror{},
	}
	if s.d.Status != nil {
		h.localID = s.d.Status().NodeID
	}
	// Subscribed before any viewer reads the backlog, so nothing falls between
	// a viewer's replay and its live feed; the overlap this creates instead is
	// deduplicated downstream.
	sub := s.d.Activity.Subscribe()
	go h.localLoop(sub)
	go h.snapshotLoop()
	return h
}

// add registers v and returns what a fresh viewer needs first: the local
// replay, each member's replay, then the current snapshots. It runs under the
// lock every broadcast takes, so each frame is either in this list or queued
// on v.frames -- never neither.
func (h *meshHub) add(v *viewer) []sseFrame {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.viewers[v] = struct{}{}
	initial := h.localBacklog()
	now := time.Now()
	for _, m := range h.mirrors {
		for _, ev := range replayable(m.events, now) {
			e := ev.ev
			e.Replay = true
			if b, err := json.Marshal(e); err == nil {
				initial = append(initial, sseFrame{meshapi.SSEActivity, b})
			}
		}
	}
	if h.cluster != nil {
		initial = append(initial, sseFrame{meshapi.SSECluster, h.cluster})
	}
	if h.aliases != nil {
		initial = append(initial, sseFrame{sseAliases, h.aliases})
	}
	return initial
}

// remove unregisters v and returns how many viewers remain.
func (h *meshHub) remove(v *viewer) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.viewers, v)
	return len(h.viewers)
}

// broadcast hands f to every viewer without blocking. Callers hold h.mu.
func (h *meshHub) broadcast(f sseFrame) {
	for v := range h.viewers {
		select {
		case v.frames <- f:
		default:
			delete(h.viewers, v)
			close(v.dropped)
		}
	}
}

func (h *meshHub) localEvent(ev meshapi.Event) []byte {
	b, err := json.Marshal(meshapi.MeshEvent{Event: ev, NodeID: h.localID, Hostname: h.localHost})
	if err != nil {
		return nil
	}
	return b
}

// localBacklog is the local log's replay as frames. Callers hold h.mu.
func (h *meshHub) localBacklog() []sseFrame {
	var out []sseFrame
	for _, ev := range h.s.d.Activity.Backlog() {
		if b := h.localEvent(ev); b != nil {
			out = append(out, sseFrame{meshapi.SSEActivity, b})
		}
	}
	return out
}

// localLoop re-tags this node's activity with its name and fans it out.
func (h *meshHub) localLoop(sub chan []byte) {
	alog := h.s.d.Activity
	defer func() { alog.Unsubscribe(sub) }()
	for {
		select {
		case <-h.ctx.Done():
			return
		case raw, ok := <-sub:
			if !ok {
				// The log evicted this subscriber to make room for another.
				// Resubscribe, then replay: an event emitted in between would
				// otherwise be lost to every viewer, and a lost "done" strands
				// an in-flight row. The replay covers the gap, and viewers
				// deduplicate the overlap.
				sub = alog.Subscribe()
				h.mu.Lock()
				for _, f := range h.localBacklog() {
					h.broadcast(f)
				}
				h.mu.Unlock()
				continue
			}
			// Subscribe delivers already-encoded JSON, so it has to be decoded
			// to re-tag it with the node -- once for every viewer. Activity
			// events are low-rate, so this is not a hot path.
			var ev meshapi.Event
			if err := json.Unmarshal(raw, &ev); err != nil {
				continue
			}
			if b := h.localEvent(ev); b != nil {
				h.mu.Lock()
				h.broadcast(sseFrame{meshapi.SSEActivity, b})
				h.mu.Unlock()
			}
		}
	}
}

// snapshotLoop pushes the cluster and alias snapshots when they change, and
// keeps the member followers in step with the member list. Snapshots are
// pushed on change rather than on a timer the client drives, so the browser
// never polls. The diff matters: in-flight counts and GPU load change
// constantly, but re-sending an identical snapshot every tick would be the
// same waste as polling, just moved server-side. The same tick picks up
// members that joined and stops following members that are gone (P6
// Decision 8).
func (h *meshHub) snapshotLoop() {
	followers := newMemberFollowers(h.ctx, h)
	deadband := newHostMemDeadband()
	ticker := time.NewTicker(clusterPushInterval)
	defer ticker.Stop()
	for {
		state := h.s.d.Cluster()
		followers.update(h.s.members(), nodeIDs(state))
		deadband.apply(&state)
		if b, err := json.Marshal(state); err == nil {
			h.publish(meshapi.SSECluster, b)
		}
		if h.s.d.AliasInfo != nil {
			if b, err := json.Marshal(h.s.d.AliasInfo()); err == nil {
				h.publish(sseAliases, b)
			}
		}
		select {
		case <-h.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// publish broadcasts a snapshot if its bytes differ from the last one pushed.
// The bytes compared are the bytes sent.
func (h *meshHub) publish(event string, b []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	last := &h.cluster
	if event == sseAliases {
		last = &h.aliases
	}
	if bytes.Equal(*last, b) {
		return
	}
	*last = b
	h.broadcast(sseFrame{event, b})
}

// memberConnected starts a fresh mirror for a member whose stream was just
// accepted: the member replays its backlog first, so the mirror is rebuilt from
// that replay rather than carried across a gap it did not see.
func (h *meshHub) memberConnected(name string, owner uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.mirrors[name] = &memberMirror{owner: owner}
}

// memberDisconnected drops a member's mirror. While nobody follows the member,
// a new viewer gets no replay of it -- under-reporting, never a start whose
// done fell in the gap.
func (h *meshHub) memberDisconnected(name string, owner uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if m, ok := h.mirrors[name]; ok && m.owner == owner {
		delete(h.mirrors, name)
	}
}

// memberEvent records and fans out one event from a member's stream.
func (h *meshHub) memberEvent(name string, owner uint64, ev meshapi.MeshEvent) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if m, ok := h.mirrors[name]; ok && m.owner == owner {
		m.add(time.Now(), ev)
	}
	h.broadcast(sseFrame{meshapi.SSEActivity, b})
}

// memberMirror is the recent part of one member's activity, as this hub
// received it, so a viewer connecting later gets the replay the member itself
// would have sent a fresh connection.
type memberMirror struct {
	owner  uint64 // the follower that fills it
	events []mirroredEvent
}

type mirroredEvent struct {
	at time.Time // when this node received it
	ev meshapi.MeshEvent
}

func (m *memberMirror) add(now time.Time, ev meshapi.MeshEvent) {
	m.events = append(m.events, mirroredEvent{at: now, ev: ev})
	if len(m.events) <= mirrorCap {
		return
	}
	m.events = replayable(m.events, now)
	if n := len(m.events) - mirrorCap; n > 0 {
		// Dropping oldest first loses a start before its done: a viewer then
		// under-reports a request, never invents one.
		m.events = append([]mirroredEvent(nil), m.events[n:]...)
	}
}

// replayable applies activity.Log's replay rule to a mirror: every event from
// the last ReplayWindow, and from before it, the events of requests with no
// terminal event yet. Age is measured by this node's clock at receipt, which
// only ever keeps an event longer than the member would, never shorter.
func replayable(events []mirroredEvent, now time.Time) []mirroredEvent {
	cutoff := now.Add(-activity.ReplayWindow)
	var ended map[int64]bool
	for _, e := range events {
		if e.ev.RequestID != 0 && meshapi.IsRequestTerminal(e.ev.Message) {
			if ended == nil {
				ended = make(map[int64]bool)
			}
			ended[e.ev.RequestID] = true
		}
	}
	out := make([]mirroredEvent, 0, len(events))
	for _, e := range events {
		if e.at.Before(cutoff) && (e.ev.RequestID == 0 || ended[e.ev.RequestID]) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func (s *server) members() []mesh.Member {
	if s.d.Members == nil {
		return nil
	}
	return s.d.Members()
}

// nodeIDs maps member names to the node_id in their last status.
func nodeIDs(c meshapi.ClusterResponse) map[string]string {
	ids := make(map[string]string, len(c.Members))
	for _, m := range c.Members {
		if m.Status != nil {
			ids[m.Node] = m.Status.NodeID
		}
	}
	return ids
}

// memberFollowers runs one activity follower per alive node member.
type memberFollowers struct {
	ctx context.Context
	hub *meshHub

	mu      sync.Mutex
	ids     map[string]string // member name -> node_id, refreshed each tick
	running map[string]followerHandle
	nextID  uint64
}

type followerHandle struct {
	addr   string
	cancel context.CancelFunc
}

func newMemberFollowers(ctx context.Context, hub *meshHub) *memberFollowers {
	return &memberFollowers{ctx: ctx, hub: hub, ids: map[string]string{}, running: map[string]followerHandle{}}
}

// update starts followers for alive, non-local node members, and stops those
// whose member is gone or has moved to another address.
func (f *memberFollowers) update(members []mesh.Member, ids map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids = ids
	want := map[string]string{}
	for _, m := range members {
		if m.State == meshapi.MemberAlive && !m.Local && m.Meta.Role == meshapi.RoleNode {
			want[m.Name] = m.APIAddr()
		}
	}
	for name, h := range f.running {
		if addr, ok := want[name]; !ok || addr != h.addr {
			h.cancel()
			delete(f.running, name)
		}
	}
	for name, addr := range want {
		if _, ok := f.running[name]; ok {
			continue
		}
		ctx, cancel := context.WithCancel(f.ctx)
		f.running[name] = followerHandle{addr: addr, cancel: cancel}
		f.nextID++
		go f.follow(ctx, f.nextID, name, addr)
	}
}

func (f *memberFollowers) nodeID(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ids[name]
}

// reconnectBackoff is the delay before a follower's next connection attempt.
type reconnectBackoff struct{ next time.Duration }

// after returns how long to wait given how the last stream went: accepted
// (HTTP 200) or not, and how long it lived. A healthy stream resets the delay.
func (b *reconnectBackoff) after(accepted bool, lived time.Duration) time.Duration {
	if b.next == 0 || (accepted && lived >= followerStableAfter) {
		b.next = followerMinBackoff
	}
	d := b.next
	b.next = min(2*b.next, followerMaxBackoff)
	return d
}

// follow streams one member's activity until ctx ends, reconnecting with
// backoff, so a member that is down or restarting does not take the mesh view
// down with it.
func (f *memberFollowers) follow(ctx context.Context, id uint64, name, addr string) {
	var backoff reconnectBackoff
	for {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		accepted := f.streamOnce(ctx, id, name, addr)
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff.after(accepted, time.Since(start))):
		}
	}
}

// streamOnce connects once and pumps events until the stream ends. It reports
// whether the member accepted the stream.
func (f *memberFollowers) streamOnce(ctx context.Context, id uint64, name, addr string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+meshapi.PathActivityStream, nil)
	if err != nil {
		return false
	}
	resp, err := followerClient.Do(req)
	if err != nil {
		if logging.DebugEnabled() {
			log.Printf("[debug] mesh activity: member %s (%s) unreachable: %v", name, addr, err)
		}
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	f.hub.memberConnected(name, id)
	defer f.hub.memberDisconnected(name, id)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev meshapi.Event
		if err := json.Unmarshal([]byte(line[6:]), &ev); err != nil {
			continue
		}
		f.hub.memberEvent(name, id, meshapi.MeshEvent{Event: ev, NodeID: f.nodeID(name), Hostname: name, Addr: addr})
	}
	return true
}

// hostMemBuckets is how many distinct levels of host memory survive into the
// pushed snapshot. 64 puts a step at ~1 GB on a 64 GB host and ~2 GB on a
// 128 GB one, which is under a pixel on the strip that renders it.
const hostMemBuckets = 64

// hostMemDeadband coarsens host memory before the snapshot is diffed.
//
// An exact figure ticks every second on a live host (measured on gb1: ~86 MB
// of movement per second, spiking past 600 MB) and defeats the change detection
// in the push loop -- the stream would send a full snapshot every second
// forever, every one differing only in host_mem_used_mb. The mesh view's
// per-host memory strip needs about a hundred levels, not a megabyte, so the
// field is made exactly as precise as the only thing that reads it.
//
// Rounding alone is not enough: a host sitting near a bucket boundary flips
// between two levels on every tick and pushes just as hard as before. So the
// published value is held until the reading drifts a full step away from it.
// That is a deadband, and it is what makes the suppression hold for a host
// whose memory hovers rather than moves.
//
// State is per hub, keyed by member name, and /v1/cluster keeps the exact
// figures for anything that needs them.
type hostMemDeadband struct{ published map[string]int64 }

func newHostMemDeadband() *hostMemDeadband {
	return &hostMemDeadband{published: make(map[string]int64)}
}

func (d *hostMemDeadband) apply(state *meshapi.ClusterResponse) {
	for i := range state.Members {
		m := &state.Members[i]
		if m.Status == nil {
			continue
		}
		st := *m.Status // the caller's status is not changed
		st.HostMemUsedMB = d.value(m.Node, st.HostMemUsedMB, st.HostMemTotalMB)
		st.Models = withoutBaselineAge(st.Models)
		m.Status = &st
	}
}

// withoutBaselineAge drops each score's baseline age from the snapshot. The
// age ticks every second while a window holds fewer than perf.K samples, and
// like exact host memory it would make every snapshot differ from the last,
// so the stream would push a full cluster snapshot every second forever. The
// mesh view does not show it; /v1/status and /v1/cluster keep it. The
// caller's models are copied, never changed.
func withoutBaselineAge(models []meshapi.ModelStatus) []meshapi.ModelStatus {
	var out []meshapi.ModelStatus
	for i, ms := range models {
		if ms.Perf == nil || ms.Perf.BaselineAgeS == 0 {
			continue
		}
		if out == nil {
			out = append([]meshapi.ModelStatus(nil), models...)
		}
		p := *ms.Perf
		p.BaselineAgeS = 0
		out[i].Perf = &p
	}
	if out == nil {
		return models
	}
	return out
}

func (d *hostMemDeadband) value(key string, usedMB, totalMB int64) int64 {
	// A node that reports no total -- an unreadable /proc/meminfo -- has
	// nothing to scale a step from, so it passes through.
	if usedMB <= 0 || totalMB <= 0 {
		return usedMB
	}
	step := totalMB / hostMemBuckets
	if step < 1 {
		return usedMB
	}
	if prev, ok := d.published[key]; ok && absInt64(usedMB-prev) < step {
		return prev
	}
	v := (usedMB + step/2) / step * step
	d.published[key] = v
	return v
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
