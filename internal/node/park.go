package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/httpjson"
	"github.com/janit/viiwork/v2/internal/route"
	"github.com/janit/viiwork/v2/internal/update"
	"github.com/janit/viiwork/v2/meshapi"
)

// Parking is `viiwork down` and `viiwork up`: a configured model taken off
// this node's GPUs while the node stays in the mesh. The node keeps a set of
// parked names, in memory only (a restart brings everything back), and the
// supervisor is given the running configuration minus that set. Apply's diff
// does the rest: a parked model is a removed one, so it stops admitting and
// drains with health.respawn_grace like any removed model, and unparking
// adds it back through the load gate.
//
// A parked model still appears where a configured model with no healthy
// backend would — /v1/status (marked parked), /v1/capacity and /v1/models
// with no slots, the router as local with nothing to admit into — so a
// member routes elsewhere and a client sees the same answers as for a model
// whose backends are down.

// maxParkBody bounds a down or up request's body.
const maxParkBody = 64 << 10

// unknownModelsError is a down or up naming a model this node does not run.
type unknownModelsError struct {
	names      []string
	configured []string
}

func (e *unknownModelsError) Error() string {
	conf := "none"
	if len(e.configured) > 0 {
		conf = strings.Join(e.configured, ", ")
	}
	return fmt.Sprintf("not configured on this node: %s (configured: %s); nothing changed", strings.Join(e.names, ", "), conf)
}

// isParked reports whether model is parked. It reads an immutable snapshot,
// so the request path pays one atomic load and a map lookup.
func (n *Node) isParked(model string) bool {
	p := n.parkedView.Load()
	return p != nil && (*p)[model]
}

// publishParkedLocked snapshots the parked set for isParked. applyMu held.
func (n *Node) publishParkedLocked() {
	view := make(map[string]bool, len(n.parked))
	for name := range n.parked {
		view[name] = true
	}
	n.parkedView.Store(&view)
}

// applyUnparkedLocked hands the supervisor models minus the parked ones.
// applyMu held.
func (n *Node) applyUnparkedLocked(models []config.Model) error {
	run := make([]config.Model, 0, len(models))
	for _, m := range models {
		if !n.parked[m.Name] {
			run = append(run, m)
		}
	}
	return n.applyModels(run)
}

// applyRunning is Run's first apply: the running configuration (a reload
// may have replaced it while the node was joining) minus anything parked
// meanwhile. From here on, parking and reloads apply at once.
func (n *Node) applyRunning() error {
	n.applyMu.Lock()
	defer n.applyMu.Unlock()
	n.modelsApplied = true
	return n.applyUnparkedLocked(n.runningConfig().Models)
}

// Park parks (down) or unparks the named models, or every configured model
// when names is empty, and returns each one's state afterwards. A name the
// running configuration does not have refuses the whole request. It returns
// once the change is applied: a parked model has stopped admitting and is
// draining, an unparked one is queued to load.
func (n *Node) Park(names []string, down bool) (meshapi.ParkResponse, error) {
	n.applyMu.Lock()
	defer n.applyMu.Unlock()
	cfg := n.runningConfig()
	configured := make([]string, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		configured = append(configured, m.Name)
	}
	if len(names) == 0 {
		names = configured
	}
	var want, unknown []string
	for _, name := range names {
		switch {
		case !slices.Contains(configured, name):
			if !slices.Contains(unknown, name) {
				unknown = append(unknown, name)
			}
		case !slices.Contains(want, name):
			want = append(want, name)
		}
	}
	if len(unknown) > 0 {
		return meshapi.ParkResponse{}, &unknownModelsError{names: unknown, configured: configured}
	}

	before := make(map[string]bool, len(n.parked))
	for name := range n.parked {
		before[name] = true
	}
	resp := meshapi.ParkResponse{Node: n.name, Models: make([]meshapi.ModelPark, 0, len(want))}
	var changed []string
	for _, name := range want {
		c := n.parked[name] != down
		if c {
			changed = append(changed, name)
		}
		if down {
			n.parked[name] = true
		} else {
			delete(n.parked, name)
		}
		resp.Models = append(resp.Models, meshapi.ModelPark{Name: name, Parked: down, Changed: c})
	}
	if len(changed) == 0 {
		return resp, nil
	}
	n.publishParkedLocked()
	if n.modelsApplied {
		if err := n.applyUnparkedLocked(cfg.Models); err != nil {
			n.parked = before
			n.publishParkedLocked()
			return meshapi.ParkResponse{}, err
		}
	}
	if down {
		n.logf("models down: %s parked; in-flight requests get %s, then their engines stop (viiwork up brings them back)",
			strings.Join(changed, ", "), cfg.Health.RespawnGrace.Duration)
	} else {
		n.logf("models up: %s loading again", strings.Join(changed, ", "))
	}
	return resp, nil
}

