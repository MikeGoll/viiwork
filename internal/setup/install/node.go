package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

// Node is the freshly started node's API, as both platforms wait on it.
type Node struct {
	HTTP *http.Client
	API  string // host:port on loopback
}

// Status is the node's /v1/status.
func (n Node) Status(ctx context.Context) (meshapi.NodeStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var st meshapi.NodeStatus
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+n.API+meshapi.PathStatus, nil)
	if err != nil {
		return st, err
	}
	resp, err := n.HTTP.Do(req)
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return st, errors.New(resp.Status)
	}
	return st, json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&st)
}

// WaitUp waits until the node's API answers at all. /health is 503 until a
// model has loaded, which for large weights takes many minutes, so any HTTP
// answer means the process is up.
func (n Node) WaitUp(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+n.API+"/health", nil)
		if err != nil {
			return err
		}
		if resp, err := n.HTTP.Do(req); err == nil {
			resp.Body.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the node's API did not answer on %s within %s", n.API, timeout)
		case <-time.After(time.Second):
		}
	}
}

// WaitModels waits until every named model has a healthy backend. It stops
// early when every backend of one of them is dead: the supervisor has given
// up on it, and waiting longer changes nothing.
func (n Node) WaitModels(ctx context.Context, names []string, timeout, poll time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastUptime int64 = -1
	for {
		pending := names
		if st, err := n.Status(ctx); err == nil {
			// Uptime going backwards is a restart: a node in a restart loop
			// (launchd KeepAlive, restart: always) answers after every start,
			// and would otherwise hold the wait for its whole budget.
			if lastUptime >= 0 && st.UptimeS < lastUptime {
				return fmt.Errorf("the node restarted (uptime %d s, then %d s): it is exiting; its log says why", lastUptime, st.UptimeS)
			}
			lastUptime = st.UptimeS
			pending = nil
			for _, name := range names {
				ready, dead := modelState(st, name)
				if dead {
					return fmt.Errorf("model %s: every backend is dead", name)
				}
				if !ready {
					pending = append(pending, name)
				}
			}
			if len(pending) == 0 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("not loaded within %s: %s", timeout, strings.Join(pending, ", "))
		case <-time.After(poll):
		}
	}
}

func modelState(st meshapi.NodeStatus, name string) (ready, dead bool) {
	for _, m := range st.Models {
		if m.Name != name || len(m.Backends) == 0 {
			continue
		}
		dead = true
		for _, b := range m.Backends {
			ready = ready || b.Status == meshapi.StatusHealthy
			dead = dead && b.Status == meshapi.StatusDead
		}
	}
	return ready, dead
}

// unhealthy prints every backend that is not healthy.
func (n Node) unhealthy(ctx context.Context, out io.Writer) {
	st, err := n.Status(ctx)
	if err != nil {
		fmt.Fprintf(out, "  %s: %v\n", meshapi.PathStatus, err)
		return
	}
	for _, m := range st.Models {
		for _, b := range m.Backends {
			if b.Status != meshapi.StatusHealthy {
				fmt.Fprintf(out, "  %s backend %s: %s %s\n", m.Name, b.ID, b.Status, b.Phase)
			}
		}
	}
}
