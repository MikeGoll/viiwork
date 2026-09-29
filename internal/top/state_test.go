package top

import (
	"testing"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

var t0 = time.Unix(1790000000, 0)

func member(node string, uptime int64, tokens uint64, util float64) meshapi.Member {
	return meshapi.Member{
		Node: node, Addr: "192.0.2.1", Role: meshapi.RoleNode, State: meshapi.MemberAlive,
		Status: &meshapi.NodeStatus{
			Node: node, UptimeS: uptime,
			GPUs:   []meshapi.GPUInfo{{Index: 0, Vendor: "amd", Util: util, VRAMUsedMB: 8000, VRAMTotalMB: 16000}},
			Models: []meshapi.ModelStatus{{Name: "m", Slots: 2, TokensTotal: tokens}},
		},
	}
}

func snap(ms ...meshapi.Member) meshapi.ClusterResponse {
	return meshapi.ClusterResponse{View: "node-a", Mesh: meshapi.MeshSecured, Members: ms}
}

func TestRateFromTokenDeltas(t *testing.T) {
	s := NewState()
	s.Apply(snap(member("node-a", 100, 1000, 0)), t0)
	if _, ok := s.Rate("node-a", "m"); ok {
		t.Fatal("a single snapshot has no rate")
	}
	s.Apply(snap(member("node-a", 105, 1500, 0)), t0.Add(5*time.Second))
	if r, ok := s.Rate("node-a", "m"); !ok || r != 100 {
		t.Fatalf("rate = %v, %v; want 100, true", r, ok)
	}
	// Unchanged counters for less than two intervals keep the rate.
	s.Tick(t0.Add(14 * time.Second))
	if r, _ := s.Rate("node-a", "m"); r != 100 {
		t.Fatalf("rate decayed early: %v", r)
	}
	// More than two intervals (2 x 5 s) without a change decays it to 0,
	// even though no snapshot arrived: the stream pushes only on change.
	s.Tick(t0.Add(16 * time.Second))
	if r, ok := s.Rate("node-a", "m"); !ok || r != 0 {
		t.Fatalf("rate = %v, %v; want 0, true", r, ok)
	}
}

func TestRateResetsOnRestart(t *testing.T) {
	s := NewState()
	s.Apply(snap(member("node-a", 100, 1000, 0)), t0)
	s.Apply(snap(member("node-a", 105, 1500, 0)), t0.Add(5*time.Second))
	// uptime went backwards: the member restarted and its counters reset.
	s.Apply(snap(member("node-a", 3, 20, 0)), t0.Add(10*time.Second))
	if r, ok := s.Rate("node-a", "m"); ok {
		t.Fatalf("rate across a restart = %v; want unknown", r)
	}
	s.Apply(snap(member("node-a", 8, 520, 0)), t0.Add(15*time.Second))
	if r, ok := s.Rate("node-a", "m"); !ok || r != 100 {
		t.Fatalf("rate after restart = %v, %v; want 100, true", r, ok)
	}
}

func TestHistoryAppendsOnChangeCapsAndDrops(t *testing.T) {
	s := NewState()
	s.Apply(snap(member("node-a", 1, 0, 10)), t0)
	s.Apply(snap(member("node-a", 2, 0, 10)), t0.Add(time.Second)) // same reading
	if h := s.History("node-a", 0); len(h) != 1 || h[0].Util != 10 || h[0].VRAMPct != 50 {
		t.Fatalf("history = %+v; want one sample {10 50}", h)
	}
	for i := 0; i < historyLen+5; i++ {
		s.Apply(snap(member("node-a", int64(3+i), 0, float64(i%100))), t0.Add(time.Duration(2+i)*time.Second))
	}
	if n := len(s.History("node-a", 0)); n != historyLen {
		t.Fatalf("history length = %d; want %d", n, historyLen)
	}
	s.Apply(snap(member("node-b", 1, 0, 0)), t0.Add(time.Hour)) // node-a left the list
	if h := s.History("node-a", 0); h != nil {
		t.Fatalf("departed member kept history: %d samples", len(h))
	}
	if _, ok := s.Rate("node-a", "m"); ok {
		t.Fatal("departed member kept a rate")
	}
}

func TestLastAlive(t *testing.T) {
	s := NewState()
	s.Apply(snap(member("node-a", 1, 0, 0)), t0)
	dead := member("node-a", 1, 0, 0)
	dead.State, dead.Status = meshapi.MemberDead, nil
	s.Apply(snap(dead), t0.Add(time.Minute))
	if at, ok := s.LastAlive("node-a"); !ok || !at.Equal(t0) {
		t.Fatalf("last alive = %v, %v; want %v", at, ok, t0)
	}
}

func ev(host string, rid int64, msg string, at int64) meshapi.MeshEvent {
	return meshapi.MeshEvent{
		Event:  meshapi.Event{Time: at, Type: meshapi.EventRequest, Message: msg, RequestID: rid},
		NodeID: "id-" + host, Hostname: host,
	}
}

func TestFlights(t *testing.T) {
	s := NewState()
	s.ApplyEvent(ev("node-a", 7, meshapi.RequestStarted("m", "m/0"), 100))
	s.ApplyEvent(ev("node-a", 7, meshapi.RequestStarted("m", "m/0"), 100)) // replayed
	s.ApplyEvent(ev("node-b", 3, meshapi.RequestStarted("m", meshapi.PeerLabel("node-a")), 90))
	s.ApplyEvent(ev("node-a", 9, meshapi.RequestDone("n", "n/0", time.Second), 95)) // start never seen
	s.ApplyEvent(meshapi.MeshEvent{Event: meshapi.Event{Type: meshapi.EventBackend, Message: "m/0: healthy"}, Hostname: "node-a"})
	s.ApplyEvent(ev("node-a", 0, "no request id", 99))

	fl := s.Flights()
	if len(fl) != 2 {
		t.Fatalf("flights = %+v; want 2", fl)
	}
	fwd := fl[0] // node-b's started earlier
	if fwd.Origin != "node-b" || fwd.Exec != "node-a" || !fwd.Forward || fwd.Model != "m" || fwd.RID != 3 {
		t.Fatalf("forward = %+v", fwd)
	}
	if local := fl[1]; local.Origin != "node-a" || local.Exec != "node-a" || local.Forward || !local.Start.Equal(time.Unix(100, 0)) {
		t.Fatalf("local = %+v", local)
	}

	s.ApplyEvent(ev("node-a", 7, meshapi.RequestAborted("m", "m/0", time.Second), 101))
	if fl := s.Flights(); len(fl) != 1 || fl[0].Origin != "node-b" {
		t.Fatalf("after abort = %+v", fl)
	}
	s.Reset()
	if fl := s.Flights(); len(fl) != 0 {
		t.Fatalf("after reset = %+v", fl)
	}
}

// A request whose origin member died or left can never receive its terminal
// event, so the next snapshot drops it rather than letting it age forever.
func TestFlightsOfDeadOriginsAreDropped(t *testing.T) {
	s := NewState()
	s.Apply(snap(member("node-a", 1, 0, 0), member("node-b", 1, 0, 0)), t0)
	s.ApplyEvent(ev("node-a", 1, meshapi.RequestStarted("m", "m/0"), 100))
	s.ApplyEvent(ev("node-b", 2, meshapi.RequestStarted("m", "m/0"), 100))
	dead := member("node-b", 1, 0, 0)
	dead.State, dead.Status = meshapi.MemberDead, nil
	s.Apply(snap(member("node-a", 2, 0, 0), dead), t0.Add(time.Second))
	if fl := s.Flights(); len(fl) != 1 || fl[0].Origin != "node-a" {
		t.Fatalf("after node-b died: %+v", fl)
	}
	s.Apply(snap(member("node-b", 1, 0, 0)), t0.Add(2*time.Second)) // node-a left the list
	if fl := s.Flights(); len(fl) != 0 {
		t.Fatalf("after node-a left: %+v", fl)
	}
}

// A member that crashes and comes back under the same name may never show as
// anything but alive: the restart can fall between two snapshots. Its old
// requests will never see a terminal event, so a new process on the origin
// drops them — whether the snapshot shows it as a new node_id or, when the
// events carried none, as uptime going backwards.
func TestFlightsOfARestartedOriginAreDropped(t *testing.T) {
	withID := func(node, id string, uptime int64) meshapi.Member {
		m := member(node, uptime, 0, 0)
		m.Status.NodeID = id
		return m
	}
	s := NewState()
	s.Apply(snap(withID("node-a", "id-node-a", 100), member("node-b", 100, 0, 0)), t0)
	s.ApplyEvent(ev("node-a", 1, meshapi.RequestStarted("m", "m/0"), 100))
	noID := ev("node-b", 2, meshapi.RequestStarted("m", "m/0"), 100)
	noID.NodeID = ""
	s.ApplyEvent(noID)
	s.Apply(snap(withID("node-a", "id-node-a", 101), member("node-b", 101, 0, 0)), t0.Add(time.Second))
	if fl := s.Flights(); len(fl) != 2 {
		t.Fatalf("before the restarts: %+v", fl)
	}

	// node-a restarts: a new node_id, and its fresh counter reuses rid 1.
	s.Apply(snap(withID("node-a", "id-node-a-2", 3), member("node-b", 102, 0, 0)), t0.Add(2*time.Second))
	if fl := s.Flights(); len(fl) != 1 || fl[0].Origin != "node-b" {
		t.Fatalf("after node-a restarted: %+v", fl)
	}
	again := ev("node-a", 1, meshapi.RequestStarted("m", "m/1"), 103)
	again.NodeID = "id-node-a-2"
	s.ApplyEvent(again)
	s.Apply(snap(withID("node-a", "id-node-a-2", 4), member("node-b", 103, 0, 0)), t0.Add(3*time.Second))
	if fl := s.Flights(); len(fl) != 2 {
		t.Fatalf("the new process's request was dropped: %+v", fl)
	}

	// node-b restarts with no node_id to compare: uptime went backwards.
	s.Apply(snap(withID("node-a", "id-node-a-2", 5), member("node-b", 2, 0, 0)), t0.Add(4*time.Second))
	if fl := s.Flights(); len(fl) != 1 || fl[0].Origin != "node-a" || fl[0].RID != 1 {
		t.Fatalf("after node-b restarted: %+v", fl)
	}
}
