package node

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/janit/viiwork/v2/mesh"
	"github.com/janit/viiwork/v2/mesh/capacity"
	"github.com/janit/viiwork/v2/meshapi"
)

const (
	statusTimeout  = 2 * time.Second
	maxStatusBytes = 4 << 20
)

type polledStatus struct {
	status   meshapi.NodeStatus
	received time.Time
}

// StatusPoller holds every alive node member's last /v1/status (P6
// Decision 1). It is the capacity poller's core (capacity.MemberPoller) aimed
// at /v1/status: polls to one member never overlap and time out after
// statusTimeout, a failure keeps the last status, a status naming another
// node is refused, and a member that leaves the list is forgotten.
type StatusPoller struct {
	core *capacity.MemberPoller[polledStatus]
}

func NewStatusPoller(self string, members capacity.Members, interval time.Duration, client *http.Client, logf func(string, ...any)) *StatusPoller {
	return &StatusPoller{core: capacity.NewMemberPoller(capacity.MemberPollConfig[polledStatus]{
		Self: self, Members: members, Interval: interval, Timeout: statusTimeout,
		Path: meshapi.PathStatus, Noun: "status", MaxBytes: maxStatusBytes,
		LogPrefix: "status poll", Client: client, DialTimeout: statusTimeout, Logf: logf,
		Decode: func(body []byte, f capacity.Fetch) (string, polledStatus, error) {
			var st meshapi.NodeStatus
			if err := json.Unmarshal(body, &st); err != nil {
				return "", polledStatus{}, err
			}
			return st.Node, polledStatus{status: st, received: f.Received}, nil
		},
	})}
}

// Run polls every interval until ctx ends.
func (p *StatusPoller) Run(ctx context.Context) { p.core.Run(ctx) }

// Status is a member's last accepted status and when it arrived.
func (p *StatusPoller) Status(node string) (meshapi.NodeStatus, time.Time, bool) {
	s, ok := p.core.Get(node)
	return s.status, s.received, ok
}

// HandleMemberEvent polls a member that joined or changed at once, and forgets
// one that left or failed.
func (p *StatusPoller) HandleMemberEvent(ev mesh.MemberEvent) { p.core.HandleMemberEvent(ev) }
