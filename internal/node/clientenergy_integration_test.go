//go:build integration

package node

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/energy"
	"github.com/janit/viiwork/v2/mesh/meshtest"
	"github.com/janit/viiwork/v2/meshapi"
)

// clientEnergyMesh starts B, which serves "m" on card 0 and keeps an energy
// store, and A, an entry node with the given top-level YAML. It sends one
// chat through A, stops B so that the recorder flushes its minute, and
// returns B's store directory. beforeStop, if given, runs with both nodes
// still up.
func clientEnergyMesh(t *testing.T, entryExtra string, beforeStop ...func(a, b *testNode)) string {
	t.Helper()
	net := meshtest.NewNetwork()
	dir := filepath.Join(t.TempDir(), "energy")
	bExtra := fmt.Sprintf("energy:\n  enabled: true\n  dir: %q\n  sample_interval: 100ms\n", dir)
	// The fake engines draw nothing a tool can read, so node power is given.
	withPower := func(o *Options) { o.Power = &fakePower{available: true, source: "nvidia-smi", watts: 200} }
	model := fakeModel{name: "m"}.yaml() + "    gpus: [0]\n"
	b := startNodeOpts(t, net, "B", []string{model}, bExtra, seeded(net, "B"), withPower)
	a := startNode(t, net, "A", nil, entryExtra, seeded(net, "A", b))
	until(t, 10*time.Second, "A and B joined", allAlive(a, b))
	until(t, 10*time.Second, "m healthy on B", func() bool { return healthyModel(b.status(t), "m") })
	until(t, 10*time.Second, "A learns m", func() bool { return modelsOf(t, a)["m"] == meshapi.OwnedByPeer })

	// The test client reaches A from 127.0.0.1.
	r := chat(t, a, "m")
	if r.status != 200 || r.header.Get(meshapi.HeaderNode) != "B" {
		t.Fatalf("%d %v %s", r.status, r.header, r.body)
	}
	time.Sleep(300 * time.Millisecond) // at least one more power sample

	for _, f := range beforeStop {
		f(a, b)
	}

	// Stop B: the recorder finalises the minute in progress on the way out.
	b.cancel()
	select {
	case err := <-b.done:
		b.done <- err // startNodeOpts' cleanup reads it again
	case <-time.After(30 * time.Second):
		t.Fatal("B did not stop")
	}
	return dir
}

func TestClientEnergyAcrossAForward(t *testing.T) {
	dir := clientEnergyMesh(t, "clients:\n  factory-a: [127.0.0.1]\n")
	store, err := energy.Open(energy.Config{Dir: dir, GPUIDs: []int{0}, Location: time.UTC, Clients: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	found := false
	for _, r := range store.ReadClients(energy.TierMinute, now.Add(-5*time.Minute), now.Add(time.Minute)) {
		if store.ClientName(r.ClientIdx) == "factory-a" && store.ModelName(r.ModelIdx) == "m" {
			found = true
		}
	}
	if !found {
		raw, _ := os.ReadFile(filepath.Join(dir, "clients.txt"))
		t.Errorf("B's store has no record of factory-a on m; clients.txt = %q", raw)
	}
	ce, ok := store.ClientEnergy()
	if !ok || len(ce.Clients) != 1 || ce.Clients[0].Name != "factory-a" {
		t.Errorf("client energy = %+v, %v", ce, ok)
	}
}

// Off: the same mesh with no clients section anywhere records the minute and
// creates no client file.
func TestNoClientsSectionCreatesNoClientFile(t *testing.T) {
	// Both nodes' raw status bodies, while they are up: the key is absent,
	// not merely empty.
	dir := clientEnergyMesh(t, "", func(a, b *testNode) {
		for name, tn := range map[string]*testNode{"A": a, "B": b} {
			resp, err := http.Get(tn.url(meshapi.PathStatus))
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if len(body) == 0 || bytes.Contains(body, []byte("client_energy")) {
				t.Errorf("%s: /v1/status holds client_energy or is empty: %s", name, body)
			}
		}
	})
	// node-minute.ring is preallocated by Open, so its existence proves
	// nothing: look for a node record that holds a reading.
	store, err := energy.Open(energy.Config{Dir: dir, GPUIDs: []int{0}, Location: time.UTC}, nil)
	if err != nil {
		t.Fatalf("the store cannot be opened: %v", err)
	}
	now := time.Now()
	written := false
	for _, r := range store.ReadNode(energy.TierMinute, now.Add(-5*time.Minute), now.Add(time.Minute)) {
		if r.Watts > 0 {
			written = true
		}
	}
	store.Close()
	if !written {
		t.Fatal("the store was not written: no node record with a reading")
	}
	for _, name := range []string{"clients.txt", "client-minute.ring", "client-hour.ring", "client-totals"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s exists on a mesh where nobody named a client", name)
		}
	}
}
