package install

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/janit/viiwork/v2/internal/durable"
)

// writer is the manifest-first write both platforms share.
type writer struct {
	root       string
	os         string
	manifest   string // the manifest's path
	binary     string // where the running binary is copied
	executable string
	beforeFile func(path string)
}

func (w writer) host(p string) string { return filepath.Join(w.root, p) }

// write creates the dirs that do not exist yet, writes the files in order,
// each atomically, and copies the running binary.
//
// The manifest is written twice. First, before anything else, it lists every
// path about to be created, so a crash or kill part-way leaves a manifest
// that uninstall can act on (it tolerates entries that never appeared). At
// the end it is rewritten to list only what exists. A cancelled ctx stops
// after the current file. A binary already in place is replaced but not
// claimed, and neither is a dir that already existed: uninstall must not
// delete what the install did not create.
func (w writer) write(ctx context.Context, dirs []string, files []File, m Manifest) (Manifest, error) {
	m.Version, m.OS = manifestVersion, w.os
	var newDirs []string
	for _, d := range dirs {
		if _, err := os.Stat(w.host(d)); err != nil {
			newDirs = append(newDirs, d)
		}
	}
	_, err := os.Stat(w.host(w.binary))
	claimBinary := err != nil

	planned := m
	planned.Dirs, planned.Files, planned.Binary = newDirs, nil, ""
	for _, f := range files {
		planned.Files = append(planned.Files, f.Path)
	}
	if claimBinary {
		planned.Binary = w.binary
	}
	if err := ctx.Err(); err != nil {
		return m, err
	}
	if err := w.writeManifest(planned); err != nil {
		return m, err
	}

	m.Files, m.Dirs, m.Binary = nil, nil, ""
	var stop error
	for _, d := range newDirs {
		if err := os.MkdirAll(w.host(d), 0o755); err != nil {
			stop = err
			break
		}
		m.Dirs = append(m.Dirs, d)
	}
	for _, f := range files {
		if stop == nil {
			stop = ctx.Err()
		}
		if stop != nil {
			break
		}
		if w.beforeFile != nil {
			w.beforeFile(f.Path)
		}
		if err := os.MkdirAll(filepath.Dir(w.host(f.Path)), 0o755); err != nil {
			stop = err
			break
		}
		if err := durable.WriteFileMode(filepath.Dir(w.host(f.Path)), filepath.Base(f.Path), f.Data, f.Mode); err != nil {
			stop = fmt.Errorf("writing %s: %w", f.Path, err)
			break
		}
		m.Files = append(m.Files, f.Path)
	}
	if stop == nil {
		stop = ctx.Err()
	}
	if stop == nil {
		if err := w.copyBinary(); err != nil {
			stop = err
		} else if claimBinary {
			m.Binary = w.binary
		}
	}
	if err := w.writeManifest(m); err != nil && stop == nil {
		stop = err
	}
	return m, stop
}

func (w writer) writeManifest(m Manifest) error {
	dir := filepath.Dir(w.host(w.manifest))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(m, "", "  ")
	return durable.WriteFileMode(dir, filepath.Base(w.manifest), append(data, '\n'), 0o644)
}

func (w writer) copyBinary() error {
	data, err := os.ReadFile(w.executable)
	if err != nil {
		return fmt.Errorf("reading the running binary: %w", err)
	}
	dir := filepath.Dir(w.host(w.binary))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := durable.WriteFileMode(dir, filepath.Base(w.binary), data, 0o755); err != nil {
		return fmt.Errorf("writing %s: %w", w.binary, err)
	}
	return nil
}
