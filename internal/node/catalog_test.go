package node

// The OpenCode catalogue through a real node: real config, real engines, real
// supervisor, and the document an OpenCode client actually fetches.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/janit/viiwork/v2/internal/catalog"
	"github.com/janit/viiwork/v2/mesh/meshtest"
)

func fetchCatalog(t *testing.T, tn *testNode) catalog.Catalog {
	t.Helper()
	resp, err := http.Get(tn.url(catalog.Path))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", catalog.Path, resp.StatusCode)
	}
	var got catalog.Catalog
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

// Every model the node serves is in the catalogue, sized from the context the
// operator configured — whichever engine serves it. This is the whole promise
// of the endpoint: point a client at a node and the fleet appears, correctly
// sized, with nothing written down twice.
func TestCatalogServesEveryModelWithItsRealContext(t *testing.T) {
	tn := startNodeYAML(t, meshtest.NewNetwork(), "n1", threeEngineModels(), "", nil)
	waitForModels(t, tn, "lc", "vl", "ft")

	got := fetchCatalog(t, tn)

	p, ok := got["viiwork"]
	if !ok {
		t.Fatalf("no viiwork provider in the catalogue: %v", got)
	}
	if p.NPM != "@ai-sdk/openai-compatible" {
		t.Errorf("npm = %q", p.NPM)
	}
	// The same per-slot context /v1/capacity reports for each engine.
	for name, wantCtx := range map[string]int64{"lc": 512, "vl": 16384, "ft": 32768} {
		m, ok := p.Models[name]
		if !ok {
			t.Errorf("%s missing from the catalogue: %v", name, p.Models)
			continue
		}
		if m.Limit.Context != wantCtx {
			t.Errorf("%s context = %d, want the configured %d", name, m.Limit.Context, wantCtx)
		}
		if m.Limit.Output <= 0 || m.Limit.Output > m.Limit.Context {
			t.Errorf("%s output limit %d is not inside its %d window", name, m.Limit.Output, m.Limit.Context)
		}
		if !m.ToolCall {
			t.Errorf("%s cannot be an agent: tool_call is false", name)
		}
	}
}

// The catalogue points clients back at the node they reached, not at a name
// baked in at build time: any node is an entry point.
func TestCatalogAdvertisesTheNodeThatServedIt(t *testing.T) {
	tn := startNodeYAML(t, meshtest.NewNetwork(), "n1", threeEngineModels(), "", nil)
	waitForModels(t, tn, "lc")

	got := fetchCatalog(t, tn)

	if want := tn.url("/v1"); got["viiwork"].API != want {
		t.Errorf("api = %q, want %q", got["viiwork"].API, want)
	}
}
