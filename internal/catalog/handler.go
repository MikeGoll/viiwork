package catalog

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/janit/viiwork/v2/internal/discovery"
)

// Path is where OpenCode looks: it fetches <OPENCODE_MODELS_URL>/api.json.
const Path = "/api.json"

// upstreamLimit caps a chained catalogue. models.dev's own document is several
// megabytes and grows with every provider on it; a node on a fleet is not the
// place to find out how large it has become.
const upstreamLimit = 32 << 20

// upstreamTimeout bounds the fetch. The endpoint answers with the fleet either
// way, so a slow upstream must not become a slow node.
const upstreamTimeout = 10 * time.Second

// Handler serves the OpenCode model catalogue on Path.
type Handler struct {
	cfg Config
	src Source

	client *http.Client

	mu      sync.Mutex
	cached  Catalog
	fetched time.Time
}

// NewHandler returns a handler serving src's models as cfg's provider.
func NewHandler(cfg Config, src Source) *Handler {
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: upstreamTimeout}
	}
	return &Handler{cfg: cfg, src: src, client: client}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}

	out := Catalog{}
	// Upstream first: the node's own entry is written over it, so that a
	// provider id the fleet uses is the fleet's whatever else publishes one.
	for id, p := range h.upstream() {
		out[id] = p
	}
	for id, p := range Build(h.cfg, discovery.Models(h.src), h.baseURL(r)) {
		out[id] = p
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// baseURL is the node's own OpenAI-compatible endpoint as this client reached
// it. Any node is an entry point and every node serves the whole mesh, so the
// address that worked for the catalogue is the address that will work for
// inference — and it keeps one machine's name out of every other machine's
// answer. BaseURL overrides it for a node published under a different name
// than the one it is reached on.
func (h *Handler) baseURL(r *http.Request) string {
	if h.cfg.BaseURL != "" {
		return h.cfg.BaseURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/v1"
}

// upstream is the chained catalogue, or nil. A failure is logged and answered
// with nil: an upstream enriches the fleet's catalogue and never gates it, so
// a node with no route off the tailnet still serves its own models.
func (h *Handler) upstream() Catalog {
	if h.cfg.Upstream == "" {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cached != nil && time.Since(h.fetched) < h.cfg.UpstreamTTL {
		return h.cached
	}

	c, err := h.fetchUpstream()
	if err != nil {
		// Keep serving a stale copy rather than dropping providers the client
		// saw a moment ago: the upstream is a catalogue, not a liveness check.
		log.Printf("catalog: upstream %s: %v", h.cfg.Upstream, err)
		return h.cached
	}
	h.cached, h.fetched = c, time.Now()
	return c
}

func (h *Handler) fetchUpstream() (Catalog, error) {
	resp, err := h.client.Get(h.cfg.Upstream + Path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errStatus(resp.StatusCode)
	}
	var c Catalog
	if err := json.NewDecoder(io.LimitReader(resp.Body, upstreamLimit)).Decode(&c); err != nil {
		return nil, err
	}
	return c, nil
}

type errStatus int

func (e errStatus) Error() string { return "status " + http.StatusText(int(e)) }
