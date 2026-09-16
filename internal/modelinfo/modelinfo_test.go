package modelinfo

// A serializer over the discovery record, so every rule is testable without a
// mesh, a GPU or a Roo Code install. The tests go through discovery.Models
// rather than hand-building records: what has to stay true is the whole path
// from what the node serves to what the client reads.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/janit/viiwork/v2/internal/discovery"
	"github.com/janit/viiwork/v2/meshapi"
)

type fakeSource struct {
	entries []meshapi.ModelEntry
	fleet   []meshapi.FleetModel
}

func (f fakeSource) ModelEntries() []meshapi.ModelEntry { return f.entries }
func (f fakeSource) FleetModels() []meshapi.FleetModel  { return f.fleet }

func entry(id, ownedBy string) meshapi.ModelEntry {
	return meshapi.ModelEntry{ID: id, Object: "model", OwnedBy: ownedBy}
}

func served(name string, ctx int64) meshapi.FleetModel {
	slots := 4
	return meshapi.FleetModel{
		Name: name, Ctx: ctx, Engine: "llamacpp",
		Hosts: []meshapi.FleetHost{{Node: "gb1", Slots: &slots, Ctx: &ctx}},
	}
}

func build(src fakeSource) Response { return Build(discovery.Models(src)) }

func byName(r Response, name string) (Entry, bool) {
	for _, e := range r.Data {
		if e.ModelName == name {
			return e, true
		}
	}
	return Entry{}, false
}

// Every model a client can name has to appear here too, or the endpoint that
// exists to spare the user typing model names becomes a second list to keep
// in step with the first.
func TestEveryModelTheNodeServesIsListed(t *testing.T) {
	got := build(fakeSource{entries: []meshapi.ModelEntry{
		entry("tg", meshapi.OwnedByLocal),
		entry("dsv4", meshapi.OwnedByPeer),
		entry("summarise", meshapi.OwnedByPipeline),
	}})

	if len(got.Data) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(got.Data), got.Data)
	}
	for _, name := range []string{"dsv4", "summarise", "tg"} {
		if _, ok := byName(got, name); !ok {
			t.Errorf("model %q missing: %+v", name, got.Data)
		}
	}
}

// This is the field the endpoint exists for. Roo Code reads model_info's
// max_input_tokens into its contextWindow, and without it invents 200000.
func TestMaxInputTokensCarriesTheServedContext(t *testing.T) {
	got := build(fakeSource{
		entries: []meshapi.ModelEntry{entry("tg", meshapi.OwnedByLocal)},
		fleet:   []meshapi.FleetModel{served("tg", 98304)},
	})

	e, _ := byName(got, "tg")
	if e.ModelInfo.MaxInputTokens != 98304 {
		t.Errorf("max_input_tokens = %d, want 98304", e.ModelInfo.MaxInputTokens)
	}
}

// viiwork has no output ceiling distinct from the shared window, so it must
// claim none here. models.dev forces internal/catalog to guess one; this
// format does not, and a guess repeated across serializers is how a guess
// turns into a fact.
func TestNoOutputCeilingIsClaimed(t *testing.T) {
	got := build(fakeSource{
		entries: []meshapi.ModelEntry{entry("tg", meshapi.OwnedByLocal)},
		fleet:   []meshapi.FleetModel{served("tg", 98304)},
	})

	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"max_output_tokens", "max_tokens", "output_cost_per_token"} {
		if strings.Contains(string(b), field) {
			t.Errorf("%s appears on the wire; viiwork has no such number: %s", field, b)
		}
	}
}

// Absent is not zero. A pipeline appears in no capacity report, so nothing can
// size it — and a max_input_tokens of 0 would be read as a measurement, not as
// silence. The client's own default is wrong, but it is the client's.
func TestAModelNothingCanSizeClaimsNoWindow(t *testing.T) {
	got := build(fakeSource{entries: []meshapi.ModelEntry{entry("summarise", meshapi.OwnedByPipeline)}})

	e, ok := byName(got, "summarise")
	if !ok {
		t.Fatal("pipeline dropped; it is still a name a client may call")
	}
	if e.ModelInfo.MaxInputTokens != 0 {
		t.Errorf("max_input_tokens = %d, want 0", e.ModelInfo.MaxInputTokens)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "max_input_tokens") {
		t.Errorf("a zero window was serialised; absent is not zero: %s", b)
	}
}

// LiteLLM's readers key off litellm_params.model and skip an entry without
// one, so the field is the difference between being discovered and being
// silently dropped. It names the model as viiwork serves it.
func TestEntriesCarryTheParamsAReaderKeysOff(t *testing.T) {
	got := build(fakeSource{
		entries: []meshapi.ModelEntry{entry("tg", meshapi.OwnedByLocal)},
		fleet:   []meshapi.FleetModel{served("tg", 32768)},
	})

	e, _ := byName(got, "tg")
	if e.LiteLLMParams.Model != "tg" {
		t.Errorf("litellm_params.model = %q, want tg", e.LiteLLMParams.Model)
	}
	if e.ModelInfo.Mode != "chat" {
		t.Errorf("mode = %q, want chat", e.ModelInfo.Mode)
	}
	if !e.ModelInfo.SupportsFunctionCalling {
		t.Error("supports_function_calling is false: no client would run it as an agent")
	}
}

// An alias is a name for another model's capacity and has to carry it: the
// aliases are the mesh-wide stable names operators are told to configure.
func TestAnAliasCarriesItsTargetsWindow(t *testing.T) {
	got := build(fakeSource{
		entries: []meshapi.ModelEntry{
			entry("dsv4", meshapi.OwnedByLocal),
			{ID: "big", Object: "model", OwnedBy: meshapi.OwnedByAlias, Target: "dsv4"},
		},
		fleet: []meshapi.FleetModel{served("dsv4", 98304)},
	})

	e, _ := byName(got, "big")
	if e.ModelInfo.MaxInputTokens != 98304 {
		t.Errorf("alias max_input_tokens = %d, want its target's 98304", e.ModelInfo.MaxInputTokens)
	}
	if e.LiteLLMParams.Model != "big" {
		t.Errorf("litellm_params.model = %q: a client must send the name it picked", e.LiteLLMParams.Model)
	}
}

// The endpoint answers on GET and nothing else; a write here has no meaning.
func TestHandlerServesGetOnly(t *testing.T) {
	src := fakeSource{
		entries: []meshapi.ModelEntry{entry("tg", meshapi.OwnedByLocal)},
		fleet:   []meshapi.FleetModel{served("tg", 32768)},
	}
	h := NewHandler(src)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	var resp Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("%q: %v", rec.Body.String(), err)
	}
	if len(resp.Data) != 1 || resp.Data[0].ModelInfo.MaxInputTokens != 32768 {
		t.Errorf("body = %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, Path, nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST = %d, want 404", rec.Code)
	}
}
