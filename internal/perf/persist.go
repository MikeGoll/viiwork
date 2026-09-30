package perf

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/janit/viiwork/v2/internal/durable"
)

// fileName is the baseline file under state_dir. Losing it costs a few
// minutes of learning, never a start: Load's error is for logging.
const fileName = "perf.json"

// Load reads saved baselines. A missing file is a first start, not an error.
// Baselines are held with the key they were measured under, and SetKeys
// drops any whose key no longer matches.
func (t *Tracker) Load(dir string) error {
	data, err := os.ReadFile(filepath.Join(dir, fileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("perf: reading %s: %w", fileName, err)
	}
	var saved map[string]baseline
	if err := json.Unmarshal(data, &saved); err != nil {
		return fmt.Errorf("perf: %s is not valid: %w", fileName, err)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for name, b := range saved {
		b := b
		m := t.get(name)
		m.base, m.cached = &b, false
	}
	return nil
}

// Save writes the baselines if any changed since the last Save.
func (t *Tracker) Save(dir string) error {
	t.mu.Lock()
	if !t.dirty {
		t.mu.Unlock()
		return nil
	}
	out := map[string]baseline{}
	for name, m := range t.models {
		if m.base != nil {
			out[name] = *m.base
		}
	}
	t.dirty = false
	t.mu.Unlock()
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if err := durable.WriteFile(dir, fileName, data); err != nil {
		t.mu.Lock()
		t.dirty = true
		t.mu.Unlock()
		return fmt.Errorf("perf: writing %s: %w", fileName, err)
	}
	return nil
}