// pruneParkedLocked drops parked names the configuration no longer has, so
// a model removed and later added back starts. applyMu held.
func (n *Node) pruneParkedLocked(models []config.Model) {
	pruned := false
	for name := range n.parked {
		if !slices.ContainsFunc(models, func(m config.Model) bool { return m.Name == name }) {
			delete(n.parked, name)
			pruned = true
		}
	}
	if pruned {
		n.publishParkedLocked()
	}
}

// modelStatuses is /v1/status's models: the supervisor's, with each parked
// model in its place as parked with no backends, in configuration order.
func (n *Node) modelStatuses() []meshapi.ModelStatus {
	running := n.sup.Status()
	parked := n.parkedView.Load()
	if parked == nil || len(*parked) == 0 {
		return running
	}
	out := make([]meshapi.ModelStatus, 0, len(running)+len(*parked))
	byName := make(map[string]int, len(running))
	for i, m := range running {
		byName[m.Name] = i
	}
	used := make([]bool, len(running))
	for _, m := range n.runningConfig().Models {
		if i, ok := byName[m.Name]; ok {
			out = append(out, running[i])
			used[i] = true
		} else if (*parked)[m.Name] {
			out = append(out, meshapi.ModelStatus{Name: m.Name, Engine: m.Engine, Ctx: int64(m.Context),
				Backends: []meshapi.BackendStatus{}, Parked: true})
		}
	}
	for i, m := range running {
		if !used[i] {
			out = append(out, m)
		}
	}
	return out
}

// localCapacity is this node's capacity as the proxy and the alias resolver
// read it: the supervisor's, plus each parked model with no backends and no
// slots — what a configured model whose backends are all down reports.
type localCapacity struct{ n *Node }

func (l localCapacity) Capacity() []meshapi.ModelCapacity {
	out := l.n.sup.Capacity()
	parked := l.n.parkedView.Load()
	if parked == nil || len(*parked) == 0 {
		return out
	}
	for _, m := range l.n.runningConfig().Models {
		if (*parked)[m.Name] && !slices.ContainsFunc(out, func(c meshapi.ModelCapacity) bool { return c.Name == m.Name }) {
			out = append(out, meshapi.ModelCapacity{Name: m.Name, Engine: m.Engine, Ctx: int64(m.Context)})
		}
	}
	return out
}

// parkedLocal is the router's view of local models: a parked model is still
// this node's, with no backend to admit into, so a request for it queues
// and a forward is refused with 429 rather than 404 — as for a model whose
// backends are down. The parked check runs only on a supervisor miss.
type parkedLocal struct {
	inner route.LocalModels
	n     *Node
}

func (p parkedLocal) Backends(model string) ([]route.LocalBackend, bool, bool) {
	bs, admitting, ok := p.inner.Backends(model)
	if !ok && p.n.isParked(model) {
		return nil, false, true
	}
	return bs, admitting, ok
}

// parkHandler is POST PathModelsDown and PathModelsUp, authorised like alias
// writes: a meshauth signature in a secured mesh, a program on this machine
// in an open one. The CSRF guard has already required JSON.
type parkHandler struct {
	auth update.Authorizer
	park func(names []string, down bool) (meshapi.ParkResponse, error)
}

func (h *parkHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	down := r.URL.Path == meshapi.PathModelsDown
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		httpjson.Error(w, http.StatusMethodNotAllowed, "invalid_request", "method not allowed")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxParkBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httpjson.Error(w, http.StatusRequestEntityTooLarge, "invalid_request", "request body too large")
			return
		}
		httpjson.Error(w, http.StatusBadRequest, "invalid_request", "failed to read request")
		return
	}
	// Authorisation first, so an unauthorised caller learns nothing about
	// the node's models.
	if status, msg, ok := h.auth.Authorize(r, body); !ok {
		httpjson.Error(w, status, "invalid_request", msg)
		return
	}
	var req meshapi.ParkRequest
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			httpjson.Error(w, http.StatusBadRequest, "invalid_request", "the body must be a JSON object: "+err.Error())
			return
		}
	}
	resp, err := h.park(req.Models, down)
	var unknown *unknownModelsError
	switch {
	case errors.As(err, &unknown):
		httpjson.Error(w, http.StatusBadRequest, "invalid_request", unknown.Error())
		return
	case err != nil:
		httpjson.Error(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	httpjson.Write(w, http.StatusOK, resp)
}
