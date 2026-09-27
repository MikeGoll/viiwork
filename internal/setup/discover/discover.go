// Package discover looks for viiwork nodes near this machine before the setup
// wizard asks about the mesh. Every source is best effort and finding nothing
// is a normal outcome. Nothing here reads or infers a secret: a secured mesh
// still needs its join code.
package discover

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

// Node is one node that answered.
type Node struct {
	Name, Version, API string
	Models             []string
}

// Sources are where candidate hosts come from.
type Sources struct {
	// Tailnet lists the online tailnet peers' addresses.
	Tailnet func(ctx context.Context) ([]netip.Addr, error)
	// LAN lists gossip addresses ("ip:port") from one mDNS browse.
	LAN     func(ctx context.Context) ([]string, error)
	APIPort int
	HTTP    *http.Client
}

const (
	maxHosts = 256 // a large tailnet is probed only this far
	parallel = 32
)

// Find asks every candidate host's /v1/status, all within timeout. The
// sources get half of it. Nodes are sorted by name, and a node reached by two
// addresses is listed once, by the first.
func Find(ctx context.Context, s Sources, timeout time.Duration) []Node {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	srcCtx, srcCancel := context.WithTimeout(ctx, timeout/2)
	defer srcCancel()

	var tail []netip.Addr
	var lan []string
	var wg sync.WaitGroup
	if s.Tailnet != nil {
		wg.Add(1)
		go func() { defer wg.Done(); tail, _ = s.Tailnet(srcCtx) }()
	}
	if s.LAN != nil {
		wg.Add(1)
		go func() { defer wg.Done(); lan, _ = s.LAN(srcCtx) }()
	}
	wg.Wait()

	var hosts []netip.Addr
	seen := map[netip.Addr]bool{}
	add := func(a netip.Addr) {
		a = a.Unmap()
		if a.IsValid() && !seen[a] && len(hosts) < maxHosts {
			seen[a] = true
			hosts = append(hosts, a)
		}
	}
	for _, a := range tail {
		add(a)
	}
	for _, c := range lan {
		if ap, err := netip.ParseAddrPort(c); err == nil {
			add(ap.Addr())
		}
	}

	found := make([]*Node, len(hosts))
	sem := make(chan struct{}, parallel)
	for i, h := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			found[i] = status(ctx, s.HTTP, netip.AddrPortFrom(h, uint16(s.APIPort)).String())
		}()
	}
	wg.Wait()

	var out []Node
	names := map[string]bool{}
	for _, n := range found {
		if n != nil && !names[n.Name] {
			names[n.Name] = true
			out = append(out, *n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func status(ctx context.Context, c *http.Client, api string) *Node {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+api+meshapi.PathStatus, nil)
	if err != nil {
		return nil
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var st meshapi.NodeStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st); err != nil || st.Node == "" {
		return nil
	}
	n := &Node{Name: st.Node, Version: st.Ver, API: api}
	for _, m := range st.Models {
		n.Models = append(n.Models, m.Name)
	}
	return n
}
