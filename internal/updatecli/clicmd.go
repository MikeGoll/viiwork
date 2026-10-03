package updatecli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/parrot"
	"github.com/janit/viiwork/v2/internal/release"
	"github.com/janit/viiwork/v2/internal/update"
)

// CLIUsage is `viiwork update cli`'s.
const CLIUsage = `usage: viiwork update cli [--to PATH] [--state-dir DIR] [--dry-run] [--node host:port] [--config path]

Installs the release this machine's node runs as this machine's viiwork CLI.
It asks the node (on loopback) which release it runs, takes that release's
viiwork from <state_dir>/releases/<version>/, verifies it against the signed
release exactly as staging does (SHA256SUMS and its signature from GitHub,
the archive from GitHub or the host's viiwork-parrot), and replaces the CLI
in one rename, keeping the old one as <path>.prev. Only ever forward: a CLI
at or past the node's release is left alone. Run it with sudo where the CLI
is root's (/usr/local/bin/viiwork).

flags:
  --to PATH         the CLI to replace (default: this viiwork, links resolved)
  --state-dir DIR   the node's state_dir (default: node.state_dir in --config)
  --dry-run         say what would be done, and do nothing
  --node host:port  the node to ask (default 127.0.0.1:<api.port> from --config, else 127.0.0.1:8086)
  --config path     viiwork.yaml (default: this machine's)
`

// cliTimeout bounds the verification, which downloads the release's archive.
const cliTimeout = 10 * time.Minute

// maxCLI bounds what is hashed of the node's staged copy.
const maxCLI = 512 << 20

// runCLI is `viiwork update cli`.
func runCLI(ctx context.Context, args []string, env Env) int {
	fs := flag.NewFlagSet("update cli", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	to := fs.String("to", "", "")
	stateDir := fs.String("state-dir", "", "")
	dryRun := fs.Bool("dry-run", false, "")
	node := fs.String("node", "", "")
	configPath := fs.String("config", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprint(env.Stderr, CLIUsage)
		return 2
	}
	cfg := *configPath
	if cfg == "" {
		if _, err := os.Stat(env.DefaultConfig); env.DefaultConfig != "" && err == nil {
			cfg = env.DefaultConfig
		}
	}
	// The port is peeked, not parsed: a CLI behind its node is exactly the
	// one that may not know every key of the node's config.
	addr := *node
	if addr == "" && cfg != "" {
		if p := peekPort(cfg); p > 0 {
			addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(p))
		}
	}
	c, code := newCLI(env, addr, "", "")
	if code != 0 {
		return code
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(env.Stderr, "viiwork update cli: "+format+"\n", a...)
		return 1
	}

	target := *to
	if target == "" {
		exe, err := env.Executable()
		if err != nil {
			return fail("finding this viiwork: %v; pass --to", err)
		}
		target = exe
	}
	if p, err := filepath.EvalSymlinks(target); err == nil {
		target = p
	}
	if abs, err := filepath.Abs(target); err == nil {
		target = abs
	}

	st, err := c.updateStatus(ctx, c.node)
	if err != nil {
		return fail("asking the node at %s which release it runs: %v", c.node, err)
	}
	version := st.Running
	if !release.ValidVersion(version) {
		return fail("the node at %s runs %q, which is not a signed release: there is nothing to install", c.node, version)
	}

	dir := *stateDir
	if dir == "" {
		if cfg == "" {
			return fail("cannot tell the node's state directory: pass --state-dir or --config")
		}
		d, _, err := config.PeekUpdate(cfg)
		if err != nil {
			return fail("cannot tell the node's state directory from %s (%v): pass --state-dir", cfg, err)
		}
		dir = d
	}
	staged := filepath.Join(update.ReleasesDir(dir), version, "viiwork")
	stagedSum, err := regularSHA256(staged)
	if err != nil {
		return fail("the node runs %s, but %s is missing (%v): a node keeps a release there once it has staged it; is the state directory right (--state-dir)?", version, staged, err)
	}

	behind, was := update.CLIBehind(ctx, target, version)
	if !behind {
		fmt.Fprintf(env.Stdout, "%s is %s and the node runs %s: nothing to do\n", target, was, version)
		return 0
	}
	if *dryRun {
		fmt.Fprintf(env.Stdout, "would verify %s against the signed %s release, then install it as %s (now %s), keeping the old one as %s.prev\n",
			staged, version, target, was, target)
		return 0
	}
	if err := canWrite(filepath.Dir(target)); err != nil {
		return fail("cannot write %s (%v): run it with sudo, or pass --to a path you can write", filepath.Dir(target), err)
	}

	data, err := verifyCLI(ctx, env, cfg, filepath.Dir(target), version, staged, stagedSum)
	if err != nil {
		return fail("not installing %s: %v", version, err)
	}
	if err := update.InstallCLI(target, data); err != nil {
		return fail("installing %s at %s: %v", version, target, err)
	}
	fmt.Fprintf(env.Stdout, "%s: %s -> %s (the old one is %s.prev)\n", target, was, version, target)
	return 0
}

