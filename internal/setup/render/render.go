// Package render turns the setup wizard's answers into the files it writes:
// viiwork.yaml, mesh.env and the Docker Compose file. The config is checked
// by the node's own config.Parse and Validate before it is returned, so a
// file the node would refuse never reaches the disk.
package render

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/janit/viiwork/v2/internal/config"
	_ "github.com/janit/viiwork/v2/internal/engine/all" // Validate checks each model's engine block
	"gopkg.in/yaml.v3"
)

// Model is one served model.
type Model struct {
	Name                          string
	Path                          string // as the node sees it: /models/... in a container
	Size                          int64  // weights in bytes, all shards
	GPUs                          []int
	PerBackend, Context, Parallel int
	Binary                        string // llamacpp.binary; empty keeps the engine's default
}

// Answers are the wizard's decisions.
type Answers struct {
	Node     string
	StateDir string // empty keeps the default
	Network  string // config.NetworkTailnet or config.NetworkLAN
	Open     bool
	Seeds    []string
	Update   bool
	Vendor   string
	Models   []Model
}

// A split backend or weights over 20 GiB load slowly. These are the values
// internal/accept's config check asks for.
const (
	largeWeights = 20 << 30
	longStartup  = "45m"
)

const header = "# Written by viiwork init. Edit freely, then reload the node (SIGHUP).\n" +
	"# Every key and its default: viiwork.yaml.example in the repository.\n\n"

type file struct {
	Node   nodeY    `yaml:"node"`
	Mesh   meshY    `yaml:"mesh"`
	GPU    gpuY     `yaml:"gpu"`
	Update *updateY `yaml:"update,omitempty"`
	Models []modelY `yaml:"models"`
}

type nodeY struct {
	Name     string `yaml:"name"`
	StateDir string `yaml:"state_dir,omitempty"`
}

type meshY struct {
	Network string   `yaml:"network"`
	Open    bool     `yaml:"open,omitempty"`
	Seeds   []string `yaml:"seeds,omitempty"`
}

type gpuY struct {
	Vendor string `yaml:"vendor"`
}

type updateY struct {
	Enabled bool `yaml:"enabled"`
}

type modelY struct {
	Name           string  `yaml:"name"`
	Engine         string  `yaml:"engine"`
	Path           string  `yaml:"path"`
	GPUs           []int   `yaml:"gpus,flow"`
	GPUsPerBackend int     `yaml:"gpus_per_backend"`
	Context        int     `yaml:"context"`
	Parallel       int     `yaml:"parallel"`
	StartupTimeout string  `yaml:"startup_timeout,omitempty"`
	Llamacpp       *llamaY `yaml:"llamacpp,omitempty"`
}

type llamaY struct {
	Binary string `yaml:"binary"`
}

// Config renders viiwork.yaml. secret is the mesh secret Validate is given
// (nil for an open mesh); it is never written into the file.
func Config(a Answers, secret []byte) ([]byte, error) {
	f := file{
		Node: nodeY{Name: a.Node, StateDir: a.StateDir},
		Mesh: meshY{Network: a.Network, Open: a.Open, Seeds: a.Seeds},
		GPU:  gpuY{Vendor: a.Vendor},
	}
	if a.Update {
		f.Update = &updateY{Enabled: true}
	}
	for _, m := range a.Models {
		y := modelY{Name: m.Name, Engine: "llamacpp", Path: m.Path, GPUs: m.GPUs,
			GPUsPerBackend: m.PerBackend, Context: m.Context, Parallel: m.Parallel}
		if m.PerBackend > 1 || m.Size > largeWeights {
			y.StartupTimeout = longStartup
		}
		if m.Binary != "" {
			y.Llamacpp = &llamaY{Binary: m.Binary}
		}
		f.Models = append(f.Models, y)
	}
	var body bytes.Buffer
	body.WriteString(header)
	enc := yaml.NewEncoder(&body)
	enc.SetIndent(2)
	if err := enc.Encode(f); err != nil {
		return nil, err
	}
	data := body.Bytes()
	cfg, err := config.Parse(data)
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	if secret != nil {
		env[config.DefaultMeshSecretEnv] = base64.StdEncoding.EncodeToString(secret)
	}
	if err := cfg.Validate(func(k string) (string, bool) { v, ok := env[k]; return v, ok }); err != nil {
		return nil, err
	}
	return data, nil
}

// MeshEnv is mesh.env for a secured mesh.
func MeshEnv(secret []byte) []byte {
	return []byte(config.DefaultMeshSecretEnv + "=" + base64.StdEncoding.EncodeToString(secret) + "\n")
}

// Compose is the Docker Compose file's inputs. Every path is the host's.
type Compose struct {
	Image      string
	ModelsDir  string // mounted read-only at /models
	ConfigFile string
	EnvFile    string // empty for an open mesh
	StateDir   string
	Tailscale  bool // mount tailscaled's socket directory
}

var composeTmpl = template.Must(template.New("compose").Parse(`# Written by viiwork init; ` + "`viiwork uninstall`" + ` removes it.
# Reload the model list after editing viiwork.yaml: docker kill -s HUP viiwork
name: viiwork
services:
  viiwork:
    image: {{.Image}}
    container_name: viiwork
    # always, not unless-stopped: unless-stopped remembers a manual stop across
    # a daemon restart and leaves the node down after a reboot.
    restart: always
    # Gossip binds the machine's own address, which a bridged container does
    # not have.
    network_mode: host
    # The on-GPU check matches backends against host PIDs.
    pid: host
    environment:
      NVIDIA_DRIVER_CAPABILITIES: compute,utility
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              count: all
              capabilities: [gpu]
    volumes:
      - "{{.ModelsDir}}:/models:ro"
      - "{{.ConfigFile}}:/etc/viiwork/viiwork.yaml:ro"
      - "{{.StateDir}}:/var/lib/viiwork"
{{- if .Tailscale}}
      - "/var/run/tailscale:/var/run/tailscale:ro"
{{- end}}
{{- if .EnvFile}}
    env_file:
      - "{{.EnvFile}}"
{{- end}}
    # The base image's own HEALTHCHECK probes llama-server's port, which the
    # node does not serve on.
    healthcheck:
      test: ["CMD", "curl", "-fsS", "http://127.0.0.1:8086/health"]
      interval: 30s
      timeout: 5s
      start_period: 60s
    # Leaving the mesh and draining takes about 75 s.
    stop_grace_period: 90s
`))

// ComposeFile renders the compose file for an NVIDIA host. A path that is not
// absolute, or holds a colon, a quote or a line break, is refused: it would
// break the volume syntax. So is a '$', which Compose expands as a variable.
func ComposeFile(c Compose) ([]byte, error) {
	paths := []string{c.ModelsDir, c.ConfigFile, c.StateDir}
	if c.EnvFile != "" {
		paths = append(paths, c.EnvFile)
	}
	for _, p := range paths {
		if !filepath.IsAbs(p) || strings.ContainsAny(p, ":\"\\\n\r$") {
			return nil, fmt.Errorf("path %q cannot be mounted: use an absolute path without ':', '$', quotes or backslashes", p)
		}
	}
	var b bytes.Buffer
	if err := composeTmpl.Execute(&b, c); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
