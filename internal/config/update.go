package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/janit/viiwork/v2/internal/engine"
	"gopkg.in/yaml.v3"
)

// PeekUpdate reads just enough of a config file to hand over to a staged
// release: node.state_dir (the default when absent) and update.enabled. It is
// deliberately lenient — the launcher is older than the release it hands over
// to, and the config may already carry keys only that release knows; strict
// validation belongs to the binary that actually runs.
func PeekUpdate(path string) (stateDir string, enabled bool, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	var peek struct {
		Node struct {
			StateDir string `yaml:"state_dir"`
		} `yaml:"node"`
		Update struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"update"`
	}
	if err := yaml.Unmarshal(b, &peek); err != nil {
		return "", false, err
	}
	stateDir = peek.Node.StateDir
	if stateDir == "" {
		stateDir = Defaults().Node.StateDir
	}
	return stateDir, peek.Update.Enabled, nil
}

// DefaultUpdateSource is where signed releases are downloaded from.
const DefaultUpdateSource = "https://github.com/janit/viiwork/releases/download"

// UpdateRepo is the GitHub owner/name DefaultUpdateSource names: how a
// release is identified to viiwork-parrot.
func UpdateRepo() string {
	rest := strings.TrimPrefix(DefaultUpdateSource, "https://github.com/")
	return strings.TrimSuffix(rest, "/releases/download")
}

// UpdateConfig is how this node takes part in rolling updates. Off by
// default: a node that has not opted in refuses stage, activate and rollback.
type UpdateConfig struct {
	Enabled bool `yaml:"enabled"`
	// Source is the release download root: <source>/<version>/<asset>. A
	// request names a version, never a URL. Only DefaultUpdateSource is
	// accepted (validateSource).
	Source string `yaml:"source"`
	// ConfirmTimeout bounds how long a newly activated release has to bring
	// every previously healthy backend back. Zero derives it (ConfirmWindow).
	ConfirmTimeout Duration `yaml:"confirm_timeout"`
}

// PeekSource reads update.source from a config file as leniently as
// PeekUpdate, the default when it is absent, and refuses any other value,
// as Validate does. The host's engine helper reads it: it verifies releases itself,
// from the same source as the node.
func PeekSource(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var peek struct {
		Update struct {
			Source string `yaml:"source"`
		} `yaml:"update"`
	}
	if err := yaml.Unmarshal(b, &peek); err != nil {
		return "", err
	}
	src := peek.Update.Source
	if src == "" {
		src = DefaultUpdateSource
	}
	return src, validateSource(src)
}

// PeekParrotAPI reads viiwork_parrot.api from a config file as leniently as
// PeekUpdate, the default when absent, and refuses one Validate would refuse.
// The host's engine helper reads it to stage through the same parrot as the
// node.
func PeekParrotAPI(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var peek struct {
		ViiworkParrot struct {
			API string `yaml:"api"`
		} `yaml:"viiwork_parrot"`
	}
	if err := yaml.Unmarshal(b, &peek); err != nil {
		return "", err
	}
	api := peek.ViiworkParrot.API
	if api == "" {
		api = Defaults().ViiworkParrot.API
	}
	return api, validParrotAPI(api)
}

func (c *Config) validateUpdate() error {
	if err := validateSource(c.Update.Source); err != nil {
		return err
	}
	if c.Update.ConfirmTimeout.Duration < 0 {
		return fmt.Errorf("update.confirm_timeout must be >= 0")
	}
	return nil
}

// validateSource accepts only DefaultUpdateSource. A release is signed, so
// another host could not smuggle code in, but it could choose which signed
// releases the fleet sees, withhold them or watch the fleet ask: releases
// come from GitHub and nowhere else. The key stays because operator keys are
// frozen, and a trailing slash is forgiven.
func validateSource(src string) error {
	if strings.TrimSuffix(src, "/") != DefaultUpdateSource {
		return fmt.Errorf("update.source %q: releases are downloaded only from %s (remove the key)", src, DefaultUpdateSource)
	}
	return nil
}

// ConfirmWindow is how long a newly activated release has to bring every
// previously healthy backend back before the node returns to its last good
// release: update.confirm_timeout when set, else the sum of the models'
// effective startup timeouts plus 10 minutes. Loads are serialised through
// the load gate, so "all healthy" takes the sum, not the longest.
func (c *Config) ConfirmWindow() time.Duration {
	if c.Update.ConfirmTimeout.Duration > 0 {
		return c.Update.ConfirmTimeout.Duration
	}
	total := 10 * time.Minute
	for _, m := range c.Models {
		d := m.StartupTimeout.Duration
		if d <= 0 {
			if e, ok := engine.Lookup(m.Engine); ok {
				d = e.DefaultStartupTimeout()
			}
		}
		total += d
	}
	return total
}
