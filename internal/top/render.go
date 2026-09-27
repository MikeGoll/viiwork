package top

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

// minWidth is the narrowest terminal the screens are laid out for.
const minWidth = 60

// Style is how a line is emphasised. Colour is applied per line by Paint.
type Style int

const (
	Normal Style = iota
	Bold
	Dim
	Alert
	Selected
)

// Line is one row of the screen, never wider than the width it was drawn for.
type Line struct {
	Text  string
	Style Style
}

type Page int

const (
	Fleet Page = iota
	Host
)

type ModelSort int

const (
	SortName ModelSort = iota
	SortBusy
	SortQueued
)

func (m ModelSort) Next() ModelSort { return (m + 1) % 3 }

func (m ModelSort) String() string {
	return [...]string{"name", "busy", "queued"}[m]
}

// View is what the user has chosen to look at.
type View struct {
	Screen Page
	Cursor int    // index into HostNames on the fleet screen
	Host   string // the host screen's host
	Sort   ModelSort
	Paused bool
}

// Render draws v at w×h. It never returns a line wider than w or more than h
// lines; when the frame is too tall the bottom of the body is cut and the
// footer kept.
func Render(s *State, v View, w, h int, now time.Time) []Line {
	if w < minWidth {
		return []Line{{Text: sanitize(fit(fmt.Sprintf("viiwork top needs %d columns (has %d)", minWidth, w), w)), Style: Alert}}
	}
	var body []Line
	if v.Screen == Host {
		body = renderHost(s, v, w, now)
	} else {
		body = renderFleet(s, v, w, now)
	}
	lines := append([]Line{header(s, v, w)}, body...)
	if h < 2 {
		h = 2
	}
	if len(lines) > h-1 {
		lines = lines[:h-1]
	}
	lines = append(lines, footer(v, w))
	for i := range lines {
		lines[i].Text = sanitize(lines[i].Text)
	}
	return lines
}

