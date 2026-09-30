package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/janit/viiwork/v2/internal/activity"
	"github.com/janit/viiwork/v2/internal/api"
	"github.com/janit/viiwork/v2/internal/discovery"
	"github.com/janit/viiwork/v2/internal/engine"
	"github.com/janit/viiwork/v2/internal/httpjson"
	"github.com/janit/viiwork/v2/internal/logging"
	"github.com/janit/viiwork/v2/internal/perf"
	"github.com/janit/viiwork/v2/internal/pipeline"
	"github.com/janit/viiwork/v2/internal/route"
	"github.com/janit/viiwork/v2/meshapi"
)

// Error types the handler writes besides meshapi's.
const (
	errTypeInvalidRequest = "invalid_request"
	errTypeNotFound       = "not_found"
	errTypeServer         = "server_error"
)

// queueRetryAfter is the Retry-After, in seconds, on a 429 from the queue.
const queueRetryAfter = "2"

// ResolveError is written as-is by the handler; see api.ResolveError.
type ResolveError = api.ResolveError

// Resolver maps the requested model name to the real model and the alias it
// was reached through ("" for a real name).
type Resolver func(requested string) (model, alias string, err error)

// ModelLister adds entries to /v1/models.
type ModelLister func() []meshapi.ModelEntry

// LocalCapacity is this node's per-model occupancy; *supervisor.Supervisor
// satisfies it.
type LocalCapacity interface {
	Capacity() []meshapi.ModelCapacity
}

type Deps struct {
	Self         string
	Version      string
	Router       *route.Router
	Reports      route.Reports // *capacity.Poller
	Local        LocalCapacity // *supervisor.Supervisor
	Auth         *ForwardAuth
	Counters     *Counters
	ForwardRetry int
	// StaleAfter is routing.stale_after. The fleet view uses it so its totals
	// count exactly the reports the router would trust.
	StaleAfter   time.Duration
	Activity     *activity.Log // nil = no events, no prompt history
	Pipelines    *PipelineResolver
	PipelineExec *pipeline.Executor
	Resolve      Resolver    // nil = identity
	ExtraModels  ModelLister // nil = none
	// Perf receives service-TTFT samples of requests this node executed and
	// supplies this node's scores (performance routing). nil = no measurement.
	Perf PerfRecorder
	// PublishPerf adds the scores to /v1/capacity (routing.performance).
	PublishPerf bool
	// Usage says how a model's engine reports usage. nil = the zero value:
	// ask for usage, and treat an absent cached_tokens as unknown.
	Usage func(model string) engine.UsageReporting
}

// PerfRecorder is *perf.Tracker as the proxy uses it.
type PerfRecorder interface {
	Record(model string, at time.Time, uncached int64, ttft time.Duration)
	Score(model string) (perf.Score, bool)
}

// Handler serves inference, /v1/models and /v1/capacity. The node server
// wraps it with panic recovery, CORS, /health and the dashboards.
type Handler struct {
	d Deps
}

func NewHandler(d Deps) *Handler { return &Handler{d: d} }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case meshapi.PathModels:
		// Not part of a dialect (C8): the model list answers the fleet's
		// question, not a client library's. A second dialect wanting a
		// differently shaped catalogue gets its own path.
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		h.handleModels(w)
		return
	case meshapi.PathCapacity:
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		h.handleCapacity(w)
		return
	case meshapi.PathFleetCapacity:
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		h.handleFleetCapacity(w, r)
		return
	}
	// Inference paths come from the dialect registry rather than from three
	// string literals here, so adding a client API shape later is a package
	// and a blank import rather than an excavation. One map lookup, no
	// allocation; see internal/api.
	if d, ok := api.Lookup(r.URL.Path); ok {
		if r.Method != http.MethodPost {
			httpjson.Error(w, http.StatusMethodNotAllowed, errTypeInvalidRequest, "method not allowed")
			return
		}
		h.handleInference(w, r, d)
		return
	}
	http.NotFound(w, r)
}

