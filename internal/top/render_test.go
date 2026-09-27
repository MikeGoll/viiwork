package top

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/janit/viiwork/v2/meshapi"
)

// fixture is a mesh of two alive nodes (one a Mac), one dead node and a
// gateway, with model m on both nodes and n on node-a.
func fixture() *State {
	a := meshapi.NodeStatus{
		Node: "node-a", Ver: "v2.6.0", UptimeS: 7200, HostMemTotalMB: 131072, HostMemUsedMB: 41984,
		GPUs: []meshapi.GPUInfo{
			{Index: 0, Vendor: "amd", Util: 90, VRAMUsedMB: 14000, VRAMTotalMB: 16000, PowerW: 160},
			{Index: 1, Vendor: "amd", Util: 10, VRAMUsedMB: 2000, VRAMTotalMB: 16000, PowerW: 30},
		},
		Models: []meshapi.ModelStatus{
			{Name: "m", Engine: "llamacpp", Slots: 4, Busy: 3, Queued: 2, Ctx: 49152, TokensTotal: 1000},
			{Name: "n", Engine: "llamacpp", Slots: 2, Busy: 0, Ctx: 16384},
		},
		Power: meshapi.PowerInfo{Watts: 412, Available: true, Source: "dcmi"},
	}
	b := meshapi.NodeStatus{
		Node: "node-b", Ver: "v2.6.0", UptimeS: 600, HostMemTotalMB: 36864, HostMemUsedMB: 30720,
		GPUs:   []meshapi.GPUInfo{{Index: 0, Vendor: "apple", Util: 61, VRAMUsedMB: 19000, VRAMTotalMB: 28000}},
		Models: []meshapi.ModelStatus{{Name: "m", Engine: "llamacpp", Slots: 2, Busy: 1, Ctx: 32768}},
		Power:  meshapi.PowerInfo{Available: false},
	}
	s := NewState()
	s.Apply(meshapi.ClusterResponse{
		View: "node-a", Mesh: meshapi.MeshSecured, ClusterCostEURPerHour: 0.31,
		Members: []meshapi.Member{
			{Node: "node-b", Role: meshapi.RoleNode, State: meshapi.MemberAlive, Status: &b},
			{Node: "gw", Role: meshapi.RoleGateway, State: meshapi.MemberAlive},
			{Node: "node-c", Role: meshapi.RoleNode, State: meshapi.MemberDead},
			{Node: "node-a", Role: meshapi.RoleNode, State: meshapi.MemberAlive, Status: &a},
		},
	}, t0)
	s.Connected = true
	s.ApplyEvent(ev("node-a", 7, meshapi.RequestStarted("m", "m/0"), t0.Unix()-12))
	return s
}

func texts(lines []Line) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.Text + "\n")
	}
	return b.String()
}

func find(t *testing.T, lines []Line, prefix string) Line {
	t.Helper()
	for _, l := range lines {
		if strings.HasPrefix(l.Text, prefix) {
			return l
		}
	}
	t.Fatalf("no line starts with %q in\n%s", prefix, texts(lines))
	return Line{}
}

func TestFleetScreen(t *testing.T) {
	s := fixture()
	lines := Render(s, View{}, 200, 50, t0)
	out := texts(lines)
	for _, want := range []string{
		"via node-a (v2.6.0)", "secured", "2/3 alive", // the gateway is not a host
		"power 0.41 kW", "0.31 €/h",
		"412W",
		"m 3/4 · n 0/2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "gw") {
		t.Errorf("gateway listed as a host:\n%s", out)
	}
	// Hosts sort by name whatever the member order.
	if ia, ib, ic := strings.Index(out, "node-a  "), strings.Index(out, "node-b  "), strings.Index(out, "node-c  "); !(ia < ib && ib < ic) {
		t.Errorf("hosts not sorted by name:\n%s", out)
	}
	// The Mac's VRAM is a share of unified memory; its power is absent.
	mac := find(t, lines, "node-b")
	if !strings.Contains(mac.Text, "68%*") || !strings.Contains(mac.Text, "—") {
		t.Errorf("mac row = %q", mac.Text)
	}
	if dead := find(t, lines, "node-c"); !strings.Contains(dead.Text, "dead") || dead.Style != Alert {
		t.Errorf("dead row = %+v", dead)
	}
	// Model totals across members: m has 6 slots, 4 busy, 2 queued, and the
	// context floor across members with slots.
	m := find(t, lines, "m  ")
	if f := strings.Fields(m.Text); len(f) < 5 || f[1] != "6" || f[2] != "4" || f[3] != "2" || f[4] != "32768" {
		t.Errorf("model row = %q", m.Text)
	}
	if m.Style != Alert {
		t.Errorf("queued model not alerted: %+v", m)
	}
	if !strings.Contains(out, "12s") || !strings.Contains(out, "m → node-a") {
		t.Errorf("in-flight row missing:\n%s", out)
	}
	if lines[1+1].Style != Selected { // header, column header, then the cursor's host
		t.Errorf("cursor row style = %v", lines[2].Style)
	}
}

