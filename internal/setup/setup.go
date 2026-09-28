// Package setup is the first-run wizard: `viiwork init`, or `viiwork` on a
// terminal with no config. It asks, shows every file it will write, and
// writes only after a yes; on Linux it then starts the node in Docker and
// checks it. Every question can be abandoned with nothing written.
package setup

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path/filepath"
	"time"

	"github.com/janit/viiwork/v2/internal/accept"
	"github.com/janit/viiwork/v2/internal/gguf"
	"github.com/janit/viiwork/v2/internal/gpu"
	"github.com/janit/viiwork/v2/internal/setup/discover"
	"github.com/janit/viiwork/v2/internal/setup/install"
	"github.com/janit/viiwork/v2/internal/setup/plan"
	"github.com/janit/viiwork/v2/internal/setup/probe"
	"github.com/janit/viiwork/v2/internal/setup/prompt"
)

// Host is the machine the wizard runs on. Tests replace every field.
type Host struct {
	GOOS, GOARCH string
	Version      string // this binary's; only a release has published images
	Root         string // prefixed to every path read or written; "" in production
	Home         string // the invoking user's home, even under sudo
	Euid         int
	Hostname     func() (string, error)
	Run          gpu.Runner   // nvidia-smi, docker version/ps/info
	Exec         install.Exec // docker pull and compose, output shown
	Sys          fs.FS        // /sys
	PortFree     func(network string, port int) bool
	Discover     func(ctx context.Context) []discover.Node
	Tailnet      func(ctx context.Context) bool // tailscaled is running here
	Rand         io.Reader
	Executable   string
	HTTP         *http.Client
	Accept       accept.Env
	NodeAPI      string // this node's API, once started
	PeerAPIPort  int    // every node's API port (8086)
	Out          io.Writer
	ReadyTimeout time.Duration
	UpTimeout    time.Duration // for the node's API to answer at all
	Poll         time.Duration // between status polls
	UID          int           // for launchctl's gui/<uid> domain (macOS)
	LlamaPin     string        // the llama.cpp release this build was cut against
	LlamaAPI     string        // install.LlamaReleaseAPI in production
	Download     *http.Client  // for GitHub downloads; honours HTTPS_PROXY
	Path         string        // the invoking shell's PATH (the Mac hint for ~/.local/bin)
	// Signals wraps the write step so that Ctrl-C stops after the current
	// file. nil: no signal handling.
	Signals func(ctx context.Context) (context.Context, context.CancelFunc)
}

func (h Host) path(p string) string { return filepath.Join(h.Root, p) }

// imageRepo is the engine image a Linux NVIDIA host runs.
const imageRepo = "ghcr.io/janit/viiwork-llamacpp-cuda"

// weights is one GGUF model found in the models directory.
type weights struct {
	file string // relative to the models directory
	name string
	info gguf.Info
}

// state is what the steps decide, in order.
type state struct {
	gpus      []probe.GPU
	vendor    string
	image     string // empty: write the config only
	cdi       bool   // Docker reaches the cards as CDI devices
	why       string // why there is no image
	modelsDir string
	models    []weights
	placed    []plan.Placement
	network   string
	secret    []byte
	open      bool
	seeds     []string
	update    bool
	name      string
	nearby    []string // node names discovery found
	budget    int64    // the Metal budget in MiB (Apple GPU)
	mac       install.MacPaths
	llama     string // llama-server's path (Apple GPU)
}

// Run is the wizard. configPath is where the config goes; a Linux install
// only writes install.ConfigFile.
func Run(ctx context.Context, h Host, p prompt.Prompter, configPath string) error {
	var pre func(context.Context, Host, string) error
	write := writeAndStart
	switch h.GOOS {
	case "linux":
		pre = preflight
	case "darwin":
		pre, write = preflightMac, writeAndStartMac
	default:
		return fmt.Errorf("setup does not support %s", h.GOOS)
	}
	if err := pre(ctx, h, configPath); err != nil {
		return err
	}
	s := &state{}
	for _, step := range []func(context.Context, Host, prompt.Prompter, *state) error{
		hardware, chooseModels, layout, meshStep, updates, naming,
	} {
		if err := step(ctx, h, p, s); err != nil {
			return err
		}
	}
	return write(ctx, h, p, s)
}

// DefaultConfigPath is where init writes the config on goos: the Linux
// install's /etc/viiwork, or a Mac user's ~/.config/viiwork.
func DefaultConfigPath(goos, home string) string {
	if goos == "darwin" {
		return install.MacLayout(home).ConfigFile
	}
	return install.ConfigFile
}
