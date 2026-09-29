// Package top is `viiwork top`: a live terminal view of the whole mesh, fed by
// one node's /v1/mesh/stream. State and rendering are pure; only cmd.go and
// the term package touch a terminal.
package top

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

// historyLen is how many GPU samples the host screen graphs.
const historyLen = 60

// Sample is one GPU reading kept for the history graph.
type Sample struct {
	Util    float64 // percent
	VRAMPct float64 // percent of total; 0 when the total is unknown
}

// Flight is one request in flight, rebuilt from activity events: a start adds
// it, a terminal event removes it. There is no server-side registry of jobs.
type Flight struct {
	Origin  string // the node that logged the request
	RID     int64  // per process, so only meaningful with Origin
	Model   string
	Exec    string // the node executing it: Origin, or the peer it went to
	Forward bool
	Start   time.Time

	proc string // the origin's node_id when the event carried one: its process
}

type gpuKey struct {
	node  string
	index int
}

type rateKey struct{ node, model string }

// rate turns a cumulative token counter into tokens per second. Snapshots
// carry no timestamp, so time is measured here, between the two snapshots
// that carried a change.
type rate struct {
	tokens   uint64
	uptime   int64
	at       time.Time // when tokens last changed, or the baseline was taken
	interval time.Duration
	perSec   float64
	known    bool
}

type flightKey struct {
	node string
	rid  int64
}

// State is everything the screens are drawn from. It does no I/O.
type State struct {
	Cluster   *meshapi.ClusterResponse
	Connected bool
	Err       string

	history   map[gpuKey][]Sample
	lastGPUs  map[string]string
	rates     map[rateKey]*rate
	lastAlive map[string]time.Time
	uptimes   map[string]int64
	flights   map[flightKey]Flight
}

func NewState() *State {
	return &State{
		history:   map[gpuKey][]Sample{},
		lastGPUs:  map[string]string{},
		rates:     map[rateKey]*rate{},
		lastAlive: map[string]time.Time{},
		uptimes:   map[string]int64{},
		flights:   map[flightKey]Flight{},
	}
}

// Apply takes a cluster snapshot received at now.
func (s *State) Apply(c meshapi.ClusterResponse, now time.Time) {
	s.Cluster = &c
	present, alive := map[string]bool{}, map[string]bool{}
	procs, restarted := map[string]string{}, map[string]bool{}
	for _, m := range c.Members {
		present[m.Node] = true
		if m.State == meshapi.MemberAlive {
			alive[m.Node] = true
			s.lastAlive[m.Node] = now
		}
		if m.Status == nil {
			continue
		}
		procs[m.Node] = m.Status.NodeID
		if up, ok := s.uptimes[m.Node]; ok && m.Status.UptimeS < up {
			restarted[m.Node] = true
		}
		s.uptimes[m.Node] = m.Status.UptimeS
		s.recordGPUs(m.Node, m.Status.GPUs)
		for _, ms := range m.Status.Models {
			s.recordTokens(rateKey{m.Node, ms.Name}, ms.TokensTotal, m.Status.UptimeS, now)
		}
	}
	for k := range s.history {
		if !present[k.node] {
			delete(s.history, k)
		}
	}
	for n := range s.lastGPUs {
		if !present[n] {
			delete(s.lastGPUs, n)
		}
	}
	for k := range s.rates {
		if !present[k.node] {
			delete(s.rates, k)
		}
	}
	for n := range s.uptimes {
		if !present[n] {
			delete(s.uptimes, n)
		}
	}
	// A request whose origin is no longer alive will never see its terminal
	// event; keeping it would show work that is not happening. Neither will
	// one whose origin is alive but is a different process: a member that
	// crashed and came back under the same name can do so between two
	// snapshots and never be seen as anything but alive. A new node_id says
	// so, or, for a request whose event carried none, uptime going backwards.
	// A request the new process started before this snapshot arrived may go
	// too, which under-reports for a moment rather than inventing work.
	for k, f := range s.flights {
		id := procs[f.Origin]
		replaced := id != "" && f.proc != "" && id != f.proc
		if !alive[f.Origin] || restarted[f.Origin] || replaced {
			delete(s.flights, k)
		}
	}
	s.Tick(now)
}