// hosts is every member that is a machine, sorted by name: gateways carry no
// status and serve nothing, so they are not rows.
func hosts(s *State) []meshapi.Member {
	if s.Cluster == nil {
		return nil
	}
	var out []meshapi.Member
	for _, m := range s.Cluster.Members {
		if m.Role != meshapi.RoleGateway {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out
}

// HostNames is the fleet screen's host rows, in order.
func HostNames(s *State) []string {
	var out []string
	for _, m := range hosts(s) {
		out = append(out, m.Node)
	}
	return out
}

func header(s *State, v View, w int) Line {
	c := s.Cluster
	if c == nil {
		t := "viiwork top — waiting for the first snapshot"
		if s.Err != "" {
			t += " · " + s.Err
		}
		return Line{Text: fit(t, w), Style: Bold}
	}
	alive, total, ver := 0, 0, "?"
	var watts float64
	powered := false
	for _, m := range hosts(s) {
		total++
		if m.State == meshapi.MemberAlive {
			alive++
		}
		if m.Status == nil {
			continue
		}
		if m.Node == c.View {
			ver = m.Status.Ver
		}
		if m.State == meshapi.MemberAlive && m.Status.Power.Available {
			watts += m.Status.Power.Watts
			powered = true
		}
	}
	left := fmt.Sprintf("viiwork top — via %s (%s) · %s · %d/%d alive", c.View, ver, c.Mesh, alive, total)
	if v.Paused {
		left += " · PAUSED"
	}
	style := Bold
	if !s.Connected {
		left += " · reconnecting"
		if s.Err != "" {
			left += ": " + s.Err
		}
		style = Alert
	}
	var right []string
	if powered {
		right = append(right, fmt.Sprintf("power %.2f kW", watts/1000))
	}
	if c.ClusterCostEURPerHour > 0 {
		right = append(right, fmt.Sprintf("%.2f €/h", c.ClusterCostEURPerHour))
	}
	return Line{Text: spread(left, strings.Join(right, "  "), w), Style: style}
}

// spread puts left and right at the two ends of a w-cell line, dropping right
// when both do not fit.
func spread(left, right string, w int) string {
	if right == "" || width(left)+2+width(right) > w {
		return fit(left, w)
	}
	return left + strings.Repeat(" ", w-width(left)-width(right)) + right
}

func footer(v View, w int) Line {
	t := "[esc] back  [p] pause  [q] quit"
	if v.Screen == Fleet {
		t = fmt.Sprintf("[↑↓] host  [enter] detail  [m] sort: %s  [p] pause  [q] quit  · remote hosts refresh every 5 s", v.Sort)
	}
	return Line{Text: fit(t, w), Style: Dim}
}

func rule(w int) Line { return Line{Text: strings.Repeat("─", w), Style: Dim} }

// hostCols is which host-row columns fit the width.
type hostCols struct {
	cards, pwr, ram, models bool
	cardsW                  int
}

// layoutHosts drops columns in the spec's order until the row fits: RAM,
// power, the per-card mini-bars (the mean stays), then the model list.
func layoutHosts(w, maxCards int) hostCols {
	c := hostCols{cards: true, pwr: true, ram: true, models: true, cardsW: max(maxCards, 4)}
	need := func() int {
		n := 14 + 1 + 4 + 1 + 15 // host, util mean, vram
		if c.cards {
			n += c.cardsW + 1
		}
		if c.pwr {
			n += 7
		}
		if c.ram {
			n += 11
		}
		if c.models {
			n += 1 + 20
		}
		return n
	}
	for _, drop := range []*bool{&c.ram, &c.pwr, &c.cards, &c.models} {
		if need() <= w {
			break
		}
		*drop = false
	}
	return c
}

func hostHeader(c hostCols) string {
	var b strings.Builder
	b.WriteString(fit("HOST", 14))
	if c.cards {
		b.WriteString(" " + fit("GPUS", c.cardsW))
	}
	b.WriteString(" " + fit("UTIL", 4))
	b.WriteString(" " + fit("VRAM", 15))
	if c.pwr {
		b.WriteString(" " + fit("PWR", 6))
	}
	if c.ram {
		b.WriteString(" " + fit("RAM", 10))
	}
	if c.models {
		b.WriteString(" MODELS")
	}
	return b.String()
}

func renderFleet(s *State, v View, w int, now time.Time) []Line {
	if s.Cluster == nil {
		return nil
	}
	hs := hosts(s)
	maxCards := 0
	for _, m := range hs {
		if m.Status != nil {
			maxCards = max(maxCards, len(m.Status.GPUs))
		}
	}
	cols := layoutHosts(w, maxCards)
	out := []Line{{Text: fit(hostHeader(cols), w), Style: Bold}}
	for i, m := range hs {
		l := hostLine(s, m, cols, w, now)
		if i == v.Cursor {
			l.Style = Selected
		}
		out = append(out, l)
	}
	out = append(out, rule(w),
		Line{Text: fit(fmt.Sprintf("%-16s %5s %5s %7s %7s %6s", "MODEL", "SLOTS", "BUSY", "QUEUED", "CTX", "TOK/S"), w), Style: Bold})
	for _, r := range modelRows(s, v.Sort) {
		out = append(out, modelLine(r, w))
	}
	out = append(out, rule(w),
		Line{Text: fit(fmt.Sprintf("%-10s %6s %6s  %s", "IN FLIGHT", "RID", "AGE", "MODEL → HOST"), w), Style: Bold})
	fl := s.Flights()
	if len(fl) == 0 {
		out = append(out, Line{Text: fit("nothing in flight", w), Style: Dim})
	}
	for _, f := range fl {
		out = append(out, flightLine(f, w, now))
	}
	return out
}

func hostLine(s *State, m meshapi.Member, c hostCols, w int, now time.Time) Line {
	name := fit(m.Node, 14)
	if m.State != meshapi.MemberAlive {
		t := name + " " + m.State
		if at, ok := s.LastAlive(m.Node); ok {
			t += " " + dur(now.Sub(at))
		}
		style := Alert
		if m.State == meshapi.MemberLeft {
			style = Dim
		}
		return Line{Text: fit(t, w), Style: style}
	}
	if m.Status == nil {
		return Line{Text: fit(name+" alive, no status yet", w), Style: Dim}
	}
	st := m.Status
	var b strings.Builder
	b.WriteString(name)
	if c.cards {
		var cards strings.Builder
		for _, g := range st.GPUs {
			cards.WriteString(spark(g.Util))
		}
		b.WriteString(" " + fit(cards.String(), c.cardsW))
	}
	b.WriteString(" " + fit(utilText(st.GPUs), 4))
	b.WriteString(" " + fit(vramText(st.GPUs), 15))
	if c.pwr {
		b.WriteString(" " + fit(powerText(st.Power), 6))
	}
	if c.ram {
		b.WriteString(" " + fit(ramText(st), 10))
	}
	if c.models {
		b.WriteString(" " + modelsText(st.Models))
	}
	return Line{Text: fit(b.String(), w)}
}

// utilText is the mean utilisation, or a dash when no GPU reports one.
func utilText(gpus []meshapi.GPUInfo) string {
	if len(gpus) == 0 {
		return "—"
	}
	var sum float64
	for _, g := range gpus {
		sum += g.Util
	}
	return fmt.Sprintf("%3.0f%%", sum/float64(len(gpus)))
}

// vramText is used over total across the host's GPUs. An Apple GPU's memory
// is a share of unified RAM, marked with *.
func vramText(gpus []meshapi.GPUInfo) string {
	var used, total float64
	star := ""
	for _, g := range gpus {
		used += g.VRAMUsedMB
		total += g.VRAMTotalMB
		if g.Vendor == "apple" {
			star = "*"
		}
	}
	if total <= 0 {
		return "—"
	}
	pct := used / total * 100
	return fmt.Sprintf("%s %3.0f%%%s", bar(pct, 9), pct, star)
}

func powerText(p meshapi.PowerInfo) string {
	if !p.Available {
		return "—"
	}
	return fmt.Sprintf("%.0fW", p.Watts)
}

func ramText(st *meshapi.NodeStatus) string {
	if st.HostMemTotalMB <= 0 {
		return "—"
	}
	return fmt.Sprintf("%d/%dG", st.HostMemUsedMB/1024, st.HostMemTotalMB/1024)
}

func modelsText(ms []meshapi.ModelStatus) string {
	parts := make([]string, 0, len(ms))
	for _, m := range ms {
		parts = append(parts, fmt.Sprintf("%s %d/%d", m.Name, m.Busy, m.Slots))
	}
	return strings.Join(parts, " · ")
}

// modelRow is one model's totals across alive members. ctx is the smallest
// context among members with slots, the floor a client can rely on.
type modelRow struct {
	name                string
	slots, busy, queued int
	ctx                 int64
	tokS                float64
	tokKnown            bool
}

func modelRows(s *State, order ModelSort) []modelRow {
	by := map[string]*modelRow{}
	for _, m := range s.Cluster.Members {
		if m.State != meshapi.MemberAlive || m.Status == nil {
			continue
		}
		for _, ms := range m.Status.Models {
			r := by[ms.Name]
			if r == nil {
				r = &modelRow{name: ms.Name}
				by[ms.Name] = r
			}
			r.slots += ms.Slots
			r.busy += ms.Busy
			r.queued += ms.Queued
			if ms.Slots > 0 && ms.Ctx > 0 && (r.ctx == 0 || ms.Ctx < r.ctx) {
				r.ctx = ms.Ctx
			}
			if v, ok := s.Rate(m.Node, ms.Name); ok {
				r.tokS += v
				r.tokKnown = true
			}
		}
	}
	rows := make([]modelRow, 0, len(by))
	for _, r := range by {
		rows = append(rows, *r)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch {
		case order == SortBusy && a.busy != b.busy:
			return a.busy > b.busy
		case order == SortQueued && a.queued != b.queued:
			return a.queued > b.queued
		}
		return a.name < b.name
	})
	return rows
}

func modelLine(r modelRow, w int) Line {
	ctx, tok := "—", "—"
	if r.ctx > 0 {
		ctx = strconv.FormatInt(r.ctx, 10)
	}
	if r.tokKnown {
		tok = fmt.Sprintf("%.0f", r.tokS)
	}
	l := Line{Text: fit(fmt.Sprintf("%s %5d %5d %7d %7s %6s", fit(r.name, 16), r.slots, r.busy, r.queued, ctx, tok), w)}
	if r.queued > 0 {
		l.Style = Alert
	}
	return l
}

func flightLine(f Flight, w int, now time.Time) Line {
	fwd := ""
	if f.Forward {
		fwd = " (fwd)"
	}
	return Line{Text: fit(fmt.Sprintf("%s %6d %6s  %s → %s%s", fit(f.Origin, 10), f.RID, dur(now.Sub(f.Start)), f.Model, f.Exec, fwd), w)}
}
