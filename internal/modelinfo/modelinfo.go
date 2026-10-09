// Package modelinfo serves a viiwork node's models in LiteLLM's /v1/model/info
// shape, so that a client which knows that shape discovers the fleet with no
// user configuration at all.
//
// The client this exists for is Roo Code. Its generic OpenAI-compatible
// provider makes the user hand-type a context window for every model; its
// LiteLLM provider reads one from this endpoint. Same fleet, same transport —
// the only difference is whether the numbers are discovered or typed.
//
// This package is one SERIALIZER over internal/discovery's record and holds no
// model state of its own. The shape is LiteLLM's because that is what the
// readers parse, and like internal/catalog it is an edge-only compatibility
// surface (C8's neighbourhood), deliberately not meshapi: nothing here crosses
// a boundary between viiwork nodes, and the fields are a third party's to
// change.
//
// Serving this shape does NOT make a node a LiteLLM proxy. Only the discovery
// document is LiteLLM's; inference stays on the OpenAI-compatible endpoints a
// node already serves, which is the transport every one of these clients
// speaks anyway. The rule from §5 of the design applies in reverse and is the
// reason that is safe: a node must never advertise a dialect it will not then
// accept requests in, and here it advertises none.
package modelinfo

import (
	"encoding/json"
	"net/http"

	"github.com/janit/viiwork/v2/internal/discovery"
)

// Path is where the readers look.
const Path = "/v1/model/info"

// Response is the document: LiteLLM answers with a bare list under "data".
type Response struct {
	Data []Entry `json:"data"`
}

// Entry is one model.
type Entry struct {
	ModelName string `json:"model_name"`
	// LiteLLMParams is how a reader learns what to put in the request body.
	// It is not decoration: a reader that finds no litellm_params.model skips
	// the entry, so leaving it out means being silently undiscovered.
	LiteLLMParams Params `json:"litellm_params"`
	ModelInfo     Info   `json:"model_info"`
}

// Params is the request LiteLLM would make. viiwork serves the model itself,
// so the only field with meaning here is the name a client puts in the body.
type Params struct {
	Model string `json:"model"`
}

// Info is what a client may rely on.
//
// Note what is NOT here, and do not add it: max_output_tokens, max_tokens, and
// any cost. viiwork has no output ceiling distinct from the shared window —
// a slot's context is one prompt+completion budget and the split is the
// client's to choose — so any output figure could only be a guess. models.dev
// forces internal/catalog to make that guess because the field is required
// there; this format lets it be absent, and a guess repeated across
// serializers is how a guess becomes a fact. Local inference is also free at
// the point of use: the electricity is in the energy store, not in a
// per-token price.
type Info struct {
	// MaxInputTokens is THE ONE PLACE viiwork knowingly approximates, and it
	// is a deliberate choice rather than an oversight.
	//
	// LiteLLM defines it as a prompt-only ceiling. viiwork's number is a
	// shared prompt+completion budget, so a client that budgets input+output
	// against it will overshoot slightly. It is emitted anyway because it is
	// still a true hard bound — the server really will accept a prompt of
	// that size, it just leaves no room to answer — and because the
	// alternative is not "no claim": Roo Code defaults contextWindow to
	// 200000 when the field is absent, which is wrong by an order of
	// magnitude on every model on this fleet. A slightly generous true bound
	// beats an invented one.
	//
	// Omitted when no host can say, because absent is not zero and a 0 here
	// would be read as a measurement.
	MaxInputTokens int64 `json:"max_input_tokens,omitempty"`
	// Mode is what the model is for. Everything viiwork serves on this seam
	// is a chat model.
	Mode string `json:"mode,omitempty"`
	// SupportsFunctionCalling decides whether a client will let the model be
	// an agent, which is the only reason to point a coding client at a fleet.
	// It is a property of the seam — one OpenAI-compatible endpoint that
	// passes tool definitions straight through — rather than of a checkpoint.
	SupportsFunctionCalling bool `json:"supports_function_calling"`
}

// Build renders the fleet's models in LiteLLM's shape.
func Build(models []discovery.Model) Response {
	out := Response{Data: make([]Entry, 0, len(models))}
	for _, m := range models {
		out.Data = append(out.Data, Entry{
			ModelName: m.Name,
			// The name the client picked, not an alias's target: resolution
			// happens once, on the origin node, and a client that sent the
			// resolved name would bypass it.
			LiteLLMParams: Params{Model: m.Name},
			ModelInfo: Info{
				MaxInputTokens:          m.ServedContext,
				Mode:                    "chat",
				SupportsFunctionCalling: m.Tools,
			},
		})
	}
	return out
}

// Handler serves the model-info document on Path.
type Handler struct{ src discovery.Source }

// NewHandler returns a handler over the node's published model views.
func NewHandler(src discovery.Source) *Handler { return &Handler{src: src} }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(Build(discovery.Models(h.src)))
}