// handleInference runs the inference flow of P4 Task 8: parse, verify a
// forward, resolve, then dispatch with retries over the router.
func (h *Handler) handleInference(w http.ResponseWriter, r *http.Request, dialect api.Dialect) {
	start := time.Now()

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	body, err := readBodyPresized(r.Body, r.ContentLength)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httpjson.Error(w, http.StatusRequestEntityTooLarge, errTypeInvalidRequest, "request body too large")
			return
		}
		httpjson.Error(w, http.StatusBadRequest, errTypeInvalidRequest, "failed to read request")
		return
	}

	req, _, err := dialect.Decode(r, body)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, errTypeInvalidRequest, "failed to read request")
		return
	}
	body = req.Body
	if req.Model == "" {
		httpjson.Error(w, http.StatusBadRequest, errTypeInvalidRequest, "model is required")
		return
	}
	fields := struct {
		Model string
		Think *bool
	}{Model: req.Model, Think: req.Think}
	thinkDisabled := fields.Think == nil || !*fields.Think
	taskID := req.Task
	if taskID != "" {
		r.Header.Set(HeaderTask, taskID)
	}

	_, forwarded := h.d.Auth.Verify(r, body)
	stripMeshHeaders(r.Header)

	if !forwarded && h.d.Pipelines != nil {
		if p, locale, localeKey, ok := h.d.Pipelines.Resolve(fields.Model); ok {
			sourceText := pipelineSourceText(body)
			if sourceText == "" {
				httpjson.Error(w, http.StatusBadRequest, errTypeInvalidRequest, "no user message found")
				return
			}
			h.handlePipeline(w, r, p, locale, localeKey, sourceText, fields.Model, taskID)
			return
		}
		if name, matched := h.d.Pipelines.MatchesPipelinePrefix(fields.Model); matched {
			msg := fmt.Sprintf("unknown locale in model '%s', available: %v", fields.Model, h.d.Pipelines.AvailableLocales(name))
			httpjson.Error(w, http.StatusBadRequest, errTypeInvalidRequest, msg)
			return
		}
	}

	// A forward carries the real name, resolved once on the origin.
	model, alias := fields.Model, ""
	if !forwarded && h.d.Resolve != nil {
		m, a, err := h.d.Resolve(fields.Model)
		if err != nil {
			var re *ResolveError
			if errors.As(err, &re) {
				if re.RetryAfter > 0 {
					w.Header().Set("Retry-After", strconv.Itoa(re.RetryAfter))
				}
				httpjson.Error(w, re.Status, re.Type, re.Message)
				return
			}
			log.Printf("proxy: resolving model %q: %v", fields.Model, err)
			httpjson.Error(w, http.StatusInternalServerError, errTypeServer, "model resolution failed")
			return
		}
		model, alias = m, a
		if alias != "" {
			// Engines and peers see the name they serve; a receiver never
			// resolves. Only aliased requests pay for the rewrite.
			body = rewriteModel(body, model)
		}
	}

	// The pin is compared against member names by the router and never
	// dialled. A forward ignores it: it already reached the pinned node.
	// Guarded on RawQuery so the common request parses nothing.
	var host string
	if !forwarded && r.URL.RawQuery != "" {
		pin, ok := sanitizeHost(r.URL.Query().Get(meshapi.QueryHost))
		if !ok {
			httpjson.Error(w, http.StatusBadRequest, errTypeInvalidRequest, "invalid host parameter")
			return
		}
		host = pin
	}

	w, capture := newCaptureWriter(w)
	// Forwards leave no prompt history: the origin already has it (Decision 12).
	d := &dispatch{
		model: model, alias: alias, host: host, taskID: taskID, body: body,
		forwarded: forwarded, thinkDisabled: thinkDisabled,
		record: !forwarded && h.d.Activity != nil, start: start, capture: capture,
	}
	if d.record {
		// The prompt history names both the model and the alias it was asked
		// for (P5 Decision 17); events and counters keep the real model.
		historyModel := model
		if alias != "" {
			historyModel = model + " (alias " + alias + ")"
		}
		d.rid = activity.NewRequestID()
		h.d.Activity.StorePrompt(d.rid, historyModel, extractPromptText(body))
		// Deferred, so a response aborted by panic is recorded too.
		defer func() {
			h.d.Activity.StoreOutput(d.rid, historyModel, capture.Output(), time.Since(start).Milliseconds())
		}()
	}
	h.dispatch(w, r, d)
}

