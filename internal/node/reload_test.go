package node

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/mesh/meshtest"
)

func TestReload(t *testing.T) {
	m := fakeModel{name: "m"}
	n := fakeModel{name: "n"}
	tn := startNode(t, meshtest.NewNetwork(), "n1", []fakeModel{m}, "", nil)
	until(t, 10*time.Second, "m healthy", func() bool { return healthyModel(tn.status(t), "m") })
	mPIDs := pidsOf(tn.status(t), "m")

	// RL1: add n.
	writeFile(t, tn.cfgPath, nodeConfig("n1", tn.stateDir, []fakeModel{m, n}, ""))
	if err := tn.Reload(); err != nil {
		t.Fatalf("RL1: %v", err)
	}
	until(t, 10*time.Second, "n healthy", func() bool { return healthyModel(tn.status(t), "n") })
	if got := pidsOf(tn.status(t), "m"); !reflect.DeepEqual(got, mPIDs) {
		t.Errorf("RL1: m restarted: %v -> %v", mPIDs, got)
	}
	nPIDs := pidsOf(tn.status(t), "n")

	// RL2: change m's args.
	m.args = []string{"--seed", "1"}
	writeFile(t, tn.cfgPath, nodeConfig("n1", tn.stateDir, []fakeModel{m, n}, ""))
	if err := tn.Reload(); err != nil {
		t.Fatalf("RL2: %v", err)
	}
	until(t, 15*time.Second, "m restarted healthy", func() bool {
		st := tn.status(t)
		return healthyModel(st, "m") && !reflect.DeepEqual(pidsOf(st, "m"), mPIDs)
	})
	if got := pidsOf(tn.status(t), "n"); !reflect.DeepEqual(got, nPIDs) {
		t.Errorf("RL2: n restarted: %v -> %v", nPIDs, got)
	}

	// RL3: invalid YAML keeps everything running.
	writeFile(t, tn.cfgPath, "models: [unclosed\n")
	if err := tn.Reload(); err == nil {
		t.Error("RL3: an invalid file must return an error")
	}
	if st := tn.status(t); !healthyModel(st, "m") || !healthyModel(st, "n") {
		t.Errorf("RL3: models stopped after a failed reload: %+v", st.Models)
	}
	if !strings.Contains(tn.log.String(), "keeping the running configuration") {
		t.Error("RL3: no log line for the failed reload")
	}

	// RL4: a changed api.port needs a restart and restarts nothing.
	before := tn.status(t)
	writeFile(t, tn.cfgPath, strings.Replace(nodeConfig("n1", tn.stateDir, []fakeModel{m, n}, ""), "port: 18086", "port: 18087", 1))
	if err := tn.Reload(); err != nil {
		t.Fatalf("RL4: %v", err)
	}
	if !strings.Contains(tn.log.String(), "changes to API need a restart") {
		t.Errorf("RL4: log = %q", tn.log.String())
	}
	after := tn.status(t)
	if !reflect.DeepEqual(pidsOf(after, "m"), pidsOf(before, "m")) || !reflect.DeepEqual(pidsOf(after, "n"), pidsOf(before, "n")) {
		t.Error("RL4: a restart-only change restarted a model")
	}

	// RL5: pipelines changed and n removed.
	extra := "pipelines:\n  tr:\n    locales:\n      fi:\n        language: Finnish\n    steps: []\n"
	writeFile(t, tn.cfgPath, nodeConfig("n1", tn.stateDir, []fakeModel{m}, extra))
	if err := tn.Reload(); err != nil {
		t.Fatalf("RL5: %v", err)
	}
	until(t, 10*time.Second, "n removed", func() bool { _, ok := modelOf(tn.status(t), "n"); return !ok })
	if !strings.Contains(tn.log.String(), "Pipelines") {
		t.Errorf("RL5: the log does not name Pipelines: %q", tn.log.String())
	}
}

func TestReloadClients(t *testing.T) {
	addr := netip.MustParseAddr("100.64.0.10")
	clients := func(name string) string { return "clients:\n  " + name + ": [100.64.0.10]\n" }
	tn := startNode(t, meshtest.NewNetwork(), "n1", nil, clients("factory-a"), nil)
	if got := tn.clients.Load().Lookup(addr); got != "factory-a" {
		t.Fatalf("at start: %q", got)
	}

	// RC1: without clients_reload a changed section waits for a restart.
	writeFile(t, tn.cfgPath, nodeConfig("n1", tn.stateDir, nil, clients("batch-b")))
	if err := tn.Reload(); err != nil {
		t.Fatalf("RC1: %v", err)
	}
	if got := tn.clients.Load().Lookup(addr); got != "factory-a" {
		t.Errorf("RC1: the section was applied without clients_reload: %q", got)
	}
	if !strings.Contains(tn.log.String(), "changes to Clients need a restart") {
		t.Errorf("RC1: log = %q", tn.log.String())
	}

	// RC2: with it, in the same file, the table is swapped.
	writeFile(t, tn.cfgPath, nodeConfig("n1", tn.stateDir, nil, clients("batch-b")+"clients_reload: true\n"))
	if err := tn.Reload(); err != nil {
		t.Fatalf("RC2: %v", err)
	}
	if got := tn.clients.Load().Lookup(addr); got != "batch-b" {
		t.Errorf("RC2: %q", got)
	}
	if !strings.Contains(tn.log.String(), "reload: clients applied (1 named)") {
		t.Errorf("RC2: log = %q", tn.log.String())
	}

	// RC3: a file that fails validation changes nothing.
	writeFile(t, tn.cfgPath, nodeConfig("n1", tn.stateDir, nil, "clients:\n  other: [100.64.0.10]\nclients_reload: true\n"))
	if err := tn.Reload(); err == nil {
		t.Error("RC3: a reserved name must fail the reload")
	}
	if got := tn.clients.Load().Lookup(addr); got != "batch-b" {
		t.Errorf("RC3: a bad file changed the table: %q", got)
	}

	// RC4: an unchanged section is not applied or logged again.
	before := strings.Count(tn.log.String(), "clients applied")
	writeFile(t, tn.cfgPath, nodeConfig("n1", tn.stateDir, nil, clients("batch-b")+"clients_reload: true\n"))
	if err := tn.Reload(); err != nil {
		t.Fatalf("RC4: %v", err)
	}
	if strings.Count(tn.log.String(), "clients applied") != before {
		t.Error("RC4: an unchanged section was applied again")
	}

	// RC5: removing the section, with clients_reload, stops the labelling.
	writeFile(t, tn.cfgPath, nodeConfig("n1", tn.stateDir, nil, "clients_reload: true\n"))
	if err := tn.Reload(); err != nil {
		t.Fatalf("RC5: %v", err)
	}
	if tn.clients.Load() != nil {
		t.Error("RC5: the table outlived its section")
	}
}