// verifyCLI stages version afresh, with this binary's keys and the code a
// node stages with, into a temporary directory beside the CLI (which must
// allow executing: staging runs the binary), and requires the node's staged
// copy, whose sha256 is got, to be byte for byte that signed release's
// viiwork. It returns the verified bytes. The state directory is never
// trusted on its own: on a Docker install the container can write it.
func verifyCLI(ctx context.Context, env Env, cfg, beside, version, staged, got string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, cliTimeout)
	defer cancel()
	keys := env.Keys
	if keys == nil {
		var err error
		if keys, err = release.Keys(); err != nil {
			return nil, err
		}
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = time.Minute
	st := &update.Stager{Source: config.DefaultUpdateSource, Client: &http.Client{Transport: tr}, Keys: keys,
		Target: release.Target{OS: runtime.GOOS, Arch: runtime.GOARCH},
		Log:    func(format string, a ...any) { fmt.Fprintf(env.Stderr, format+"\n", a...) }}
	if cfg != "" {
		if s, err := config.PeekSource(cfg); err == nil {
			st.Source = s
		}
		// The same parrot as the node's.
		if api, err := config.PeekParrotAPI(cfg); err == nil {
			pc := parrot.New(api)
			st.Local = func(ctx context.Context, version string) (string, error) {
				return pc.AwaitRelease(ctx, config.UpdateRepo(), version, nil, parrot.Await{})
			}
		}
	}
	if env.UpdateSource != "" {
		st.Source, st.Local = env.UpdateSource, nil
	}
	tmp, err := os.MkdirTemp(beside, ".viiwork-verify-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	st.Dir = tmp
	if err := st.Stage(ctx, version); err != nil {
		return nil, err
	}
	bin, err := update.Binary(tmp, version)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(bin)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if want := hex.EncodeToString(sum[:]); got != want {
		return nil, fmt.Errorf("%w: %s is not the signed %s release's viiwork (sha256 %s, signed %s)", update.ErrUnverified, staged, version, got, want)
	}
	return data, nil
}

// regularSHA256 is the sha256 of the regular file at p, refusing anything
// else without following a link or blocking on a FIFO: it is in a directory
// a container may write.
func regularSHA256(p string) (string, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", errors.New("not a regular file")
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if fi2, err := f.Stat(); err != nil || !fi2.Mode().IsRegular() || !os.SameFile(fi, fi2) {
		return "", errors.New("changed while it was opened")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxCLI+1))
	if err != nil {
		return "", err
	}
	if n > maxCLI {
		return "", fmt.Errorf("larger than %d bytes", maxCLI)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// peekPort is api.port from a config file, or 0.
func peekPort(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var peek struct {
		API struct {
			Port int `yaml:"port"`
		} `yaml:"api"`
	}
	if yaml.Unmarshal(b, &peek) != nil {
		return 0
	}
	return peek.API.Port
}

// canWrite reports whether a file can be created in dir.
func canWrite(dir string) error {
	f, err := os.CreateTemp(dir, ".viiwork-write-test-")
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return fs.ErrPermission
		}
		return err
	}
	f.Close()
	return os.Remove(f.Name())
}
