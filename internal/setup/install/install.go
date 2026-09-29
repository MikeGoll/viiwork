// Package install writes what the setup wizard decided, starts the node, and
// records everything it created in a manifest, which `viiwork uninstall`
// trusts and nothing else.
package install

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
)

// Where a Linux install puts things.
const (
	ConfigDir    = "/etc/viiwork"
	ConfigFile   = "/etc/viiwork/viiwork.yaml"
	EnvFile      = "/etc/viiwork/mesh.env"
	ComposeFile  = "/etc/viiwork/docker-compose.yaml"
	ManifestFile = "/etc/viiwork/install.json"
	// StateDir is a host directory, not a named volume: any `docker volume
	// prune` would delete a named volume while the node is stopped.
	StateDir   = "/var/lib/viiwork"
	BinaryPath = "/usr/local/bin/viiwork"
	Project    = "viiwork"
)

// manifestVersion is the format this build writes and understands.
const manifestVersion = 1

// Manifest is install.json: everything the install created, and nothing else.
type Manifest struct {
	Version     int      `json:"version"`
	OS          string   `json:"os"`
	InstalledBy string   `json:"installed_by"`
	Files       []string `json:"files"`
	Dirs        []string `json:"dirs"`
	Compose     *Compose `json:"compose,omitempty"`
	Images      []string `json:"images"`
	LaunchAgent string   `json:"launch_agent"`
	Binary      string   `json:"binary"`
	ModelDirs   []string `json:"model_dirs"`
	// LlamaRoot is where a Mac install keeps its llama.cpp builds, one
	// directory per tag. A running node reads it to fetch the build a staged
	// release is pinned to and to run the build of its own pin. Absent from a
	// manifest written before v2.6.0, and on Linux.
	LlamaRoot string `json:"llama_root,omitempty"`
	// EngineHelper is absent on an install made before v2.6.0 until
	// `viiwork init` adds it. Unknown fields are ignored by every reader, so
	// an older binary still reads a manifest that has it.
	EngineHelper *EngineHelper `json:"engine_helper,omitempty"`
}

// Compose is the compose project the install started.
type Compose struct {
	File    string   `json:"file"`
	Project string   `json:"project"`
	Volumes []string `json:"volumes"`
}

// ReadManifest reads install.json, refusing a format this build does not know.
func ReadManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", path, err)
	}
	if m.Version != manifestVersion {
		return Manifest{}, fmt.Errorf("%s: manifest version %d was written by a newer viiwork", path, m.Version)
	}
	return m, nil
}

// File is one file to write.
type File struct {
	Path string // absolute, as on the host
	Mode fs.FileMode
	Data []byte
}

// Exec runs a command with its output streamed to out.
type Exec func(ctx context.Context, out io.Writer, name string, args ...string) error

// ExecCommand is the real Exec.
func ExecCommand(ctx context.Context, out io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}
