package engine

import "context"

// This file is the whole set of optional engine capabilities, kept together so
// that it can be read at once. Each is found by type assertion at its call
// site: an engine implements what it can answer honestly and nothing else,
// because a method returning a plausible-looking zero is worse than an absent
// one — the mesh renders absent as "unknown" and zero as fact.
//
// Adding a capability is additive and breaks no existing engine. Widening
// Engine instead would force every engine to implement a method it must then
// lie in.

// OptionsValidator validates this engine's configuration block. path is the
// model's position ("models[0]"), so every error can name the field that is
// wrong — C1's rule does not stop being true because the rule moved into the
// engine. Called during config validation, before anything starts.
type OptionsValidator interface {
	ValidateOptions(path string, s Spec) error
}

// CPURunner is implemented by an engine that can serve with no GPUs. Not
// implementing it means gpus: is required for that engine. The rule was never
// about llama.cpp by name; it is about an engine that can run without a card.
type CPURunner interface {
	RunsOnCPU() bool
}

// TokenProgressReader is implemented by an engine that can report PER-REQUEST
// token progress, and is called in place of Load on every load tick.
// Cumulative token counters are NOT this: a running total in a field the
// dashboard renders as "tokens left in this request" is worse than a blank.
type TokenProgressReader interface {
	LoadProgress(ctx context.Context, s Spec, addr string) (load Load, decoded, remain int64, err error)
}

// GPUBindingReader is implemented by an engine that reports which card it
// actually bound, and is called once on the transition to healthy. A mismatch
// against the host inventory is REPORTED, never acted on: the backend is
// serving correctly, and taking it out of the mesh would trade a wrong label
// for a lost GPU. ok is false when the engine says nothing, which is not a
// mismatch.
type GPUBindingReader interface {
	BoundGPU(ctx context.Context, addr string) (uuid string, ok bool, err error)
}

// Versioner is an engine that can say which version of itself is installed
// and which version this viiwork needs. Optional, like every capability here:
// an engine without it declares no requirement and is never checked
// (FreeToken's rolling nightly has no version to read).
type Versioner interface {
	// MinVersion is the oldest engine this viiwork works with, in the
	// engine's own spelling; "" means no requirement. Raise it in the change
	// that starts relying on a newer engine.
	MinVersion() string
	// Version runs the binary s's options name and reports its version.
	Version(ctx context.Context, s Spec) (string, error)
	// AtLeast reports whether installed satisfies min in this engine's order.
	AtLeast(installed, min string) (bool, error)
}

// UsageReporting says what an engine's streamed chat responses tell the proxy
// about token usage (performance routing, spec §4).
type UsageReporting struct {
	// Unasked: a streamed response ends with a usage chunk even when the
	// client did not set stream_options.include_usage. When false the proxy
	// asks for usage and strips the chunk again for a client that did not.
	Unasked bool
	// CachedTokens: usage.prompt_tokens_details.cached_tokens is present
	// whenever any prompt token came from cache, so its absence means zero.
	// When false an absent field means "unknown" and the sample is dropped.
	CachedTokens bool
}

// UsageReporter declares how the engine reports token usage, which is what
// performance routing measures with. Optional, like every capability here: an
// engine that does not implement it is assumed to report nothing it was not
// asked for and no cached count, so viiwork never asks it for usage, it
// yields no time-to-first-token sample in practice, and the scored router
// treats it as learning (priced at the fleet median, plus a 5% trickle).
type UsageReporter interface {
	UsageReporting() UsageReporting
}

// UsageOf is e's UsageReporting, or the zero value.
func UsageOf(e Engine) UsageReporting {
	if u, ok := e.(UsageReporter); ok {
		return u.UsageReporting()
	}
	return UsageReporting{}
}

// ReasoningSeparator is implemented by an engine whose streamed responses keep
// reasoning apart from the answer: reasoning arrives as
// delta.reasoning_content with no <think> tags, and the answer follows as
// delta.content. Such a stream needs no rewriting, and for a client that did
// not ask for thinking the proxy passes it through as it is — the client
// shows the reasoning field or ignores it — instead of renaming reasoning to
// content, the rule for every other engine, written for a llama-server that
// may put its whole answer in reasoning_content. Declare it only for a server
// that always writes the answer to content.
type ReasoningSeparator interface {
	SeparatesReasoning() bool
}

// SeparatesReasoning reports whether e declares a separate reasoning channel.
func SeparatesReasoning(e Engine) bool {
	r, ok := e.(ReasoningSeparator)
	return ok && r.SeparatesReasoning()
}

// PerfKeyer is implemented by an engine whose model speed depends on
// something the node's model entry does not show — a config file of the
// engine's own that the entry only names. PerfKey returns a short string that
// changes when that something does; the node adds it to the key its saved
// performance baseline is filed under, so the change drops the baseline as a
// change of args does. "" adds nothing. Called at start and on reload, with
// the Spec ValidateOptions gets.
type PerfKeyer interface {
	PerfKey(s Spec) string
}