// dispatch is one inference request past parsing and resolution: what the
// dispatch loop needs to run it and to report on it.
type dispatch struct {
	model, alias, host, taskID string
	body                       []byte
	forwarded, thinkDisabled   bool
	record                     bool // emit activity events (the origin only)
	rid                        int64
	start                      time.Time
	capture                    *captureWriter
	localBody                  []byte // body with include_usage injected, built on the first local attempt
	localStrip                 bool
	localBuilt                 bool
}

// dispatch runs the request over the router. The rules that make it safe all
// live here:
//
//   - One queue budget per request. The first acquisition may queue for
//     routing.queue_timeout; the final one, after the retries, only for what
//     is left of it (Decision 17).
//   - Retries happen only before the first byte. An attempt that wrote
//     nothing is retryable; once headers went out the attempt is final, and a
//     response cut short after that is aborted, never retried.
//   - A forward is dispatched once: the origin picks again (Decision 16).
//   - Every attempt releases its lease, whatever happens (see attempt).
//   - An exhausted dispatch whose every attempt was a capacity refusal is a
//     503 with Retry-After — the mesh is busy, not broken; any other failure
//     among them makes it a 502.
func (h *Handler) dispatch(w http.ResponseWriter, r *http.Request, d *dispatch) {
	estK := float64(len(d.body)) / 4000 // ~4 bytes per token; the entry cannot see caches
	lease, queued, err := h.d.Router.Acquire(r.Context(), route.Request{Model: d.model, Host: d.host, Forwarded: d.forwarded, EstK: estK})
	if err != nil {
		if d.forwarded && errors.Is(err, route.ErrModelNotFound) {
			// The origin chose this node from a report that still listed
			// the model (dropped on reload since, or a restart with another
			// config): a refusal it retries elsewhere, not a 404 for the
			// client while another member serves the model.
			w.Header().Set("Retry-After", "1")
			httpjson.Error(w, http.StatusServiceUnavailable, meshapi.ErrTypeUnavailable, fmt.Sprintf("model %q is not served here", d.model))
			return
		}
		h.writeAcquireError(w, err, d.model, d.host)
		return
	}
	var (
		tried      map[string]bool
		retries    int
		final      bool // the last dispatch: the one after the final acquisition
		allRefusal = true
	)
	for {
		t := lease.Target()
		if queued > 0 {
			w.Header().Set(meshapi.HeaderQueuedMs, strconv.FormatInt(queued.Milliseconds(), 10))
		}
		if d.alias != "" {
			w.Header().Set(meshapi.HeaderAlias, d.alias)
		}
		label := t.BackendID
		if !t.Local {
			label = meshapi.PeerLabel(t.Node)
		}
		if d.record {
			h.d.Activity.EmitRequestTask(d.rid, -1, d.taskID, "%s", meshapi.RequestStarted(d.model, label))
		}

		res := h.attempt(w, r, lease, d, label)

		if res.Outcome == outcomeServed {
			if d.record {
				elapsed := time.Since(d.start).Round(time.Millisecond)
				msg := meshapi.RequestDone(d.model, label, elapsed)
				if res.Aborted || res.Truncated {
					msg = meshapi.RequestAborted(d.model, label, elapsed)
				}
				h.d.Activity.EmitRequestTask(d.rid, -1, d.taskID, "%s", msg)
			}
			// Counted where the request ran, so a forward is counted by its
			// receiver and not twice (spec).
			if t.Local {
				// The response is complete here, so the capture's usage
				// decode (cached on first use) sees the whole body. The
				// capture never sees a stripped usage chunk, so without the
				// stripped event tokens_total would lose every such request.
				tokens, _ := d.capture.CompletionTokens()
				if n, ok := completionTokensFromEvent(res.StrippedUsage); ok {
					tokens = n
				}
				h.d.Counters.Add(d.model, tokens)
				h.recordPerf(d.model, res, d.capture)
			}
			if res.Truncated {
				// The headers are out, so the status cannot say it failed; a
				// clean end would make the partial body look complete. Abort
				// the connection, as httputil.ReverseProxy does. The lease is
				// already released and the deferred history still runs.
				log.Printf("proxy: %s: %s", d.model, res.Reason)
				panic(http.ErrAbortHandler)
			}
			return
		}

		if logging.DebugEnabled() {
			log.Printf("[debug] %s: dispatch not served: %s", d.model, res.Reason)
		}
		allRefusal = allRefusal && res.Refusal
		if d.forwarded {
			// One dispatch only: the origin picks again (Decision 16).
			httpjson.Error(w, http.StatusServiceUnavailable, meshapi.ErrTypeUnavailable, "backend failed before responding")
			return
		}
		if final {
			h.endUnserved(d, label)
			if allRefusal {
				log.Printf("proxy: %s: every route refused the request; last: %s", d.model, res.Reason)
				w.Header().Set("Retry-After", queueRetryAfter)
				httpjson.Error(w, http.StatusServiceUnavailable, meshapi.ErrTypeUnavailable, fmt.Sprintf("no route has capacity for %q", d.model))
				return
			}
			log.Printf("proxy: %s: no route could serve the request; last: %s", d.model, res.Reason)
			httpjson.Error(w, http.StatusBadGateway, errTypeServer, "no route could serve the request")
			return
		}

		if tried == nil {
			tried = map[string]bool{}
		}
		tried[t.Key()] = true
		if retries < h.d.ForwardRetry {
			retries++
			if next, err := h.d.Router.Pick(route.Request{Model: d.model, Host: d.host, Exclude: tried, EstK: estK}); err == nil {
				lease = next
				continue
			}
		}

		// The final acquisition may queue, but only for what is left of the
		// request's queue budget (Decision 17).
		budget := h.d.Router.QueueTimeout() - queued
		if budget <= 0 {
			budget = -1
		}
		var waited time.Duration
		lease, waited, err = h.d.Router.Acquire(r.Context(), route.Request{Model: d.model, Host: d.host, QueueBudget: budget, EstK: estK})
		queued += waited
		if err != nil {
			h.endUnserved(d, label)
			h.writeAcquireError(w, err, d.model, d.host)
			return
		}
		final = true
	}
}

