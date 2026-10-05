package catalog

// The catalogue is a pure function over the discovery record, which is itself
// a pure join of the two lists a node already publishes — so every rule is
// testable without a mesh, a browser or an OpenCode install. The tests go
// through discovery.Models rather than hand-building records, because what
// has to stay true is the whole path from what the node serves to what the
// client reads.

import (
	"testing"

	"github.com/janit/viiwork/v2/internal/discovery"
	"github.com/janit/viiwork/v2/meshapi"
)

// fakeSource is the node's two published model views.
type fakeSource struct {
	entries []meshapi.ModelEntry
	fleet   []meshapi.FleetModel
}

func (f fakeSource) ModelEntries() []meshapi.ModelEntry { return f.entries }
func (f fakeSource) FleetModels() []meshapi.FleetModel  { return f.fleet }

func entry(id, ownedBy string) meshapi.ModelEntry {
	return meshapi.ModelEntry{ID: id, Object: "model", OwnedBy: ownedBy}
}

// Every model a client can name on /v1/models has to be nameable in OpenCode
// too, whichever node serves it: that is the whole point of the endpoint.
func TestCatalogListsEveryModelTheNodeServes(t *testing.T) {
	src := fakeSource{
		entries: []meshapi.ModelEntry{
			entry("tg", meshapi.OwnedByLocal),
			entry("dsv4", meshapi.OwnedByPeer),
			entry("summarise", meshapi.OwnedByPipeline),
		},
	}

	got := Build(Config{ProviderID: "viiwork"}, discovery.Models(src), "http://gb1:8086/v1")

	p, ok := got["viiwork"]
	if !ok {
		t.Fatalf("catalog has no viiwork provider: %+v", got)
	}
	for _, name := range []string{"tg", "dsv4", "summarise"} {
		if _, ok := p.Models[name]; !ok {
			t.Errorf("model %q missing from catalog: %+v", name, p.Models)
		}
	}
}

// Context is the one field that must be right. OpenCode sizes its requests
// from limit.context, and a wrong figure is not a cosmetic problem: the
// operator's own config comments record that a missing limit made OpenCode ask
// for 32000 output tokens and the backend reject every request.
func TestContextComesFromTheFleetView(t *testing.T) {
	src := fakeSource{
		entries: []meshapi.ModelEntry{entry("tg", meshapi.OwnedByLocal)},
		fleet:   []meshapi.FleetModel{{Name: "tg", Ctx: 98304}},
	}

	got := Build(Config{ProviderID: "viiwork"}, discovery.Models(src), "http://gb1:8086/v1")

	if c := got["viiwork"].Models["tg"].Limit.Context; c != 98304 {
		t.Errorf("context = %d, want 98304", c)
	}
}

// An alias has no capacity row of its own — it is a name for another model's
// capacity — so it has to inherit its target's window. Without this the
// mesh-wide stable names, the ones operators are told to use, would be the
// only ones sized wrong.
func TestAliasInheritsItsTargetsContext(t *testing.T) {
	src := fakeSource{
		entries: []meshapi.ModelEntry{
			entry("dsv4", meshapi.OwnedByLocal),
			{ID: "big", Object: "model", OwnedBy: meshapi.OwnedByAlias, Target: "dsv4"},
		},
		fleet: []meshapi.FleetModel{{Name: "dsv4", Ctx: 98304}},
	}

	got := Build(Config{ProviderID: "viiwork"}, discovery.Models(src), "http://gb1:8086/v1")

	if c := got["viiwork"].Models["big"].Limit.Context; c != 98304 {
		t.Errorf("alias context = %d, want its target's 98304", c)
	}
}

// A pipeline is node-local and never appears in a capacity report, so nothing
// can say what its window is. It still needs a usable one: OpenCode sizes
// requests from this number, and a zero is not "unknown" to it.
//
// The fallback is deliberately small. Too large a window makes every request
// fail at the backend; too small a one only shortens the conversation.
func TestAModelWithNoCapacityRowGetsAConservativeWindow(t *testing.T) {
	src := fakeSource{entries: []meshapi.ModelEntry{entry("summarise", meshapi.OwnedByPipeline)}}

	got := Build(Config{ProviderID: "viiwork"}, discovery.Models(src), "http://gb1:8086/v1")

	m := got["viiwork"].Models["summarise"]
	if m.Limit.Context != defaultContext {
		t.Errorf("context = %d, want the %d fallback", m.Limit.Context, defaultContext)
	}
	if m.Limit.Context == 0 {
		t.Error("context is zero: OpenCode reads that as a window, not as unknown")
	}
}

