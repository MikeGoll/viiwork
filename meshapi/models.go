package meshapi

// ModelEntry.OwnedBy values. local, peer and pipeline are the v1 values.
const (
	OwnedByLocal    = "local"
	OwnedByPeer     = "peer"
	OwnedByPipeline = "pipeline"
	OwnedByAlias    = "alias"
)

// ModelEntry is one OpenAI-compatible model list entry. Target is set only
// for aliases.
//
// MaxModelLen and ContextLength both carry the SAME number — the total
// prompt+completion budget per slot the fleet guarantees — in two spellings,
// because OpenAI's /v1/models was never extended to carry a window and the
// ecosystem has converged on these two names for it instead. vLLM spells it
// max_model_len, OpenRouter spells it context_length, and both define it as a
// total, which is what makes them safe to emit from one figure. A prompt-only
// ceiling such as LiteLLM's max_input_tokens is NOT the same quantity and is
// deliberately not here.
//
// Additive and omitempty, which is the permitted form of a C4 change: machines
// are upgraded one at a time, so an older node simply omits them and a reader
// gets "this node cannot say" rather than a measured zero. No output ceiling
// belongs here — viiwork has none distinct from the shared window, so a field
// for one could only be a guess.
type ModelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
	Target  string `json:"target,omitempty"`

	MaxModelLen   int64 `json:"max_model_len,omitempty"`  // vLLM's spelling
	ContextLength int64 `json:"context_length,omitempty"` // OpenRouter's spelling
}

// ModelsResponse is the body of GET PathModels.
type ModelsResponse struct {
	Object string       `json:"object"`
	Data   []ModelEntry `json:"data"`
}
