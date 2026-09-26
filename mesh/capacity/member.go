package capacity

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/janit/viiwork/v2/mesh"
	"github.com/janit/viiwork/v2/mesh/meshclient"
	"github.com/janit/viiwork/v2/meshapi"
)

// Fetch describes one successful HTTP exchange with a member, handed to
// MemberPollConfig.Decode.
type Fetch struct {
	Node     string    // the member polled
	APIAddr  string    // where it was polled
	Start    time.Time // before the request was sent
	Received time.Time // after the body was read
}

// MemberPollConfig configures a MemberPoller: one endpoint, polled on every
// alive remote member of role node.
type MemberPollConfig[T any] struct {
	Self     string
	Members  Members
	Interval time.Duration
	// Timeout bounds one poll; 0 = Interval, so polls to one member never
	// stack (they never overlap either way).
	Timeout time.Duration
	Path    string // e.g. meshapi.PathCapacity
	// Noun names what is fetched in errors: "report" gives "report larger
	// than ...", "report names \"x\", expected y".
	Noun     string
	MaxBytes int64
	// Decode turns a 200 body into a value and the node name it claims. A
	// value naming another node than the one polled is refused by the poller,
	// so Decode need not check it.
	Decode func(body []byte, f Fetch) (node string, v T, err error)
	// Merge combines an accepted value with the member's previous one (for
	// smoothing); nil keeps the new value.
	Merge func(prev, next T) T
	// OnAccept runs after each accepted value; must not block; nil = none.
	OnAccept func(node string)
	// LogPrefix leads the one line logged per member until its next accepted
	// value: "<LogPrefix> of <node> (<addr>): <err>".
	LogPrefix   string
	Client      *http.Client                     // nil = a member-traffic client dialling within DialTimeout
	DialTimeout time.Duration                    // for the default client; 0 = meshclient.DefaultDialTimeout
	Logf        func(format string, args ...any) // nil = log.Printf
	Now         func() time.Time                 // nil = time.Now
}

// MemberPoller is the polling core shared by the capacity poller and the
// node's status poller. The rules it carries are load bearing:
//
//   - Polls to one member never overlap, so a slow member never stacks
//     requests; a poll is bounded by Timeout.
//   - A failure keeps the previous value, which simply ages, and is logged once
//     per member until its next accepted value.
//   - A value naming another node than the member it was fetched from is
//     refused: an address that changed hands (DHCP, a reused tailnet IP) must
//     not attach one machine's figures to another's name.
//   - A member that leaves the polling set is forgotten, and an answer landing
//     after it left is dropped.
type MemberPoller[T any] struct {
	c      MemberPollConfig[T]
	client *http.Client

	mu       sync.Mutex
	ctx      context.Context
	values   map[string]T
	wanted   map[string]bool // members currently polled; a late answer for anyone else is dropped
	inflight map[string]bool
	logged   map[string]bool // a rejection was logged since the last accepted value
}

// NewMemberPoller returns a poller; Run starts it.
func NewMemberPoller[T any](c MemberPollConfig[T]) *MemberPoller[T] {
	if c.Logf == nil {
		c.Logf = log.Printf
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Timeout <= 0 {
		c.Timeout = c.Interval
	}
	if c.Noun == "" {
		c.Noun = "response"
	}
	if c.LogPrefix == "" {
		c.LogPrefix = "poll"
	}
	client := c.Client
	if client == nil {
		client = meshclient.New(meshclient.Options{DialTimeout: c.DialTimeout})
	}
	return &MemberPoller[T]{
		c: c, client: client, ctx: context.Background(),
		values: map[string]T{}, wanted: map[string]bool{},
		inflight: map[string]bool{}, logged: map[string]bool{},
	}
}

// Run polls every Interval until ctx ends; in-flight polls use ctx.
func (p *MemberPoller[T]) Run(ctx context.Context) {
	p.mu.Lock()
	p.ctx = ctx
	p.mu.Unlock()
	p.tick()
	t := time.NewTicker(p.c.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.tick()
		}
	}
}

// Pollable reports whether m is polled: an alive, remote member of role node
// (a gateway serves no models and has no node status).
func (p *MemberPoller[T]) Pollable(m mesh.Member) bool {
	return m.State == meshapi.MemberAlive && !m.Local && m.Name != p.c.Self && m.Meta.Role == meshapi.RoleNode
}

