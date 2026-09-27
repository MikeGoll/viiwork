package config

import (
	"fmt"
	"net/url"
	"os"
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

// UpdateConfig is how this node takes part in rolling updates. Off by
// default: a node that has not opted in refuses stage, activate and rollback.
type UpdateConfig struct {
	Enabled bool `yaml:"enabled"`
	// Source is the release download root: <source>/<version>/<asset>. A
	// request names a version, never a URL.
	Source string `yaml:"source"`
	// ConfirmTimeout bounds how long a newly activated release has to bring
	// every previously healthy backend back. Zero derives it (ConfirmWindow).
	ConfirmTimeout Duration `yaml:"confirm_timeout"`
}

func (c *Config) validateUpdate() error {
	src := c.Update.Source
	u, err := url.Parse(src)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("update.source %q must be an https URL with no query", src)
	}
	switch u.Scheme {
	case "https":
	case "http":
		// Only a test server on this machine. A release is signed, so http
		// could not smuggle code in, but anyone on the path could withhold
		// or observe the fleet's updates.
		if h := u.Hostname(); h != "127.0.0.1" && h != "localhost" && h != "::1" {
			return fmt.Errorf("update.source %q: http is allowed only for a loopback host", src)
		}
	default:
		return fmt.Errorf("update.source %q must be an https URL", src)
	}
	if c.Update.ConfirmTimeout.Duration < 0 {
		return fmt.Errorf("update.confirm_timeout must be >= 0")
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
