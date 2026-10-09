package top

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

// renderHost is one host in detail: each GPU with a history graph, each
// model's backends, and the requests in flight on or from it.
func renderHost(s *State, v View, w int, now time.Time) []Line {
	if s.Cluster == nil {
		return nil
	}
	var m *meshapi.Member
	for i := range s.Cluster.Members {
		if s.Cluster.Members[i].Node == v.Host {
			m = &s.Cluster.Members[i]
		}
	}
	if m == nil {
		return []Line{{Text: fit("host "+v.Host+" is not in the mesh", w), Style: Alert}}
	}
	if m.State != meshapi.MemberAlive || m.Status == nil {
		return []Line{{Text: fit(fmt.Sprintf("host %s is %s", v.Host, m.State), w), Style: Alert}}
	}
	st := m.Status
	out := []Line{{Text: fit(fmt.Sprintf("HOST %s · %s · up %s · RAM %s · %s",
		m.Node, st.Ver, dur(time.Duration(st.UptimeS)*time.Second), ramText(st), powerText(st.Power)), w), Style: Bold}, rule(w)}

	if len(st.GPUs) == 0 {
		out = append(out, Line{Text: fit("no GPU readings from this host", w), Style: Dim})
	}
	for _, g := range st.GPUs {
		out = append(out, Line{Text: fit(gpuText(g), w)})
		h := s.History(m.Node, g.Index)
		out = append(out,
			Line{Text: fit("  util   "+graph(h, w-9, func(x Sample) float64 { return x.Util }), w), Style: Dim},
			Line{Text: fit("  vram   "+graph(h, w-9, func(x Sample) float64 { return x.VRAMPct }), w), Style: Dim})
	}

	out = append(out, rule(w), Line{Text: fit(fmt.Sprintf("%-16s %-6s %-24s %6s %4s %8s %6s",
		"BACKEND", "GPUS", "STATUS", "BUSY", "RSP", "UP", "RSS"), w), Style: Bold})
	for _, ms := range st.Models {
		out = append(out, Line{Text: fit(modelHeading(s, m.Node, ms), w)})
		for _, b := range ms.Backends {
			out = append(out, backendLine(b, w))
		}
	}

	out = append(out, rule(w), Line{Text: fit(fmt.Sprintf("%-10s %6s %6s  %s", "IN FLIGHT", "RID", "AGE", "MODEL → HOST"), w), Style: Bold})
	n := 0
	for _, f := range s.Flights() {
		if f.Exec == m.Node || f.Origin == m.Node {
			out = append(out, flightLine(f, w, now))
			n++
		}
	}
	if n == 0 {
		out = append(out, Line{Text: fit("nothing in flight", w), Style: Dim})
	}
	return out
}

func gpuText(g meshapi.GPUInfo) string {
	name := g.Name
	if name == "" {
		name = g.Vendor
	}
	vram := "—"
	if g.VRAMTotalMB > 0 {
		pct := g.VRAMUsedMB / g.VRAMTotalMB * 100
		vram = fmt.Sprintf("%s %3.0f%% %.1f/%.1fG", bar(pct, 10), pct, g.VRAMUsedMB/1024, g.VRAMTotalMB/1024)
	}
	pw := "—" // power_w is omitempty: absent, not zero
	if g.PowerW > 0 {
		pw = fmt.Sprintf("%.0fW", g.PowerW)
	}
	return fmt.Sprintf("GPU %-2d %s %s %3.0f%%  %s  %s", g.Index, fit(name, 14), bar(g.Util, 10), g.Util, vram, pw)
}

// modelHeading is a model's line on the host screen. tok/s is per model: the
// payload has no cumulative per-backend counter to derive a rate from.
func modelHeading(s *State, node string, ms meshapi.ModelStatus) string {
	if ms.Parked {
		return fmt.Sprintf("%s · %s · down (viiwork up on this host brings it back)", ms.Name, ms.Engine)
	}
	ctx, tok := "—", "—"
	if ms.Ctx > 0 {
		ctx = strconv.FormatInt(ms.Ctx, 10)
	}
	if r, ok := s.Rate(node, ms.Name); ok {
		tok = fmt.Sprintf("%.0f tok/s", r)
	}
	return fmt.Sprintf("%s · %s · ctx %s · %d/%d busy · %d queued · %s", ms.Name, ms.Engine, ctx, ms.Busy, ms.Slots, ms.Queued, tok)
}

func backendLine(b meshapi.BackendStatus, w int) Line {
	gpus := make([]string, len(b.GPUs))
	for i, g := range b.GPUs {
		gpus[i] = strconv.Itoa(g)
	}
	status := b.Status
	if b.Phase != "" {
		status += " " + b.Phase
	}
	rss := "—"
	if b.RSSMB > 0 {
		rss = fmt.Sprintf("%.1fG", float64(b.RSSMB)/1024)
	}
	l := Line{Text: fit(fmt.Sprintf("%s %s %s %6s %4d %8s %6s",
		fit(b.ID, 16), fit(strings.Join(gpus, ","), 6), fit(status, 24),
		fmt.Sprintf("%d/%d", b.Busy, b.Slots), b.Respawns, dur(time.Duration(b.UptimeS)*time.Second), rss), w)}
	switch b.Status {
	case meshapi.StatusUnhealthy, meshapi.StatusDead:
		l.Style = Alert
	case meshapi.StatusStarting:
		l.Style = Dim
	}
	return l
}