func (p *MemberPoller[T]) tick() {
	wanted := map[string]bool{}
	var targets []mesh.Member
	for _, m := range p.c.Members.Members() {
		if p.Pollable(m) {
			wanted[m.Name] = true
			targets = append(targets, m)
		}
	}
	p.mu.Lock()
	p.wanted = wanted
	for name := range p.values {
		if !wanted[name] {
			delete(p.values, name)
		}
	}
	p.mu.Unlock()
	for _, m := range targets {
		p.poll(m)
	}
}

// PollNow polls one member at once, unless a poll of it is already in flight.
func (p *MemberPoller[T]) PollNow(node string) {
	for _, m := range p.c.Members.Members() {
		if m.Name == node && p.Pollable(m) {
			p.mu.Lock()
			p.wanted[node] = true
			p.mu.Unlock()
			p.poll(m)
			return
		}
	}
}

// HandleMemberEvent polls a member that joined or changed at once, and forgets
// one that left or failed.
func (p *MemberPoller[T]) HandleMemberEvent(ev mesh.MemberEvent) {
	switch ev.Kind {
	case mesh.EventJoin, mesh.EventUpdate:
		p.PollNow(ev.Member.Name)
	case mesh.EventLeave, mesh.EventFail:
		p.Forget(ev.Member.Name)
	}
}

// Forget drops a member's value.
func (p *MemberPoller[T]) Forget(node string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.values, node)
	delete(p.wanted, node)
}

// Get returns a member's last accepted value.
func (p *MemberPoller[T]) Get(node string) (T, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.values[node]
	return v, ok
}

// All returns every accepted value, sorted by node.
func (p *MemberPoller[T]) All() []T {
	p.mu.Lock()
	names := make([]string, 0, len(p.values))
	for n := range p.values {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]T, 0, len(names))
	for _, n := range names {
		out = append(out, p.values[n])
	}
	p.mu.Unlock()
	return out
}

// poll fetches one member's value in its own goroutine. Polls to one member
// never overlap.
func (p *MemberPoller[T]) poll(m mesh.Member) {
	p.mu.Lock()
	if p.inflight[m.Name] {
		p.mu.Unlock()
		return
	}
	p.inflight[m.Name] = true
	ctx := p.ctx
	p.mu.Unlock()

	go func() {
		defer func() {
			p.mu.Lock()
			delete(p.inflight, m.Name)
			p.mu.Unlock()
		}()
		addr := m.APIAddr()
		v, err := p.fetch(ctx, m.Name, addr)
		if err != nil {
			p.reject(m.Name, addr, err)
			return
		}
		p.mu.Lock()
		if !p.wanted[m.Name] {
			p.mu.Unlock()
			return // left the polling set while the poll was in flight
		}
		if prev, ok := p.values[m.Name]; ok && p.c.Merge != nil {
			v = p.c.Merge(prev, v)
		}
		p.values[m.Name] = v
		delete(p.logged, m.Name)
		p.mu.Unlock()
		if p.c.OnAccept != nil {
			p.c.OnAccept(m.Name)
		}
	}()
}

func (p *MemberPoller[T]) fetch(ctx context.Context, name, addr string) (T, error) {
	var zero T
	ctx, cancel := context.WithTimeout(ctx, p.c.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+p.c.Path, nil)
	if err != nil {
		return zero, err
	}
	start := p.c.Now()
	resp, err := p.client.Do(req)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, p.c.MaxBytes+1))
	received := p.c.Now()
	switch {
	case err != nil:
		return zero, fmt.Errorf("reading the %s: %w", p.c.Noun, err)
	case resp.StatusCode != http.StatusOK:
		return zero, fmt.Errorf("HTTP %d", resp.StatusCode)
	case int64(len(body)) > p.c.MaxBytes:
		return zero, fmt.Errorf("%s larger than %d bytes", p.c.Noun, p.c.MaxBytes)
	}
	node, v, err := p.c.Decode(body, Fetch{Node: name, APIAddr: addr, Start: start, Received: received})
	if err != nil {
		return zero, fmt.Errorf("decoding the %s: %w", p.c.Noun, err)
	}
	if node != name {
		// An address that changed hands must not attach one machine's figures
		// to another's name (Decision 3).
		return zero, fmt.Errorf("%s names %q, expected %s", p.c.Noun, node, name)
	}
	return v, nil
}

// reject keeps the previous value, which simply ages, and logs once per member
// until its next accepted value.
func (p *MemberPoller[T]) reject(name, addr string, err error) {
	p.mu.Lock()
	first := !p.logged[name]
	p.logged[name] = true
	p.mu.Unlock()
	if first {
		p.c.Logf("%s of %s (%s): %v", p.c.LogPrefix, name, addr, err)
	}
}
