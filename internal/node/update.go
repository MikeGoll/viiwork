package node

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/release"
	"github.com/janit/viiwork/v2/internal/update"
)

// ErrRestart is Run's return when an update asked for a restart: the node
// has done its ordered shutdown, and main now execs the launcher.
var ErrRestart = errors.New("restart requested by an update")

// confirmEvery is how often a pending release checks its baseline.
const confirmEvery = 5 * time.Second

// RequestRestart makes Run shut down in order and return ErrRestart. Only the
// first call counts.
func (n *Node) RequestRestart() { n.restartOnce.Do(func() { close(n.restart) }) }

// buildUpdate builds /v1/update and, on a node that takes part in updates,
// the confirmer for a pending release.
func (n *Node) buildUpdate(cfg *config.Config, auth update.Authorizer) (*update.Service, *update.Confirmer, error) {
	dir := update.ReleasesDir(cfg.Node.StateDir)
	keys := n.o.ReleaseKeys
	if keys == nil {
		var err error
		if keys, err = release.Keys(); err != nil {
			return nil, nil, err
		}
	}
	models := func() []config.Model { return n.runningConfig().Models }
	var engines struct {
		once sync.Once
		v    map[string]string
	}
	// One store for the API and the confirmer: every read-modify-write of
	// state.json in this process goes through its lock.
	store := update.NewStore(dir)
	stager := &update.Stager{
		Dir: dir, Source: cfg.Update.Source, Client: &http.Client{}, Keys: keys,
		Target: release.Target{OS: runtime.GOOS, Arch: runtime.GOARCH},
		Engines: func(ctx context.Context, required map[string]string) error {
			return update.CheckEngines(ctx, models(), required)
		},
	}
	if n.o.Managed {
		// The release is the image: the host's helper pulls and verifies it
		// at stage, so activation never waits on a pull, and the engine it
		// carries is what the requirements are checked against.
		helper := update.ImageHelper{Dir: dir, Wait: n.o.ImageHelperWait}
		stager.PrepareImage = func(ctx context.Context, version string, required map[string]string) error {
			engines, err := helper.Prepare(ctx, version)
			if err != nil {
				return err
			}
			return update.CheckEngineVersions(models(), required, engines)
		}
	}
	svc := &update.Service{
		Enabled: cfg.Update.Enabled, Running: n.o.Version, Store: store, Auth: auth,
		Stager:  stager,
		Managed: n.o.Managed, Stopping: n.stopping,
		Backends: n.sup.Status,
		// Read once: running every engine's --version on each GET would cost
		// a process per engine per poll.
		Engines: func() map[string]string {
			engines.once.Do(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				engines.v = update.InstalledEngines(ctx, n.llama.follow(models(), n.llama.pin))
			})
			return engines.v
		},
		Restart: n.RequestRestart,
		Log:     n.logf,
	}
	if n.llama.root != "" {
		svc.Stager.PrepareLlama = n.prepareLlama(n.llamaFetch(), models)
	}
	var c *update.Confirmer
	if cfg.Update.Enabled {
		c = &update.Confirmer{
			Store: store, Running: n.o.Version, Window: cfg.ConfirmWindow(),
			Backends: n.sup.Status, Restart: n.RequestRestart, Now: time.Now, Log: n.logf,
			Managed: n.o.Managed,
		}
		svc.Deadline = c.Deadline
		if n.llama.root != "" {
			c.PruneEngines = n.pruneLlama(dir)
		}
	}
	return svc, c, nil
}
