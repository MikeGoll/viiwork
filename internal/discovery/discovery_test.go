package discovery

// The record is a pure join of the two lists a node already publishes, so
// every rule here is testable without a mesh, a GPU or a client.

import (
	"testing"

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

// host is one fresh host contributing slots and a window.
func host(node string, slots int, ctx int64, ageMS int64) meshapi.FleetHost {
	return meshapi.FleetHost{Node: node, Slots: &slots, Ctx: &ctx, AgeMS: ageMS}
}

func byName(models []Model, name string) (Model, bool) {
	for _, m := range models {
		if m.Name == name {
			return m, true
		}
	}
	return Model{}, false
}

// /v1/models is the list of what a client may name, and the record is what
// every serializer renders. If the two diverge a model becomes callable but
// undiscoverable, or discoverable but uncallable — one list, many renderings.
func TestRecordCoversEveryModelTheNodeLists(t *testing.T) {
	src := fakeSource{entries: []meshapi.ModelEntry{
		entry("tg", meshapi.OwnedByLocal),
		entry("dsv4", meshapi.OwnedByPeer),
		entry("summarise", meshapi.OwnedByPipeline),
	}}

	got := Models(src)

	if len(got) != 3 {
		t.Fatalf("got %d models, want 3: %+v", len(got), got)
	}
	for _, name := range []string{"dsv4", "summarise", "tg"} {
		if _, ok := byName(got, name); !ok {
			t.Errorf("model %q missing: %+v", name, got)
		}
	}
}

// Ordered, so that a consumer diffing two reads sees real changes rather than
// map iteration order — the same reason /v1/fleet/capacity sorts.
func TestRecordIsOrderedByName(t *testing.T) {
	src := fakeSource{entries: []meshapi.ModelEntry{
		entry("tg", meshapi.OwnedByLocal),
		entry("dsv4", meshapi.OwnedByLocal),
		entry("alpha", meshapi.OwnedByLocal),
	}}

	got := Models(src)

	for i := 1; i < len(got); i++ {
		if got[i-1].Name > got[i].Name {
			t.Fatalf("not ordered: %q before %q", got[i-1].Name, got[i].Name)
		}
	}
}

// ServedContext is the number the whole feature exists to publish: the total
// prompt+completion budget per slot the fleet GUARANTEES, which is the minimum
// across the hosts the router would actually route to.
func TestServedContextComesFromTheFleetView(t *testing.T) {
	src := fakeSource{
		entries: []meshapi.ModelEntry{entry("tg", meshapi.OwnedByLocal)},
		fleet:   []meshapi.FleetModel{{Name: "tg", Ctx: 98304, Engine: "llamacpp"}},
	}

	m, _ := byName(Models(src), "tg")

	if m.ServedContext != 98304 {
		t.Errorf("ServedContext = %d, want 98304", m.ServedContext)
	}
	if m.Engine != "llamacpp" {
		t.Errorf("Engine = %q, want llamacpp", m.Engine)
	}
}

// Absent is not zero. A pipeline is node-local and never appears in a capacity
// report, so nothing can size it — and the record says so rather than
// inventing a figure. Supplying one is a serializer's job, and only for a
// format that cannot express "unknown".
func TestAModelNothingCanSizeCarriesNoContext(t *testing.T) {
	src := fakeSource{entries: []meshapi.ModelEntry{entry("summarise", meshapi.OwnedByPipeline)}}

	m, _ := byName(Models(src), "summarise")

	if m.ServedContext != 0 {
		t.Errorf("ServedContext = %d, want 0 — nothing can say", m.ServedContext)
	}
	if m.Kind != meshapi.OwnedByPipeline {
		t.Errorf("Kind = %q, want pipeline", m.Kind)
	}
}

// An alias is a name for another model's capacity, so it has to inherit that
// model's numbers. Aliases are the mesh-wide stable names operators are told
// to use, so getting this wrong mis-sizes precisely the recommended path.
func TestAnAliasInheritsItsTargetsNumbers(t *testing.T) {
	src := fakeSource{
		entries: []meshapi.ModelEntry{
			entry("dsv4", meshapi.OwnedByLocal),
			{ID: "big", Object: "model", OwnedBy: meshapi.OwnedByAlias, Target: "dsv4"},
		},
		fleet: []meshapi.FleetModel{{
			Name: "dsv4", Ctx: 98304, Engine: "llamacpp",
			Hosts: []meshapi.FleetHost{host("gb1", 4, 98304, 200)},
		}},
	}

	m, _ := byName(Models(src), "big")

	if m.ServedContext != 98304 {
		t.Errorf("ServedContext = %d, want its target's 98304", m.ServedContext)
	}
	if m.Target != "dsv4" {
		t.Errorf("Target = %q, want dsv4", m.Target)
	}
	if m.Engine != "llamacpp" {
		t.Errorf("Engine = %q, want its target's", m.Engine)
	}
	if m.Hosts != 1 {
		t.Errorf("Hosts = %d, want its target's 1", m.Hosts)
	}
}

// An alias whose target nothing serves has no numbers to inherit. That is a
// normal operating state, not an error: the record carries the name and says
// nothing about size, and every serializer decides for itself what to do.
func TestAnAliasWithNoServedTargetCarriesNoContext(t *testing.T) {
	src := fakeSource{entries: []meshapi.ModelEntry{
		{ID: "big", Object: "model", OwnedBy: meshapi.OwnedByAlias, Target: "gone"},
	}}

	m, ok := byName(Models(src), "big")

	if !ok {
		t.Fatal("alias dropped from the record; it is still a name a client may type")
	}
	if m.ServedContext != 0 || m.Hosts != 0 {
		t.Errorf("ServedContext = %d, Hosts = %d, want 0 and 0", m.ServedContext, m.Hosts)
	}
}

// Provenance is the honest half of the record: how many hosts stand behind
// that window, and how old the oldest of their reports is. A client that
// wants to know whether a number is worth trusting has no other way to ask.
func TestProvenanceCountsTheHostsBehindTheWindow(t *testing.T) {
	src := fakeSource{
		entries: []meshapi.ModelEntry{entry("tg", meshapi.OwnedByLocal)},
		fleet: []meshapi.FleetModel{{Name: "tg", Ctx: 32768, Hosts: []meshapi.FleetHost{
			host("gb1", 4, 98304, 120),
			host("gb2", 2, 32768, 900),
			{Node: "gb3", AgeMS: 9000, Stale: true}, // no numbers to assert
		}}},
	}

	m, _ := byName(Models(src), "tg")

	if m.Hosts != 2 {
		t.Errorf("Hosts = %d, want 2 — the stale host asserts nothing", m.Hosts)
	}
	if m.OldestAgeMS != 900 {
		t.Errorf("OldestAgeMS = %d, want 900 — the oldest report actually counted", m.OldestAgeMS)
	}
}

// A host reporting no slots contributes no capacity the router will ever use,
// so it must not stand behind the advertised window either. Counting it would
// let a draining backend depress the figure for the hosts still serving.
func TestAHostWithNoSlotsIsNotProvenance(t *testing.T) {
	src := fakeSource{
		entries: []meshapi.ModelEntry{entry("tg", meshapi.OwnedByLocal)},
		fleet: []meshapi.FleetModel{{Name: "tg", Ctx: 98304, Hosts: []meshapi.FleetHost{
			host("gb1", 4, 98304, 120),
			host("gb2", 0, 4096, 130), // draining
		}}},
	}

	m, _ := byName(Models(src), "tg")

	if m.Hosts != 1 {
		t.Errorf("Hosts = %d, want 1 — a slotless host takes no work", m.Hosts)
	}
}

// Every model viiwork serves is reached through one OpenAI-compatible endpoint
// that passes tools straight through, so the flag is a property of the seam
// rather than of the checkpoint. Pinned because a client reads it to decide
// whether a model may be an agent at all, which is the only reason to point a
// coding client at a fleet.
func TestModelsAdvertiseToolCalling(t *testing.T) {
	src := fakeSource{entries: []meshapi.ModelEntry{entry("tg", meshapi.OwnedByLocal)}}

	m, _ := byName(Models(src), "tg")

	if !m.Tools {
		t.Error("Tools is false: no client would run this model as an agent")
	}
}
