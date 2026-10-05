package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/janit/viiwork/v2/internal/durable"
)

// The engine helper: `viiwork engine-sync`, run as root on the host by
// systemd, makes the compose file's image follow the node's current release
// (docs/releases.md). Its units live in SystemdDir; EngineDir is its own,
// root-only directory, outside everything the container mounts.
const (
	SystemdDir    = "/etc/systemd/system"
	EngineDir     = "/var/lib/viiwork-engine"
	EngineService = "viiwork-engine.service"
	EnginePath    = "viiwork-engine.path"
	EngineTimer   = "viiwork-engine.timer"
)

// EngineUnits are the helper's units: the service, and the path unit and
// timer that start it.
var EngineUnits = []string{EngineService, EnginePath, EngineTimer}

// EngineHelper records the helper. It is also the node's sign that it runs
// on a helper-managed install (ManagedInstall).
type EngineHelper struct {
	Units []string `json:"units"`
	Dir   string   `json:"dir"`
}

// ManagedInstall reports whether the manifest at path is a Docker install
// whose image the engine helper swaps. A node reads it from the
// /etc/viiwork its container mounts read-only; anything short of a readable
// Linux manifest with both a compose project and the helper — a hand-built
// node, a beta install the helper was never added to — is not.
func ManagedInstall(path string) bool {
	m, err := ReadManifest(path)
	return err == nil && m.OS == "linux" && m.Compose != nil && m.EngineHelper != nil
}

// EnableEngineHelper makes the helper's directory root-only, loads its
// units and starts watching.
func (l Linux) EnableEngineHelper(ctx context.Context) error {
	if err := os.MkdirAll(l.host(EngineDir), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(l.host(EngineDir), 0o700); err != nil {
		return err
	}
	if err := l.Exec(ctx, l.Out, "systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	if err := l.Exec(ctx, l.Out, "systemctl", "enable", "--now", EnginePath, EngineTimer); err != nil {
		return fmt.Errorf("enabling the engine helper: %w", err)
	}
	return nil
}

// AddEngineHelper adds the helper to an install made before it existed:
// units, its directory, and this binary at BinaryPath (the one that has
// engine-sync), leaving the config and everything else as it is. Like Write,
// the manifest is written first with what is about to be created, so that
// uninstall can undo a run that stops part-way.
func (l Linux) AddEngineHelper(ctx context.Context, m Manifest, units []File) (Manifest, error) {
	w := writer{root: l.Root, os: "linux", manifest: ManifestFile, binary: BinaryPath, executable: l.Executable}
	m.Files, m.Dirs = slices.Clone(m.Files), slices.Clone(m.Dirs)
	for _, u := range units {
		if !slices.Contains(m.Files, u.Path) {
			m.Files = append(m.Files, u.Path)
		}
	}
	if _, err := os.Stat(l.host(EngineDir)); err != nil && !slices.Contains(m.Dirs, EngineDir) {
		m.Dirs = append(m.Dirs, EngineDir)
	}
	// The files are recorded before they are written, so an uninstall finds
	// them whatever happens next; the helper itself only once it runs, so a
	// failed enable neither makes the node wait on a helper that never acts
	// nor stops the next `sudo viiwork init` from adding it again.
	if err := w.writeManifest(m); err != nil {
		return m, err
	}
	for _, u := range units {
		if err := ctx.Err(); err != nil {
			return m, err
		}
		dir := filepath.Dir(l.host(u.Path))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return m, err
		}
		if err := durable.WriteFileMode(dir, filepath.Base(u.Path), u.Data, u.Mode); err != nil {
			return m, fmt.Errorf("writing %s: %w", u.Path, err)
		}
	}
	if err := w.copyBinary(); err != nil {
		return m, err
	}
	if err := l.EnableEngineHelper(ctx); err != nil {
		return m, err
	}
	m.EngineHelper = &EngineHelper{Units: EngineUnits, Dir: EngineDir}
	return m, w.writeManifest(m)
}