// The output limit is derived, not guessed. OpenCode asks for max_tokens up to
// this figure, so a default larger than the backend's window makes every
// request fail — the failure the operator's config comments already record.
// A quarter of the window, capped, leaves room for the prompt — and the
// rounding that tidies the common sizes must never push the figure past the
// window itself, which is what a 512-token model on a real node exposed.
func TestOutputLimitIsAQuarterOfTheWindow(t *testing.T) {
	for _, tc := range []struct {
		ctx  int64
		want int64
	}{
		{98304, 24576},
		{32768, 8192},
		{4096, 1024},
		{2048, 512},     // a small window keeps the quarter rather than a floor
		{512, 128},      // and a tiny one must never be asked for more than it holds
		{999424, 32768}, // never above a cap worth asking for
	} {
		src := fakeSource{
			entries: []meshapi.ModelEntry{entry("tg", meshapi.OwnedByLocal)},
			fleet:   []meshapi.FleetModel{{Name: "tg", Ctx: tc.ctx}},
		}
		got := Build(Config{ProviderID: "viiwork"}, discovery.Models(src), "http://gb1:8086/v1")
		if o := got["viiwork"].Models["tg"].Limit.Output; o != tc.want {
			t.Errorf("ctx %d: output = %d, want %d", tc.ctx, o, tc.want)
		}
	}
}

// The provider has to say which SDK drives it: OpenCode reads npm to decide a
// provider is an AI-SDK one, and a provider without it is treated as native
// and never reaches the node. tool_call decides whether the model may be an
// agent at all, which is the only reason to point a coding client at a fleet.
func TestModelsAreUsableAsAgents(t *testing.T) {
	src := fakeSource{entries: []meshapi.ModelEntry{entry("tg", meshapi.OwnedByLocal)}}

	p := Build(Config{ProviderID: "viiwork"}, discovery.Models(src), "http://gb1:8086/v1")["viiwork"]

	if p.NPM != "@ai-sdk/openai-compatible" {
		t.Errorf("npm = %q, want the openai-compatible SDK", p.NPM)
	}
	if !p.Models["tg"].ToolCall {
		t.Error("tool_call is false: the model could not run as an agent")
	}
	if p.API != "http://gb1:8086/v1" {
		t.Errorf("api = %q, want the node's own endpoint", p.API)
	}
	if got := p.Models["tg"].Modalities.Input; len(got) != 1 || got[0] != "text" {
		t.Errorf("input modalities = %v, want [text]", got)
	}
}

// An alias is a name for another model, and the picker is where that matters:
// a list of bare alias names hides which machine-sized model you are about to
// send work to.
func TestAnAliasIsLabelledWithItsTarget(t *testing.T) {
	src := fakeSource{entries: []meshapi.ModelEntry{
		{ID: "big", Object: "model", OwnedBy: meshapi.OwnedByAlias, Target: "dsv4"},
	}}

	got := Build(Config{ProviderID: "viiwork"}, discovery.Models(src), "http://gb1:8086/v1")

	if name := got["viiwork"].Models["big"].Name; name != "big -> dsv4" {
		t.Errorf("alias label = %q, want %q", name, "big -> dsv4")
	}
}

// OpenCode orders the picker by provider NAME, byte by byte, so the display
// name is the only lever over where the fleet appears in it. Build must not
// quietly substitute the id for a configured name, or the lever does nothing.
func TestProviderNameIsWhatWasConfigured(t *testing.T) {
	src := fakeSource{entries: []meshapi.ModelEntry{entry("tg", meshapi.OwnedByLocal)}}

	got := Build(Config{ProviderID: "viiwork", ProviderName: " viiwork (mesh)"}, discovery.Models(src), "http://gb1:8086/v1")

	if n := got["viiwork"].Name; n != " viiwork (mesh)" {
		t.Errorf("provider name = %q, want the configured one", n)
	}
}

// An unnamed provider would sort first and render as nothing.
func TestAnUnnamedProviderFallsBackToItsID(t *testing.T) {
	src := fakeSource{entries: []meshapi.ModelEntry{entry("tg", meshapi.OwnedByLocal)}}

	got := Build(Config{ProviderID: "viiwork"}, discovery.Models(src), "http://gb1:8086/v1")

	if n := got["viiwork"].Name; n != "viiwork" {
		t.Errorf("provider name = %q, want the id", n)
	}
}
