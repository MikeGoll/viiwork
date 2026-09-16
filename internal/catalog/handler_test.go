package catalog

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

func oneModel() fakeSource {
	return fakeSource{
		entries: []meshapi.ModelEntry{entry("tg", meshapi.OwnedByLocal)},
		fleet:   []meshapi.FleetModel{{Name: "tg", Ctx: 32768}},
	}
}

// get serves one request through h and decodes the catalogue.
func get(t *testing.T, h http.Handler, target string) (*httptest.ResponseRecorder, Catalog) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	var got Catalog
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decoding %s: %v\n%s", target, err, rec.Body.String())
		}
	}
	return rec, got
}

// The endpoint answers the document OpenCode fetches.
func TestServesTheCatalogue(t *testing.T) {
	h := NewHandler(Config{ProviderID: "viiwork"}, oneModel())

	rec, got := get(t, h, "http://gb1:8086/api.json")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	if _, ok := got["viiwork"].Models["tg"]; !ok {
		t.Errorf("catalogue does not list tg: %+v", got)
	}
}

// Any node is an entry point, so the catalogue has to point back at the node
// the client actually reached. A hard-coded address would send every client to
// one machine and make the endpoint a liability when that machine is down.
func TestTheAdvertisedEndpointIsTheNodeTheClientReached(t *testing.T) {
	h := NewHandler(Config{ProviderID: "viiwork"}, oneModel())

	_, got := get(t, h, "http://yeti:8086/api.json")

	if api := got["viiwork"].API; api != "http://yeti:8086/v1" {
		t.Errorf("api = %q, want the host the request arrived on", api)
	}
}

// upstreamServer serves a catalogue of its own and counts fetches.
func upstreamServer(t *testing.T, body string, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api.json" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

const upstreamBody = `{"anthropic":{"id":"anthropic","name":"Anthropic","env":["ANTHROPIC_API_KEY"],` +
	`"npm":"@ai-sdk/anthropic","models":{"claude":{"id":"claude","name":"Claude",` +
	`"limit":{"context":200000,"output":64000}}}}}`

// Chaining: pointing OpenCode at a node replaces its whole catalogue, so
// without this a fleet user loses every hosted provider the moment they gain
// the mesh. With an upstream configured they keep both.
func TestUpstreamProvidersAreKept(t *testing.T) {
	var hits atomic.Int64
	up := upstreamServer(t, upstreamBody, &hits)
	h := NewHandler(Config{ProviderID: "viiwork", Upstream: up.URL, UpstreamTTL: time.Hour}, oneModel())

	_, got := get(t, h, "http://gb1:8086/api.json")

	if _, ok := got["anthropic"]; !ok {
		t.Errorf("upstream provider dropped: %v", keys(got))
	}
	if _, ok := got["viiwork"].Models["tg"]; !ok {
		t.Errorf("fleet provider dropped: %v", keys(got))
	}
}

// The fleet's own entry is the node's to define. An upstream that happened to
// publish the same provider id must not redefine where the models live.
func TestTheFleetEntryWinsOverUpstream(t *testing.T) {
	var hits atomic.Int64
	up := upstreamServer(t, `{"viiwork":{"id":"viiwork","name":"Someone else","env":[],"models":{}}}`, &hits)
	h := NewHandler(Config{ProviderID: "viiwork", Upstream: up.URL, UpstreamTTL: time.Hour}, oneModel())

	_, got := get(t, h, "http://gb1:8086/api.json")

	if _, ok := got["viiwork"].Models["tg"]; !ok {
		t.Errorf("upstream overwrote the node's own provider: %+v", got["viiwork"])
	}
}

// A node serves a fleet on a tailnet and may have no route to the internet at
// all. Losing the upstream must cost the hosted providers, never the fleet.
func TestAnUnreachableUpstreamStillServesTheFleet(t *testing.T) {
	h := NewHandler(Config{
		ProviderID:  "viiwork",
		Upstream:    "http://127.0.0.1:1", // nothing listens here
		UpstreamTTL: time.Hour,
	}, oneModel())

	rec, got := get(t, h, "http://gb1:8086/api.json")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: an upstream is an enrichment, not a dependency", rec.Code)
	}
	if _, ok := got["viiwork"].Models["tg"]; !ok {
		t.Errorf("fleet missing when upstream is down: %+v", got)
	}
}

// The upstream document is megabytes. Fetching it per request would make every
// client's five-minute refresh an outbound download from the node.
func TestUpstreamIsFetchedOncePerTTL(t *testing.T) {
	var hits atomic.Int64
	up := upstreamServer(t, upstreamBody, &hits)
	h := NewHandler(Config{ProviderID: "viiwork", Upstream: up.URL, UpstreamTTL: time.Hour}, oneModel())

	for range 3 {
		get(t, h, "http://gb1:8086/api.json")
	}

	if n := hits.Load(); n != 1 {
		t.Errorf("upstream fetched %d times, want 1", n)
	}
}

// No upstream configured is the default, and it must mean no outbound request.
func TestNoUpstreamMeansNoOutboundRequest(t *testing.T) {
	var hits atomic.Int64
	up := upstreamServer(t, upstreamBody, &hits)
	h := NewHandler(Config{ProviderID: "viiwork"}, oneModel())
	_ = up

	_, got := get(t, h, "http://gb1:8086/api.json")

	if n := hits.Load(); n != 0 {
		t.Errorf("upstream fetched %d times with none configured", n)
	}
	if len(got) != 1 {
		t.Errorf("catalogue has %d providers, want only the fleet's", len(got))
	}
}

func keys(c Catalog) []string {
	out := make([]string, 0, len(c))
	for k := range c {
		out = append(out, k)
	}
	return out
}
