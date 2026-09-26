// Package capacity polls mesh members' /v1/capacity reports. It is public so
// viiwork-gateway routes by the same reports and the same code as the nodes
// (P4 Decision 1): a private poller would force the gateway to write the fifth
// copy of a registry, the problem the viiwork 2 design starts from.
package capacity

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/janit/viiwork/v2/mesh"
	"github.com/janit/viiwork/v2/meshapi"
)

const (
	maxReportBytes = 1 << 20
	dialTimeout    = 500 * time.Millisecond
	rttAlpha       = 0.3
)

// Members is the member list the poller follows; *mesh.Mesh satisfies it.
type Members interface {
	Members() []mesh.Member
}

// Report is one member's last accepted capacity report.
type Report struct {
	Node     string
	APIAddr  string
	Received time.Time
	RTT      time.Duration // EWMA of successful polls
	Models   []meshapi.ModelCapacity
}

// Model returns the report's entry for name.
func (r Report) Model(name string) (meshapi.ModelCapacity, bool) {
	for _, m := range r.Models {
		if m.Name == name {
			return m, true
		}
	}
	return meshapi.ModelCapacity{}, false
}

// Fresh reports whether r is young enough to route by.
func Fresh(r Report, now time.Time, staleAfter time.Duration) bool {
	return now.Sub(r.Received) < staleAfter
}

type Config struct {
	Self     string
	Members  Members
	Interval time.Duration                    // mesh.capacity_poll
	Client   *http.Client                     // nil = the default below
	OnReport func(node string)                // after each accepted report; must not block; nil = none
	Logf     func(format string, args ...any) // nil = log.Printf
	Now      func() time.Time                 // nil = time.Now
}

// Poller holds members' capacity reports, refreshed every Interval. It is the
// MemberPoller core specialised to /v1/capacity, plus an RTT estimate; a poll
// times out after one Interval.
type Poller struct {
	core *MemberPoller[Report]
}

func NewPoller(c Config) *Poller {
	return &Poller{core: NewMemberPoller(MemberPollConfig[Report]{
		Self: c.Self, Members: c.Members, Interval: c.Interval,
		Path: meshapi.PathCapacity, Noun: "report", MaxBytes: maxReportBytes,
		LogPrefix: "capacity poll",
		// The default client's dial is short because a member that does not
		// answer within half a second is not one to route to.
		Client: c.Client, DialTimeout: dialTimeout,
		OnAccept: c.OnReport, Logf: c.Logf, Now: c.Now,
		Decode: decodeReport, Merge: smoothRTT,
	})}
}

func decodeReport(body []byte, f Fetch) (string, Report, error) {
	var cr meshapi.CapacityResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return "", Report{}, err
	}
	rtt := f.Received.Sub(f.Start)
	if rtt <= 0 {
		rtt = time.Nanosecond
	}
	return cr.Node, Report{Node: f.Node, APIAddr: f.APIAddr, Received: f.Received, RTT: rtt, Models: cr.Models}, nil
}

func smoothRTT(prev, next Report) Report {
	if prev.RTT > 0 {
		next.RTT = time.Duration(rttAlpha*float64(next.RTT) + (1-rttAlpha)*float64(prev.RTT))
	}
	return next
}

// Run polls every Interval until ctx ends; in-flight polls use ctx.
func (p *Poller) Run(ctx context.Context) { p.core.Run(ctx) }

// PollNow polls one member at once, unless a poll of it is already in flight.
func (p *Poller) PollNow(node string) { p.core.PollNow(node) }

// HandleMemberEvent polls a member that joined or changed at once, and forgets
// one that left or failed.
func (p *Poller) HandleMemberEvent(ev mesh.MemberEvent) { p.core.HandleMemberEvent(ev) }

// Forget drops a member's report.
func (p *Poller) Forget(node string) { p.core.Forget(node) }

func (p *Poller) Report(node string) (Report, bool) { return p.core.Get(node) }

// Reports returns every report, sorted by node.
func (p *Poller) Reports() []Report { return p.core.All() }
