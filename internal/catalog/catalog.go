// Package catalog serves a viiwork node's models as an OpenCode model
// catalogue, so that an OpenCode client discovers the fleet instead of being
// told about it in a hand-written config.
//
// OpenCode has no /v1/models discovery for openai-compatible providers — that
// is an open feature request, not a feature — but it does take its whole model
// catalogue from a URL: OPENCODE_MODELS_URL, from which it fetches
// <url>/api.json, caches it for five minutes and refreshes it in the
// background. Pointing that at a node makes every model on the mesh appear in
// OpenCode, and keeps appearing and disappearing as the fleet changes, with
// nothing in the client config but the provider's name.
//
// This package is one SERIALIZER over internal/discovery's record and holds no
// model state of its own. The shape is models.dev's, because that is what
// OpenCode parses. It is NOT meshapi: nothing here crosses a machine boundary
// between viiwork nodes, and the fields are a third party's to change, so
// freezing them here would freeze the wrong contract. This is an edge-only
// compatibility surface, like a client API dialect (C8).
package catalog

import (
	"net/http"
	"time"

	"github.com/janit/viiwork/v2/internal/discovery"
)

// defaultContext is the window advertised for a model nothing can size — a
// pipeline, which is node-local and never appears in a capacity report, or an
// alias whose target nobody serves.
//
// "Absent is not zero" holds in the record, which leaves such a model at zero.
// It cannot hold HERE, because this format has no way to spell "unknown":
// OpenCode reads limit.context as a window, not as a claim about knowledge,
// and would read a zero as one. Substituting a figure is a serializer's
// privilege precisely because the format demands one — it is this document's
// answer, not a measurement viiwork is claiming.
//
// The figure is deliberately conservative, because the two errors are not
// symmetric — too large a window makes every request fail at the backend, too
// small a one only shortens the conversation.
const defaultContext = 8192

// sdkPackage is the driver OpenCode loads for the provider. Without it the
// provider is treated as one of OpenCode's own natively-implemented APIs and
// never reaches the node.
const sdkPackage = "@ai-sdk/openai-compatible"

// releaseDate is a models.dev field with no meaning for a model an operator
// loaded themselves: the catalogue is the fleet's, and when a checkpoint was
// published is not something a node knows. It is emitted because OpenCode
// sorts on it, and empty sorts last, which is where an unknown date belongs.
const releaseDate = ""

// Source is the node's published model views. *proxy.Handler satisfies it.
type Source = discovery.Source

// Config is the served catalogue's identity.
type Config struct {
	// ProviderID is the provider key, and so the "provider/model" prefix a
	// user types in OpenCode.
	ProviderID string
	// ProviderName is the display name. OpenCode orders its model picker by
	// provider name, byte by byte, after its own entry — so this string, not
	// the id, decides where the fleet sits in that list.
	ProviderName string
	// BaseURL overrides the endpoint advertised to clients. Empty means the
	// address the request arrived on, which is right for every node in a mesh
	// where any node is an entry point.
	BaseURL string
	// Upstream is a models.dev-shaped catalogue to chain, without the
	// /api.json suffix. Empty means none, and no outbound request: a node
	// reaches the internet only when an operator says so.
	Upstream string
	// UpstreamTTL is how long a fetched upstream is reused.
	UpstreamTTL time.Duration
	// Client fetches the upstream. Nil means one with a sane timeout.
	Client *http.Client
}

// Catalog is a models.dev api.json document: provider id -> provider.
type Catalog map[string]Provider

// Provider is one models.dev provider entry.
type Provider struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Env is the environment variables holding this provider's API key.
	// Empty: viiwork authenticates nothing.
	Env    []string         `json:"env"`
	NPM    string           `json:"npm,omitempty"`
	API    string           `json:"api,omitempty"`
	Models map[string]Model `json:"models"`
}

// Model is one models.dev model entry.
type Model struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	ReleaseDate string     `json:"release_date"`
	Attachment  bool       `json:"attachment"`
	Reasoning   bool       `json:"reasoning"`
	Temperature bool       `json:"temperature"`
	ToolCall    bool       `json:"tool_call"`
	Cost        Cost       `json:"cost"`
	Limit       Limit      `json:"limit"`
	Modalities  Modalities `json:"modalities"`
}

// Cost is per million tokens. Local inference is free at the point of use; the
// electricity is in the energy store, not here.
type Cost struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
}

// Limit is the model's window.
type Limit struct {
	// Context is the total prompt+completion budget per slot.
	Context int64 `json:"context"`
	// Output is a CLIENT-SIDE BUDGETING HINT required by this format, not a
	// capability viiwork has. See outputLimit.
	Output int64 `json:"output"`
}

// Modalities is what the model takes and returns.
type Modalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

// Build renders the fleet's models as a catalogue. baseURL is the node's own
// OpenAI-compatible endpoint as the requesting client reached it.
func Build(cfg Config, models []discovery.Model, baseURL string) Catalog {
	out := map[string]Model{}
	for _, m := range models {
		window := m.ServedContext
		if window == 0 {
			window = defaultContext
		}
		// An alias renders as what it points at: a bare alias name in the
		// picker hides which model the work actually goes to.
		label := m.Name
		if m.Target != "" {
			label = m.Name + " -> " + m.Target
		}
		out[m.Name] = Model{
			ID:          m.Name,
			Name:        label,
			ReleaseDate: releaseDate,
			Temperature: true,
			ToolCall:    m.Tools,
			Limit:       Limit{Context: window, Output: outputLimit(window)},
			Modalities:  Modalities{Input: []string{"text"}, Output: []string{"text"}},
		}
	}
	name := cfg.ProviderName
	if name == "" {
		name = cfg.ProviderID
	}
	return Catalog{cfg.ProviderID: Provider{
		ID:     cfg.ProviderID,
		Name:   name,
		Env:    []string{},
		NPM:    sdkPackage,
		API:    baseURL,
		Models: out,
	}}
}

// Output limits, in tokens: the cap is the most worth asking a local model
// for, the step is what the usual sizes round to.
const (
	outputStep = 1024
	maxOutput  = 32768
)

// outputLimit is the most OpenCode may ask a model to generate.
//
// READ THIS BEFORE COPYING IT INTO ANOTHER SERIALIZER. viiwork has no output
// ceiling distinct from the shared window: a slot's context is one
// prompt+completion budget and the split between them is the client's to
// choose. This figure is therefore a GUESS — a quarter of the window — and it
// lives here only because models.dev requires limit.output and OpenCode has no
// sane default for it. Omitting the field is what made OpenCode ask for 32000
// tokens and have every request rejected by a backend with a smaller window.
//
// So it is a client-side budgeting hint that this document must carry, NOT a
// capability viiwork is claiming. It must not appear in internal/discovery,
// and it must not be emitted by a serializer whose format lets the field be
// absent: there, saying nothing is both easier and true.
//
// The rounding has no floor, deliberately. A floor is what a 512-token model
// on a real node exposed: rounding UP to a step asks a backend for more output
// than its whole window, which is the failure this figure exists to prevent.
func outputLimit(window int64) int64 {
	out := window / 4
	if out > maxOutput {
		out = maxOutput
	}
	if out >= outputStep {
		out -= out % outputStep
	}
	if out < 1 {
		out = 1
	}
	return out
}
