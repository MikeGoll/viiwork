package energy

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strings"
	"sync"
)

// maxModels is bounded by ModelIdx being a uint16. A node serves a handful of
// models, so the ceiling exists only to make the failure explicit.
const maxModels = 65535

// modelTable maps model names to the small integers stamped into GPURecord.
// It is append-only: an index, once handed out, keeps its meaning for as long
// as the rings that reference it, so a name is never reused for another model.
type modelTable struct {
	mu    sync.RWMutex
	path  string
	names []string
	index map[string]uint16
}

// openModelTable loads the table. A last line without its newline is a torn
// append — a crash or a failed write part way through — whose index was never
// handed out, since Index returns only after a complete, synced line. It is
// ignored and cut off, so the next append starts on a line of its own instead
// of gluing onto the fragment and shifting every later index by one.
func openModelTable(path string) (*modelTable, error) {
	t := &modelTable{path: path, index: make(map[string]uint16)}

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return t, nil
		}
		return nil, err
	}
	complete := len(raw)
	if complete > 0 && raw[complete-1] != '\n' {
		complete = bytes.LastIndexByte(raw, '\n') + 1
		if err := os.Truncate(path, int64(complete)); err != nil {
			return nil, fmt.Errorf("dropping torn last line of %s: %w", path, err)
		}
	}

	scanner := bufio.NewScanner(bytes.NewReader(raw[:complete]))
	for scanner.Scan() {
		name := strings.TrimRight(scanner.Text(), "\r")
		t.index[name] = uint16(len(t.names))
		t.names = append(t.names, name)
	}
	return t, scanner.Err()
}

// Index returns the index for a model name, appending it if new. The empty
// name is index 0 and means "no model was resident", which is a real state:
// a GPU can be powered and idle between deployments.
func (t *modelTable) Index(name string) (uint16, error) {
	if name == "" {
		return 0, nil
	}

	t.mu.RLock()
	if idx, ok := t.index[name]; ok {
		t.mu.RUnlock()
		return idx, nil
	}
	t.mu.RUnlock()

	t.mu.Lock()
	defer t.mu.Unlock()
	if idx, ok := t.index[name]; ok {
		return idx, nil
	}
	// The in-memory table changes only after the line is on disk: a failed
	// append must not leave an index here that the file does not have, or the
	// next successful append would be read back at a different index.
	if len(t.names) == 0 {
		// Reserve 0 for "no model" so a zeroed record does not read as a real one.
		if err := t.append(""); err != nil {
			return 0, err
		}
		t.names = append(t.names, "")
		t.index[""] = 0
	}
	if len(t.names) >= maxModels {
		return 0, fmt.Errorf("model table full (%d entries)", maxModels)
	}

	if err := t.append(name); err != nil {
		return 0, err
	}
	idx := uint16(len(t.names))
	t.names = append(t.names, name)
	t.index[name] = idx
	return idx, nil
}

// Name resolves an index back to a model name. An index from a ring written by
// a build with a longer table reads as unknown rather than as another model.
func (t *modelTable) Name(idx uint16) string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if int(idx) >= len(t.names) {
		return ""
	}
	return t.names[idx]
}

// append writes one complete line and syncs it. A write that fails part way is
// cut back off, best effort, so the file keeps ending on a line boundary; a
// fragment that survives anyway (a crash) is dropped on the next open.
func (t *modelTable) append(name string) error {
	f, err := os.OpenFile(t.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if _, err := f.WriteString(name + "\n"); err != nil {
		_ = f.Truncate(info.Size())
		return err
	}
	if err := f.Sync(); err != nil {
		// The caller will not use this index, so the line must not stay either.
		_ = f.Truncate(info.Size())
		return err
	}
	return nil
}
