package top

import (
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

func TestHostScreen(t *testing.T) {
	s := fixture()
	a := s.Cluster.Members[3].Status // node-a
	a.Models[0].Backends = []meshapi.BackendStatus{
		{ID: "m/0", GPUs: []int{0, 1}, Status: meshapi.StatusHealthy, Slots: 4, Busy: 3, Respawns: 1, UptimeS: 3600, RSSMB: 2150},
	}
	a.Models[1].Backends = []meshapi.BackendStatus{
		{ID: "n/0", GPUs: []int{1}, Status: meshapi.StatusUnhealthy, Phase: "respawn grace", Slots: 2},
	}
	s.Apply(*s.Cluster, t0.Add(time.Second))
	s.ApplyEvent(ev("node-b", 4, meshapi.RequestStarted("m", meshapi.PeerLabel("node-a")), t0.Unix()-3))

	lines := Render(s, View{Screen: Host, Host: "node-a"}, 120, 60, t0)
	out := texts(lines)
	for _, want := range []string{
		"HOST node-a · v2.6.0 · up 2h00m · RAM 41/128G · 412W",
		"GPU 0", "GPU 1", "160W",
		"m · llamacpp · ctx 49152 · 3/4 busy · 2 queued",
		"n/0", "unhealthy respawn grace",
		"2.1G",
		"m → node-a (fwd)", // forwarded here from node-b
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if l := find(t, lines, "n/0"); l.Style != Alert {
		t.Errorf("unhealthy backend style = %v", l.Style)
	}
	if !strings.Contains(out, "[esc] back") {
		t.Errorf("host footer missing:\n%s", out)
	}
}

func TestHostScreenHistoryGraph(t *testing.T) {
	s := fixture()
	for i := 1; i <= 5; i++ {
		s.Cluster.Members[3].Status.GPUs[0].Util = float64(i * 20)
		s.Apply(*s.Cluster, t0.Add(time.Duration(i)*time.Second))
	}
	lines := Render(s, View{Screen: Host, Host: "node-a"}, 120, 60, t0)
	util := find(t, lines, "  util")
	if !strings.Contains(util.Text, "▇▂▄▅▇█") {
		t.Errorf("util graph = %q", util.Text)
	}
}

func TestHostScreenMissingOrDeadHost(t *testing.T) {
	s := fixture()
	if out := texts(Render(s, View{Screen: Host, Host: "node-c"}, 80, 20, t0)); !strings.Contains(out, "host node-c is dead") {
		t.Errorf("dead host:\n%s", out)
	}
	if out := texts(Render(s, View{Screen: Host, Host: "nope"}, 80, 20, t0)); !strings.Contains(out, "host nope is not in the mesh") {
		t.Errorf("unknown host:\n%s", out)
	}
}
