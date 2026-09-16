//go:build catalogfixture

package node

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/catalog"
	"github.com/janit/viiwork/v2/internal/discovery"
	"github.com/janit/viiwork/v2/internal/modelinfo"
	"github.com/janit/viiwork/v2/meshapi"
)

// TestCatalogFixture serves all three discovery surfaces on 127.0.0.1:18087
// with canned fleet data, so the documents can be checked against the real
// clients:
//
//	go test -tags catalogfixture -run TestCatalogFixture ./internal/node/ &
//	OPENCODE_MODELS_URL=http://127.0.0.1:18087 opencode models
//
// and, for a LiteLLM reader such as Roo Code, a provider pointed at
// http://127.0.0.1:18087 with any API key.
//
// The point is the client, not the server. Go tests prove the documents say
// what viiwork means; only a client proves the client agrees — and an
// integration test caught a real bug this way already, advertising a
// 512-token model a 1024-token output limit. It serves until interrupted, or
// for CATALOGFIXTURE_FOR (a duration) when set.
func TestCatalogFixture(t *testing.T) {
	src := fixtureCatalogSource{
		entries: []meshapi.ModelEntry{
			{ID: "granite-4.2-8b", Object: "model", OwnedBy: meshapi.OwnedByLocal},
			{ID: "DeepSeek-V4-Flash", Object: "model", OwnedBy: meshapi.OwnedByPeer},
			{ID: "Qwen3.8-Flash-Next", Object: "model", OwnedBy: meshapi.OwnedByPeer},
			{ID: "summarise", Object: "model", OwnedBy: meshapi.OwnedByPipeline},
			{ID: "big", Object: "model", OwnedBy: meshapi.OwnedByAlias, Target: "DeepSeek-V4-Flash"},
		},
		// With hosts, so the record's provenance is exercised too: the
		// numbers a client sees come from hosts that could take the request.
		fleet: []meshapi.FleetModel{
			fixtureModel("granite-4.2-8b", 16384, "gb1", "gb2"),
			fixtureModel("DeepSeek-V4-Flash", 98304, "plexie"),
			fixtureModel("Qwen3.8-Flash-Next", 32768, "yeti"),
		},
	}

	h := catalog.NewHandler(catalog.Config{
		ProviderID:   "viiwork",
		ProviderName: " viiwork",
		Upstream:     os.Getenv("CATALOGFIXTURE_UPSTREAM"),
		UpstreamTTL:  time.Hour,
	}, src)

	mux := http.NewServeMux()
	mux.Handle(catalog.Path, h)
	mux.Handle(modelinfo.Path, modelinfo.NewHandler(src))
	// The plain list too, so the two enriched spellings can be eyeballed
	// beside the documents built from them.
	mux.HandleFunc(meshapi.PathModels, func(w http.ResponseWriter, r *http.Request) {
		data := []meshapi.ModelEntry{}
		for _, m := range discovery.Models(src) {
			data = append(data, meshapi.ModelEntry{
				ID: m.Name, Object: "model", OwnedBy: m.Kind, Target: m.Target,
				MaxModelLen: m.ServedContext, ContextLength: m.ServedContext,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(meshapi.ModelsResponse{Object: "list", Data: data})
	})
	srv := &http.Server{Handler: mux}

	ln, err := net.Listen("tcp", "127.0.0.1:18087")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Logf("catalogue on http://127.0.0.1:18087%s", catalog.Path)
	t.Logf("model info on http://127.0.0.1:18087%s", modelinfo.Path)
	t.Logf("models     on http://127.0.0.1:18087%s", meshapi.PathModels)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if d := os.Getenv("CATALOGFIXTURE_FOR"); d != "" {
		dur, err := time.ParseDuration(d)
		if err != nil {
			t.Fatal(err)
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, dur)
		defer cancel()
	}
	<-ctx.Done()
	srv.Close()
}

type fixtureCatalogSource struct {
	entries []meshapi.ModelEntry
	fleet   []meshapi.FleetModel
}

func (f fixtureCatalogSource) ModelEntries() []meshapi.ModelEntry { return f.entries }
func (f fixtureCatalogSource) FleetModels() []meshapi.FleetModel  { return f.fleet }

// fixtureModel is one model served at ctx by each named host, four slots each.
func fixtureModel(name string, ctx int64, hosts ...string) meshapi.FleetModel {
	m := meshapi.FleetModel{Name: name, Engine: "llamacpp", Ctx: ctx}
	for _, h := range hosts {
		slots, c := 4, ctx
		m.Slots += slots
		m.Hosts = append(m.Hosts, meshapi.FleetHost{Node: h, Slots: &slots, Ctx: &c, AgeMS: 250})
	}
	return m
}
