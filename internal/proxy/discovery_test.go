package proxy

// Contract tests for the number the whole autodiscovery feature publishes: the
// context window a client may size a prompt against.
//
// They run the real path — capacity reports through aggregateFleet into
// discovery.Models — rather than hand-built records, because every bug worth
// catching here lives in the join rather than in either end. Time is explicit,
// so a draining host and a stale one are distinguishable states rather than a
// race.
//
// The promise under test, in one sentence: ServedContext is a window every
// host that could receive the request will honour. Each case below is a way
// that can quietly stop being true.

import (
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/discovery"
	"github.com/janit/viiwork/v2/mesh/capacity"
	"github.com/janit/viiwork/v2/meshapi"
)

// staleAfter is routing.stale_after's default, the same predicate the router
// applies. fleetNow comes from fleet_test.go.
const discoveryStaleAfter = 3 * time.Second

type discoverySource struct {
	entries []meshapi.ModelEntry
	fleet   []meshapi.FleetModel
}

func (s discoverySource) ModelEntries() []meshapi.ModelEntry { return s.entries }
func (s discoverySource) FleetModels() []meshapi.FleetModel  { return s.fleet }

// discover runs the real aggregation and then the record over it.
func discover(entries []meshapi.ModelEntry, own []meshapi.ModelCapacity, reports ...capacity.Report) []discovery.Model {
	fleet := aggregateFleet("gb1", own, reports, fleetNow, discoveryStaleAfter)
	return discovery.Models(discoverySource{entries: entries, fleet: fleet.Models})
}

// localEntry is a model this node serves, as /v1/models lists it.
func localEntry(id string) meshapi.ModelEntry {
	return meshapi.ModelEntry{ID: id, Object: "model", OwnedBy: meshapi.OwnedByLocal}
}

