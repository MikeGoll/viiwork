package node

import (
	"reflect"
	"strings"

	"github.com/janit/viiwork/v2/internal/clientuse"
	"github.com/janit/viiwork/v2/internal/config"
)

// Reload re-reads the config file (SIGHUP). Models are applied, and so is the
// clients section when the file sets clients_reload; any other section that
// changed is named in one log line and waits for a restart, because its
// values were captured at start (P6 Decision 6). A file that does not load
// changes nothing. Safe to call while Run is serving.
func (n *Node) Reload() error {
	next, err := config.Load(n.o.ConfigPath, n.o.LookupEnv)
	if err != nil {
		n.logf("reload: %v; keeping the running configuration", err)
		return err
	}
	current := n.runningConfig()

	sections := []struct {
		name     string
		old, new any
	}{
		{"Node", current.Node, next.Node},
		{"API", current.API, next.API},
		{"Mesh", current.Mesh, next.Mesh},
		{"Routing", current.Routing, next.Routing},
		{"GPU", current.GPU, next.GPU},
		{"Health", current.Health, next.Health},
		{"Activity", current.Activity, next.Activity},
		{"Power", current.Power, next.Power},
		{"Energy", current.Energy, next.Energy},
		{"Cost", current.Cost, next.Cost},
		{"ViiworkParrot", current.ViiworkParrot, next.ViiworkParrot},
		{"Update", current.Update, next.Update},
		{"Pipelines", current.Pipelines, next.Pipelines},
	}
	var restart []string
	for _, s := range sections {
		if !reflect.DeepEqual(s.old, s.new) {
			restart = append(restart, s.name)
		}
	}
	// The clients section is the one exception to "everything but models
	// waits for a restart", and only when the file being loaded asks for it:
	// restarting an entry node interrupts every client of the mesh.
	clientsChanged := !reflect.DeepEqual(current.Clients, next.Clients)
	if clientsChanged && !next.ClientsReload {
		restart = append(restart, "Clients")
	}
	if len(restart) > 0 {
		n.logf("reload: changes to %s need a restart and were not applied", strings.Join(restart, ", "))
	}
	if clientsChanged && next.ClientsReload {
		n.applyClients(next)
	}

	// A parked model stays parked while it is configured, its new
	// configuration included; one the file no longer has leaves the set.
	n.applyMu.Lock()
	defer n.applyMu.Unlock()
	if reflect.DeepEqual(current.Models, next.Models) {
		// The models did not change, but a file one of them names may have
		// (an engine's PerfKey reads it): the keys are read again, so a
		// baseline measured under the old file is dropped.
		n.perf.SetKeys(perfKeys(next.Models))
		n.sessions.SetModels(sessionModels(next.Models))
		return nil
	}
	if n.modelsApplied {
		if err := n.applyUnparkedLocked(next.Models); err != nil {
			n.logf("reload: applying models: %v", err)
			return err
		}
	}
	n.pruneParkedLocked(next.Models)
	n.perf.SetKeys(perfKeys(next.Models))
	n.sessions.SetModels(sessionModels(next.Models))
	n.mu.Lock()
	updated := *n.cfg
	updated.Models = next.Models
	n.cfg = &updated
	n.mu.Unlock()
	n.logf("reload: models applied (%d configured)", len(next.Models))
	return nil
}

// applyClients swaps the clients table whole. next has passed validation, so
// its section parses. Requests already running keep the name they were given.
func (n *Node) applyClients(next *config.Config) {
	table, err := clientuse.ParseTable(next.Clients)
	if err != nil {
		n.logf("reload: clients: %v; keeping the running table", err)
		return
	}
	n.clients.Store(table)
	if table != nil {
		n.ledger.Activate() // nil-safe: a node with no store has no ledger
	}
	n.mu.Lock()
	updated := *n.cfg
	updated.Clients, updated.ClientsReload = next.Clients, next.ClientsReload
	n.cfg = &updated
	n.mu.Unlock()
	n.logf("reload: clients applied (%d named)", table.Names())
}