// recordGPUs appends a sample per GPU when the member's readings changed.
// Remote members refresh every 5 s while snapshots arrive every second, so
// appending on every snapshot would stretch one reading into five samples.
func (s *State) recordGPUs(node string, gpus []meshapi.GPUInfo) {
	var fp strings.Builder
	for _, g := range gpus {
		fmt.Fprintf(&fp, "%d:%g:%g:%g;", g.Index, g.Util, g.VRAMUsedMB, g.VRAMTotalMB)
	}
	if s.lastGPUs[node] == fp.String() {
		return
	}
	s.lastGPUs[node] = fp.String()
	for _, g := range gpus {
		smp := Sample{Util: g.Util}
		if g.VRAMTotalMB > 0 {
			smp.VRAMPct = g.VRAMUsedMB / g.VRAMTotalMB * 100
		}
		k := gpuKey{node, g.Index}
		h := append(s.history[k], smp)
		if len(h) > historyLen {
			h = h[len(h)-historyLen:]
		}
		s.history[k] = h
	}
}

func (s *State) recordTokens(k rateKey, tokens uint64, uptime int64, now time.Time) {
	r, ok := s.rates[k]
	if !ok {
		s.rates[k] = &rate{tokens: tokens, uptime: uptime, at: now}
		return
	}
	if uptime < r.uptime || tokens < r.tokens {
		// The member restarted and its counters reset: this pair measures
		// nothing, so start again from here rather than show a negative rate.
		*r = rate{tokens: tokens, uptime: uptime, at: now}
		return
	}
	r.uptime = uptime
	if tokens == r.tokens {
		return
	}
	d := now.Sub(r.at)
	if d <= 0 {
		return
	}
	r.perSec = float64(tokens-r.tokens) / d.Seconds()
	r.interval = d
	r.known = true
	r.tokens = tokens
	r.at = now
}

// Tick decays every rate whose counter has not moved for more than two of its
// own intervals. The stream pushes a snapshot only when its bytes change, so
// an idle mesh sends none and the decay has to run on the clock.
func (s *State) Tick(now time.Time) {
	for _, r := range s.rates {
		if r.known && now.Sub(r.at) > 2*r.interval {
			r.perSec = 0
		}
	}
}

// Rate is the model's tokens per second on node, and whether it is known.
func (s *State) Rate(node, model string) (float64, bool) {
	r, ok := s.rates[rateKey{node, model}]
	if !ok || !r.known {
		return 0, false
	}
	return r.perSec, true
}

// History is the GPU's recent samples, oldest first.
func (s *State) History(node string, gpu int) []Sample {
	return s.history[gpuKey{node, gpu}]
}

// LastAlive is when node was last seen alive in this session.
func (s *State) LastAlive(node string) (time.Time, bool) {
	at, ok := s.lastAlive[node]
	return at, ok
}

// ApplyEvent adds or removes an in-flight request. Only request events with
// an id count; a terminal event whose start was never seen removes nothing.
func (s *State) ApplyEvent(e meshapi.MeshEvent) {
	if e.Type != meshapi.EventRequest || e.RequestID == 0 {
		return
	}
	id := e.NodeID
	if id == "" {
		id = e.Hostname
	}
	k := flightKey{id, e.RequestID}
	if meshapi.IsRequestTerminal(e.Message) {
		delete(s.flights, k)
		return
	}
	model, dest, ok := meshapi.SplitRequestMessage(e.Message)
	if !ok {
		return
	}
	origin := e.Hostname
	if origin == "" {
		origin = e.NodeID
	}
	f := Flight{Origin: origin, RID: e.RequestID, Model: model, Exec: origin, Start: time.Unix(e.Time, 0), proc: e.NodeID}
	if peer, fwd := strings.CutPrefix(dest, "peer "); fwd {
		f.Exec, f.Forward = peer, true
	}
	s.flights[k] = f
}

// Reset forgets every in-flight request. A reconnect calls it and lets the
// stream's replay rebuild the set, so the view can under-report but never
// invent work.
func (s *State) Reset() {
	clear(s.flights)
}

// Flights is every request in flight, oldest first.
func (s *State) Flights() []Flight {
	out := make([]Flight, 0, len(s.flights))
	for _, f := range s.flights {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !a.Start.Equal(b.Start) {
			return a.Start.Before(b.Start)
		}
		if a.Origin != b.Origin {
			return a.Origin < b.Origin
		}
		return a.RID < b.RID
	})
	return out
}