func TestFleetColumnsDropInOrder(t *testing.T) {
	s := fixture()
	head := func(w int) string { return Render(s, View{}, w, 50, t0)[1].Text }
	if h := head(120); !strings.Contains(h, " RAM") || !strings.Contains(h, "PWR") || !strings.Contains(h, "GPUS") {
		t.Errorf("120 cols: %q", h)
	}
	if h := head(70); strings.Contains(h, " RAM") || !strings.Contains(h, "PWR") {
		t.Errorf("70 cols: %q", h)
	}
	if h := head(60); strings.Contains(h, "PWR") || strings.Contains(h, "GPUS") || !strings.Contains(h, "MODELS") {
		t.Errorf("60 cols: %q", h)
	}
	if l := Render(s, View{}, 59, 50, t0); len(l) != 1 || !strings.Contains(l[0].Text, "60") {
		t.Errorf("59 cols: %+v", l)
	}
}

func TestNoLineWiderOrTallerThanTheTerminal(t *testing.T) {
	s := fixture()
	long := strings.Repeat("x", 90)
	s.Cluster.Members = append(s.Cluster.Members, meshapi.Member{
		Node: long, Role: meshapi.RoleNode, State: meshapi.MemberAlive,
		Status: &meshapi.NodeStatus{Node: long, Models: []meshapi.ModelStatus{{Name: long, Slots: 1}}},
	})
	for _, v := range []View{{}, {Screen: Host, Host: "node-a"}} {
		for _, w := range []int{60, 80, 120, 200} {
			for _, h := range []int{3, 8, 50} {
				lines := Render(s, v, w, h, t0)
				if len(lines) > h {
					t.Errorf("screen %d %dx%d: %d lines", v.Screen, w, h, len(lines))
				}
				for _, l := range lines {
					if n := utf8.RuneCountInString(l.Text); n > w {
						t.Errorf("screen %d %dx%d: line of %d runes: %q", v.Screen, w, h, n, l.Text)
					}
				}
			}
		}
	}
}

func TestShortTerminalKeepsTheFooter(t *testing.T) {
	lines := Render(fixture(), View{}, 120, 5, t0)
	if len(lines) != 5 || !strings.Contains(lines[4].Text, "[q] quit") {
		t.Errorf("5-line frame:\n%s", texts(lines))
	}
}

func TestAbsentReadingsAreDashes(t *testing.T) {
	s := NewState()
	cpu := meshapi.NodeStatus{Node: "node-a", GPUs: []meshapi.GPUInfo{{Index: 0, Vendor: "none"}}}
	none := meshapi.NodeStatus{Node: "node-b"}
	s.Apply(meshapi.ClusterResponse{View: "node-a", Mesh: meshapi.MeshOpen, Members: []meshapi.Member{
		{Node: "node-a", Role: meshapi.RoleNode, State: meshapi.MemberAlive, Status: &cpu},
		{Node: "node-b", Role: meshapi.RoleNode, State: meshapi.MemberAlive, Status: &none},
	}}, t0)
	out := texts(Render(s, View{}, 120, 50, t0))
	if strings.Contains(out, "NaN") || strings.Contains(out, " 0W") {
		t.Errorf("absent readings rendered as numbers:\n%s", out)
	}
	if strings.Contains(out, "power") || strings.Contains(out, "€/h") {
		t.Errorf("header claims power or cost with none reported:\n%s", out)
	}
	if row := find(t, Render(s, View{}, 120, 50, t0), "node-b"); strings.Count(row.Text, "—") < 3 {
		t.Errorf("node-b row = %q; want dashes for util, vram, power, ram", row.Text)
	}
}

func TestWaitingAndReconnectingHeader(t *testing.T) {
	s := NewState()
	if l := Render(s, View{}, 80, 10, t0); !strings.Contains(l[0].Text, "waiting") {
		t.Errorf("before a snapshot: %q", l[0].Text)
	}
	s = fixture()
	s.Connected, s.Err = false, "connection refused"
	if l := Render(s, View{}, 200, 10, t0)[0]; !strings.Contains(l.Text, "reconnecting: connection refused") || l.Style != Alert {
		t.Errorf("reconnecting header = %+v", l)
	}
}

func TestModelSort(t *testing.T) {
	s := fixture()
	order := func(o ModelSort) string {
		var names []string
		lines := Render(s, View{Sort: o}, 120, 50, t0)
		for i, l := range lines {
			if strings.HasPrefix(l.Text, "MODEL") {
				for _, r := range lines[i+1:] {
					if strings.HasPrefix(r.Text, "─") {
						break
					}
					names = append(names, strings.Fields(r.Text)[0])
				}
			}
		}
		return strings.Join(names, ",")
	}
	if got := order(SortName); got != "m,n" {
		t.Errorf("by name: %s", got)
	}
	s.Cluster.Members[3].Status.Models[1].Queued = 5 // n on node-a
	if got := order(SortQueued); got != "n,m" {
		t.Errorf("by queued: %s", got)
	}
	if SortQueued.Next() != SortName || SortName.String() != "name" {
		t.Error("sort cycle")
	}
}
