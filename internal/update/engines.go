package update

import (
	"context"
	"errors"
	"fmt"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/engine"
)

// ErrEngineTooOld marks a release this host's engines cannot run.
var ErrEngineTooOld = errors.New("engine too old for this release")

// CheckEngines requires every engine the configured models use to be at least
// the version required names for it. An engine with no requirement, or that
// no model uses, is not checked. A version that cannot be read fails: "cannot
// say" is not "new enough".
func CheckEngines(ctx context.Context, models []config.Model, required map[string]string) error {
	for _, m := range models {
		min := required[m.Engine]
		if min == "" {
			continue
		}
		e, ok := engine.Lookup(m.Engine)
		if !ok {
			continue
		}
		v, ok := e.(engine.Versioner)
		if !ok {
			return fmt.Errorf("%w: the release needs %s %s, and this node cannot read %s's version", ErrEngineTooOld, m.Engine, min, m.Engine)
		}
		installed, err := v.Version(ctx, config.ModelSpec(m))
		if err != nil {
			return fmt.Errorf("%w: model %s: the release needs %s %s; the installed version cannot be read: %v", ErrEngineTooOld, m.Name, m.Engine, min, err)
		}
		if ok, err := v.AtLeast(installed, min); err != nil || !ok {
			return fmt.Errorf("%w: model %s: the release needs %s %s, installed is %s", ErrEngineTooOld, m.Name, m.Engine, min, installed)
		}
	}
	return nil
}

// InstalledEngines is the installed version of each engine the models use,
// read from the first model of each engine; an engine whose version cannot be
// read is absent, never reported as a guess.
func InstalledEngines(ctx context.Context, models []config.Model) map[string]string {
	out := map[string]string{}
	for _, m := range models {
		if _, done := out[m.Engine]; done {
			continue
		}
		e, ok := engine.Lookup(m.Engine)
		if !ok {
			continue
		}
		if v, ok := e.(engine.Versioner); ok {
			if installed, err := v.Version(ctx, config.ModelSpec(m)); err == nil {
				out[m.Engine] = installed
			}
		}
	}
	return out
}