func (h *Handler) usage(model string) engine.UsageReporting {
	if h.d.Usage == nil {
		return engine.UsageReporting{}
	}
	return h.d.Usage(model)
}

// localBodyFor is the body a local backend gets, built once per request:
// with include_usage injected only when measuring and the engine both needs
// asking for usage and reports cached tokens. Any other engine could never
// yield a sample (recordPerf needs the cached count), so its request goes out
// exactly as the client sent it. strip reports that the usage chunk must be
// removed again, because the client never asked for it.
func (h *Handler) localBodyFor(d *dispatch) ([]byte, bool) {
	if !d.localBuilt {
		d.localBuilt, d.localBody = true, d.body
		if h.d.Perf != nil && h.usage(d.model) == (engine.UsageReporting{CachedTokens: true}) {
			d.localBody, d.localStrip = injectIncludeUsage(d.body)
		}
	}
	return d.localBody, d.localStrip
}

// recordPerf records one local serve's service TTFT (spec §1): only a 200
// with a first token and prompt usage that says how many tokens were
// prefilled. Everything else records nothing. Called only after the response
// completed: the capture caches its usage decode on first use.
func (h *Handler) recordPerf(model string, res execResult, c *captureWriter) {
	if h.d.Perf == nil || res.Status != http.StatusOK || res.FirstToken.IsZero() || res.Granted.IsZero() {
		return
	}
	u, ok := promptUsageFromEvent(res.StrippedUsage)
	if !ok {
		u, ok = c.PromptUsage()
	}
	if !ok || (!u.CachedPresent && !h.usage(model).CachedTokens) {
		return
	}
	uncached := u.Prompt - u.Cached
	if uncached < 0 {
		return
	}
	h.d.Perf.Record(model, res.FirstToken, uncached, res.FirstToken.Sub(res.Granted))
}

