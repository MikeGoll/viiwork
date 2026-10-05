// Package durable replaces files so that a crash leaves either the old file or
// the new one, never a torn one.
package durable

import (
	"os"
	"path/filepath"
)

// WriteFile replaces dir/name: it writes and syncs a temporary file of its
// own, renames it over the target, then syncs the directory so the rename
// itself survives. The temporary file's name is unique, so concurrent writers
// can never write into each other's file: the result is always exactly one
// writer's content.
func WriteFile(dir, name string, data []byte) error {
	return WriteFileMode(dir, name, data, 0o644)
}

// WriteFileMode is WriteFile with the file's mode. The temporary file is
// created 0600 and gets mode only after its content is written, so a secret
// is never readable by others, even for a moment.
func WriteFileMode(dir, name string, data []byte, mode os.FileMode) (err error) {
	f, err := os.CreateTemp(dir, name+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
