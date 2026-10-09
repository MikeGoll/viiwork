package node

// The three discovery surfaces through a real node: real config, real engines,
// real supervisor, and the documents the clients actually fetch. What matters
// here is not that each one parses — the serializer tests pin that — but that
// all three agree, because they are renderings of one record and a divergence
// is exactly the bug the record exists to prevent.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/janit/viiwork/v2/internal/modelinfo"
	"github.com/janit/viiwork/v2/mesh/meshtest"
	"github.com/janit/viiwork/v2/meshapi"
)

func fetchModelInfo(t *testing.T, tn *testNode) modelinfo.Response {
	t.Helper()
	resp, err := http.Get(tn.url(modelinfo.Path))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", modelinfo.Path, resp.StatusCode)
	}
	var got modelinfo.Response
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

func fetchModels(t *testing.T, tn *testNode) meshapi.ModelsResponse {
	t.Helper()
	resp, err := http.Get(tn.url(meshapi.PathModels))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got meshapi.ModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

// Roo Code's LiteLLM provider reads model_info.max_input_tokens and otherwise
// invents a 200000-token context window. This is the endpoint that makes the
// difference between a fleet discovered and a fleet mis-sized by an order of
// magnitude.
func TestModelInfoServesEveryModelWithItsRealContext(t *testing.T) {
	tn := startNodeYAML(t, meshtest.NewNetwork(), "n1", threeEngineModels(), "", nil)
	waitForModels(t, tn, "lc", "vl", "ft")

	got := fetchModelInfo(t, tn)

	by := map[string]modelinfo.Entry{}
	for _, e := range got.Data {
		by[e.ModelName] = e
	}
	for name, wantCtx := range map[string]int64{"lc": 512, "vl": 16384, "ft": 32768} {
		e, ok := by[name]
		if !ok {
			t.Errorf("%s missing: %+v", name, got.Data)
			continue
		}
		if e.ModelInfo.MaxInputTokens != wantCtx {
			t.Errorf("%s max_input_tokens = %d, want the configured %d", name, e.ModelInfo.MaxInputTokens, wantCtx)
		}
		if e.LiteLLMParams.Model != name {
			t.Errorf("%s litellm_params.model = %q", name, e.LiteLLMParams.Model)
		}
		if !e.ModelInfo.SupportsFunctionCalling {
			t.Errorf("%s cannot be an agent", name)
		}
	}
}

// One record, three renderings. A model that appears in one document and not
// another is a model a user can see but not call, or call but not see — and
// the three are read by different clients, so nobody would notice.
func TestTheThreeDiscoverySurfacesNameTheSameModels(t *testing.T) {
	tn := startNodeYAML(t, meshtest.NewNetwork(), "n1", threeEngineModels(), "", nil)
	waitForModels(t, tn, "lc", "vl", "ft")

	models := fetchModels(t, tn)
	info := fetchModelInfo(t, tn)
	cat := fetchCatalog(t, tn)["viiwork"]

	if len(models.Data) != len(info.Data) || len(models.Data) != len(cat.Models) {
		t.Fatalf("different lengths: /v1/models %d, model_info %d, catalogue %d",
			len(models.Data), len(info.Data), len(cat.Models))
	}
	infoCtx := map[string]int64{}
	for _, e := range info.Data {
		infoCtx[e.ModelName] = e.ModelInfo.MaxInputTokens
	}
	for _, e := range models.Data {
		if _, ok := cat.Models[e.ID]; !ok {
			t.Errorf("%s is on /v1/models but not in the catalogue", e.ID)
		}
		ctx, ok := infoCtx[e.ID]
		if !ok {
			t.Errorf("%s is on /v1/models but not in model_info", e.ID)
			continue
		}
		// The one number the whole feature exists to publish has to be the
		// same number wherever a client reads it.
		if ctx != e.MaxModelLen || ctx != e.ContextLength {
			t.Errorf("%s: model_info says %d, /v1/models says %d/%d",
				e.ID, ctx, e.MaxModelLen, e.ContextLength)
		}
		if c := cat.Models[e.ID].Limit.Context; c != e.MaxModelLen {
			t.Errorf("%s: catalogue says %d, /v1/models says %d", e.ID, c, e.MaxModelLen)
		}
	}
}

// A write to a discovery document has no meaning, and an endpoint that answers
// one invites a client to try.
func TestModelInfoIsReadOnly(t *testing.T) {
	tn := startNodeYAML(t, meshtest.NewNetwork(), "n1", threeEngineModels(), "", nil)
	waitForModels(t, tn, "lc")

	resp, err := http.Post(tn.url(modelinfo.Path), "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST %s = %d, want 404", modelinfo.Path, resp.StatusCode)
	}
}