// attempt runs one dispatch on the lease's target and releases the lease on
// every path out, a panic included: a slot that is never released is lost
// until the process restarts. A panic also closes the request's dashboard row
// as aborted before it propagates.
func (h *Handler) attempt(w http.ResponseWriter, r *http.Request, lease *route.Lease, d *dispatch, label string) (res execResult) {
	defer func() {
		lease.Release()
		if rv := recover(); rv != nil {
			if d.record {
				h.d.Activity.EmitRequestTask(d.rid, -1, d.taskID, "%s", meshapi.RequestAborted(d.model, label, time.Since(d.start).Round(time.Millisecond)))
			}
			panic(rv)
		}
	}()
	t := lease.Target()
	if t.Local {
		body, strip := h.localBodyFor(d)
		granted := time.Now()
		res = serveLocal(w, r, body, lease.Backend(), d.model, h.d.Self, d.thinkDisabled, strip)
		res.Granted = granted
		return res
	}
	res = forwardToPeer(w, r, d.body, t, h.d.Auth, h.d.Self)
	if res.Outcome == outcomeRetryable {
		lease.Refused() // before Release: its wake must not hand the peer back (Decision 18)
	}
	return res
}

// rewriteModel sets the body's model field, keeping every other field as it
// was. A body that does not decode as an object is left alone.
//
// The common body is rewritten in place: the new name is spliced over the old
// value's bytes (see modelValueSpan), and nothing else is touched. Anything
// the span cannot prove falls back to a decode and re-encode of the object.
// The input is never modified.
func rewriteModel(body []byte, model string) []byte {
	if start, end, ok := modelValueSpan(body); ok {
		name, err := json.Marshal(model)
		if err != nil {
			return body
		}
		out := make([]byte, 0, len(body)-(end-start)+len(name))
		out = append(out, body[:start]...)
		out = append(out, name...)
		return append(out, body[end:]...)
	}
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(body, &generic); err != nil {
		return body
	}
	name, err := json.Marshal(model)
	if err != nil {
		return body
	}
	generic["model"] = name
	rewritten, err := json.Marshal(generic)
	if err != nil {
		return body
	}
	return rewritten
}

// endUnserved clears the dashboard row a started event opened for a request
// that ends without being served. The grammar has no failed form, so it is
// logged as done, which is what v1 logged for a failed peer forward.
func (h *Handler) endUnserved(d *dispatch, label string) {
	if !d.record {
		return
	}
	h.d.Activity.EmitRequestTask(d.rid, -1, d.taskID, "%s", meshapi.RequestDone(d.model, label, time.Since(d.start).Round(time.Millisecond)))
}

func (h *Handler) writeAcquireError(w http.ResponseWriter, err error, model, host string) {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The client is gone; there is no one to answer.
	case errors.Is(err, route.ErrModelNotFound):
		httpjson.Error(w, http.StatusNotFound, errTypeNotFound, fmt.Sprintf("model %q not found", model))
	case errors.Is(err, route.ErrHostNotServing):
		httpjson.Error(w, http.StatusNotFound, errTypeNotFound, fmt.Sprintf("model %q is not served by host %q", model, host))
	case errors.Is(err, route.ErrNoFreeSlot):
		httpjson.Error(w, http.StatusTooManyRequests, meshapi.ErrTypeRateLimit, "no free slot")
	case errors.Is(err, route.ErrQueueFull):
		w.Header().Set("Retry-After", queueRetryAfter)
		httpjson.Error(w, http.StatusTooManyRequests, meshapi.ErrTypeRateLimit, fmt.Sprintf("queue for %q is full", model))
	case errors.Is(err, route.ErrQueueTimeout):
		w.Header().Set("Retry-After", queueRetryAfter)
		httpjson.Error(w, http.StatusTooManyRequests, meshapi.ErrTypeRateLimit, fmt.Sprintf("no free slot for %q within %s", model, h.d.Router.QueueTimeout()))
	default:
		log.Printf("proxy: routing %q: %v", model, err)
		httpjson.Error(w, http.StatusInternalServerError, errTypeServer, "routing failed")
	}
}