func found(t *testing.T, models []discovery.Model, name string) discovery.Model {
	t.Helper()
	for _, m := range models {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("model %q not in record %+v", name, models)
	return discovery.Model{}
}

// Nothing stops an operator loading one model at different contexts on
// different machines, and the router will route to either. The advertised
// window must therefore be the smallest, or a prompt sized against it fails
// on whichever host happens to take it.
func TestDiscoveryTakesTheSmallestWindowAcrossHosts(t *testing.T) {
	got := discover(
		[]meshapi.ModelEntry{localEntry("tg")},
		[]meshapi.ModelCapacity{mc("tg", 4, 0, 0, 98304)},
		rep("gb2", "10.0.0.2:8086", time.Millisecond, mc("tg", 4, 0, 0, 32768)),
		rep("gb3", "10.0.0.3:8086", time.Millisecond, mc("tg", 4, 0, 0, 65536)),
	)

	m := found(t, got, "tg")
	if m.ServedContext != 32768 {
		t.Errorf("ServedContext = %d, want the 32768 floor", m.ServedContext)
	}
	if m.Hosts != 3 {
		t.Errorf("Hosts = %d, want 3", m.Hosts)
	}
}

// A host we have lost sight of asserts nothing. Its window must not set the
// floor — we do not currently stand behind it — and it must not be counted as
// provenance, or a number would look better supported than it is.
func TestDiscoveryIgnoresAStaleHost(t *testing.T) {
	got := discover(
		[]meshapi.ModelEntry{localEntry("tg")},
		[]meshapi.ModelCapacity{mc("tg", 4, 0, 0, 98304)},
		rep("gb2", "10.0.0.2:8086", 9*time.Second, mc("tg", 4, 0, 0, 512)),
	)

	m := found(t, got, "tg")
	if m.ServedContext != 98304 {
		t.Errorf("ServedContext = %d, want 98304 — a stale host sets no floor", m.ServedContext)
	}
	if m.Hosts != 1 {
		t.Errorf("Hosts = %d, want 1", m.Hosts)
	}
}

// A draining host still reports, freshly, and still lists the model — but it
// has no slots, so no request will ever reach it. Its window is not one the
// fleet is offering, and it is the smallest windows that drain first.
func TestDiscoveryIgnoresADrainingHost(t *testing.T) {
	got := discover(
		[]meshapi.ModelEntry{localEntry("tg")},
		[]meshapi.ModelCapacity{mc("tg", 4, 0, 0, 98304)},
		rep("gb2", "10.0.0.2:8086", time.Millisecond, mc("tg", 0, 0, 0, 512)),
	)

	m := found(t, got, "tg")
	if m.ServedContext != 98304 {
		t.Errorf("ServedContext = %d, want 98304 — a draining host sets no floor", m.ServedContext)
	}
	if m.Hosts != 1 {
		t.Errorf("Hosts = %d, want 1 — only the host that can take work", m.Hosts)
	}
}

// A backend restart walks the model through "no slots anywhere" and out the
// other side. While it is down the fleet can promise no window, and says so;
// when it comes back the number returns on its own. Nothing latches, because
// the record is derived from the current reports rather than remembered.
func TestDiscoveryFollowsABackendThroughARestart(t *testing.T) {
	entries := []meshapi.ModelEntry{localEntry("tg")}

	before := found(t, discover(entries, []meshapi.ModelCapacity{mc("tg", 4, 0, 0, 32768)}), "tg")
	if before.ServedContext != 32768 || before.Hosts != 1 {
		t.Fatalf("before restart: %+v", before)
	}

	// Mid-restart: the supervisor still lists the model, with no slots.
	during := found(t, discover(entries, []meshapi.ModelCapacity{mc("tg", 0, 0, 0, 32768)}), "tg")
	if during.ServedContext != 0 {
		t.Errorf("during restart: ServedContext = %d, want 0 — nothing can serve it", during.ServedContext)
	}
	if during.Hosts != 0 {
		t.Errorf("during restart: Hosts = %d, want 0", during.Hosts)
	}
	// It stays NAMED throughout: a model that vanished from the list would
	// make every client rebuild its picker twice per restart.
	if during.Name != "tg" || during.Kind != meshapi.OwnedByLocal {
		t.Errorf("during restart the model left the list: %+v", during)
	}

	after := found(t, discover(entries, []meshapi.ModelCapacity{mc("tg", 4, 0, 0, 32768)}), "tg")
	if after.ServedContext != 32768 || after.Hosts != 1 {
		t.Errorf("after restart: %+v, want the window back", after)
	}
}

// A restart on one host of several must not take the window down with it: the
// remaining host can still serve, so the floor becomes its window.
func TestDiscoveryKeepsAWindowWhileOneOfTwoHostsRestarts(t *testing.T) {
	got := discover(
		[]meshapi.ModelEntry{localEntry("tg")},
		[]meshapi.ModelCapacity{mc("tg", 0, 0, 0, 32768)}, // restarting
		rep("gb2", "10.0.0.2:8086", time.Millisecond, mc("tg", 4, 0, 0, 98304)),
	)

	m := found(t, got, "tg")
	if m.ServedContext != 98304 {
		t.Errorf("ServedContext = %d, want the surviving host's 98304", m.ServedContext)
	}
	if m.Hosts != 1 {
		t.Errorf("Hosts = %d, want 1", m.Hosts)
	}
}

// The ordinary case, and the one a single-machine fleet lives in: one host,
// one window, and provenance that says so.
func TestDiscoveryWithASingleHost(t *testing.T) {
	got := discover(
		[]meshapi.ModelEntry{localEntry("tg")},
		nil,
		rep("gb2", "10.0.0.2:8086", 900*time.Millisecond, mc("tg", 2, 1, 0, 16384)),
	)

	m := found(t, got, "tg")
	if m.ServedContext != 16384 {
		t.Errorf("ServedContext = %d, want 16384", m.ServedContext)
	}
	if m.Hosts != 1 {
		t.Errorf("Hosts = %d, want 1", m.Hosts)
	}
	if m.OldestAgeMS != 900 {
		t.Errorf("OldestAgeMS = %d, want 900", m.OldestAgeMS)
	}
	if m.Engine != "llamacpp" {
		t.Errorf("Engine = %q", m.Engine)
	}
}

// An alias whose target nobody serves is a normal operating state, not an
// error: the alias table is replicated to every node, so a target can be
// configured before it is loaded and after it is unloaded. The name stays
// listed — a client may still type it, and the answer is a 503 with a
// Retry-After, decided on the inference path rather than here — and it claims
// no window, because there is none to claim.
func TestDiscoveryKeepsAnAliasWhoseTargetIsServedNowhere(t *testing.T) {
	got := discover(
		[]meshapi.ModelEntry{
			localEntry("tg"),
			{ID: "big", Object: "model", OwnedBy: meshapi.OwnedByAlias, Target: "gone"},
		},
		[]meshapi.ModelCapacity{mc("tg", 4, 0, 0, 32768)},
	)

	m := found(t, got, "big")
	if m.Target != "gone" {
		t.Errorf("Target = %q, want gone", m.Target)
	}
	if m.ServedContext != 0 || m.Hosts != 0 {
		t.Errorf("ServedContext = %d, Hosts = %d, want 0 and 0", m.ServedContext, m.Hosts)
	}
	// And it must not have quietly picked up some other model's numbers.
	if tg := found(t, got, "tg"); tg.ServedContext != 32768 {
		t.Errorf("the served model was disturbed: %+v", tg)
	}
}

// An alias to a target that IS served carries that target's floor, including
// when the floor comes from a different host than the one this node runs.
func TestDiscoveryAliasFollowsItsTargetsFloor(t *testing.T) {
	got := discover(
		[]meshapi.ModelEntry{
			localEntry("tg"),
			{ID: "big", Object: "model", OwnedBy: meshapi.OwnedByAlias, Target: "tg"},
		},
		[]meshapi.ModelCapacity{mc("tg", 4, 0, 0, 98304)},
		rep("gb2", "10.0.0.2:8086", time.Millisecond, mc("tg", 4, 0, 0, 8192)),
	)

	if m := found(t, got, "big"); m.ServedContext != 8192 {
		t.Errorf("alias ServedContext = %d, want its target's 8192 floor", m.ServedContext)
	}
}