// handleModels serves /v1/models: the OpenAI-compatible list, enriched with
// the window a client may size a prompt against.
//
// The enrichment happens HERE rather than in ModelEntries because
// discovery.Models reads ModelEntries to build the record — enriching the
// input would make the endpoint its own source. ModelEntries stays the raw
// list of what exists; this is one rendering of the record over it.
func (h *Handler) handleModels(w http.ResponseWriter) {
	models := discovery.Models(h)
	data := make([]meshapi.ModelEntry, 0, len(models))
	for _, m := range models {
		// Both spellings carry ServedContext, and both are omitted when no
		// host can say. Neither gets an output ceiling: viiwork has none
		// distinct from the shared window, and this format lets a field be
		// absent, so saying nothing is both easier and true.
		data = append(data, meshapi.ModelEntry{
			ID: m.Name, Object: "model", OwnedBy: m.Kind, Target: m.Target,
			MaxModelLen: m.ServedContext, ContextLength: m.ServedContext,
		})
	}
	httpjson.Write(w, http.StatusOK, meshapi.ModelsResponse{Object: "list", Data: data})
}

// ModelEntries is everything this node lists on /v1/models, in the same order,
// and without the discovered numbers — it is discovery.Models' input, not its
// output. It is exported so the record can be built from exactly the models
// the API names: one list, many renderings.
//
// It collects local models, then peers', pipelines' and the extra entries; a
// duplicate id keeps its first entry in that order.
func (h *Handler) ModelEntries() []meshapi.ModelEntry {
	seen := map[string]bool{}
	data := []meshapi.ModelEntry{}
	add := func(e meshapi.ModelEntry) {
		if e.ID == "" || seen[e.ID] {
			return
		}
		seen[e.ID] = true
		e.Object = "model"
		data = append(data, e)
	}
	if h.d.Local != nil {
		for _, mc := range h.d.Local.Capacity() {
			add(meshapi.ModelEntry{ID: mc.Name, OwnedBy: meshapi.OwnedByLocal})
		}
	}
	if h.d.Reports != nil {
		// Whatever the report's age: a model listed once stays listed while its
		// member is known (Decision 4).
		for _, rep := range h.d.Reports.Reports() {
			if rep.Node == h.d.Self {
				continue
			}
			for _, mc := range rep.Models {
				add(meshapi.ModelEntry{ID: mc.Name, OwnedBy: meshapi.OwnedByPeer})
			}
		}
	}
	if h.d.Pipelines != nil {
		for _, e := range h.d.Pipelines.VirtualModels() {
			add(e)
		}
	}
	if h.d.ExtraModels != nil {
		for _, e := range h.d.ExtraModels() {
			add(e)
		}
	}
	sort.Slice(data, func(i, j int) bool { return data[i].ID < data[j].ID })
	return data
}

func (h *Handler) handleCapacity(w http.ResponseWriter) {
	var local []meshapi.ModelCapacity
	if h.d.Local != nil {
		local = h.d.Local.Capacity()
	}
	models := make([]meshapi.ModelCapacity, len(local))
	for i, m := range local {
		m.Queued = h.d.Router.QueueLen(m.Name)
		if h.d.PublishPerf && h.d.Perf != nil {
			if s, ok := h.d.Perf.Score(m.Name); ok {
				m.TTFTOverheadMs, m.PrefillMsPer1k, m.PerfSamples = s.OverheadMs, s.MsPer1k, s.Samples
			}
		}
		models[i] = m
	}
	httpjson.Write(w, http.StatusOK, meshapi.CapacityResponse{Node: h.d.Self, Ver: h.d.Version, Models: models})
}
