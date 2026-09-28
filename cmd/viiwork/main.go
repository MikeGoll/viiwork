// Command viiwork is a viiwork 2 node: one process per machine that runs its
// models, joins the mesh and serves the API. `viiwork alias ...` manages the
// mesh's model aliases through a node's API, `viiwork top` watches the mesh,
// and `viiwork init` sets up a machine.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/janit/viiwork/v2/internal/aliascli"
	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/cost"
	"github.com/janit/viiwork/v2/internal/engine"
	"github.com/janit/viiwork/v2/internal/joincode"
	"github.com/janit/viiwork/v2/internal/node"
	"github.com/janit/viiwork/v2/internal/setup"
	"github.com/janit/viiwork/v2/internal/setup/install"
	"github.com/janit/viiwork/v2/internal/setup/prompt"
	"github.com/janit/viiwork/v2/internal/setup/uninstall"
	"github.com/janit/viiwork/v2/internal/top"
	"github.com/janit/viiwork/v2/internal/top/term"
	"github.com/janit/viiwork/v2/internal/update"
	"github.com/janit/viiwork/v2/internal/updatecli"
)

// version is stamped at build time: -ldflags "-X main.version=...".
var version = "dev"

// llamaCppPin is the llama.cpp release this build was cut against
// (docker/pins.env), stamped by scripts/gobuild.sh. Empty when unstamped.
var llamaCppPin = ""

type runEnv struct {
	stdout, stderr io.Writer
	lookupEnv      func(string) (string, bool)
	hostname       func() (string, error)
	exec           func(path string, argv, envv []string) error
	stdin          io.Reader
	// interactive reports whether stdin is a terminal: only then does a
	// missing config start the setup wizard. nil means no.
	interactive func() bool
}

func main() {
	os.Exit(run(os.Args[1:], runEnv{stdout: os.Stdout, stderr: os.Stderr, lookupEnv: os.LookupEnv, hostname: os.Hostname, exec: syscall.Exec,
		stdin: os.Stdin, interactive: func() bool { return interactive(os.Stdin, os.Stdout) }}))
}

// interactive reports whether a person is at the terminal: only then may a
// missing config start the setup wizard.
func interactive(in, out *os.File) bool {
	return term.IsTerminal(in) && term.IsTerminal(out)
}

// run is the whole command; it returns the exit code.
func run(args []string, env runEnv) int {
	if len(args) > 0 && args[0] == "alias" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		return aliascli.Run(ctx, args[1:], aliascli.Env{
			Stdout: env.stdout, Stderr: env.stderr, LookupEnv: env.lookupEnv,
			Hostname: env.hostname, Client: &http.Client{}, ReadFile: os.ReadFile,
		})
	}

	if len(args) > 0 && args[0] == "top" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		return top.Run(ctx, args[1:], top.Env{
			Stdout: env.stdout, Stderr: env.stderr, Client: &http.Client{},
			ReadFile: os.ReadFile, LookupEnv: env.lookupEnv, Now: time.Now,
			Interactive: func() bool { return term.IsTerminal(os.Stdin) && term.IsTerminal(os.Stdout) },
			OpenScreen:  func() (top.Screen, error) { return term.Open(os.Stdin, os.Stdout) },
			Keys:        os.Stdin,
		})
	}
	if len(args) > 0 && args[0] == "update" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		return updatecli.Run(ctx, args[1:], updatecli.Env{
			Stdout: env.stdout, Stderr: env.stderr, LookupEnv: env.lookupEnv, Hostname: env.hostname,
			Plist: launchAgentPlist(),
		})
	}
	if len(args) > 0 && args[0] == "join-code" {
		return joincode.Run(context.Background(), args[1:], joincode.Env{
			Stdout: env.stdout, Stderr: env.stderr, LookupEnv: env.lookupEnv,
			Client: &http.Client{}, ReadFile: os.ReadFile, Plist: launchAgentPlist(),
		})
	}

	if len(args) > 0 && args[0] == "uninstall" {
		in := env.stdin
		if in == nil {
			in = os.Stdin
		}
		return uninstall.Main(context.Background(), args[1:], uninstall.Env{Stdin: in, Stdout: env.stdout, Stderr: env.stderr})
	}
	if len(args) > 0 && args[0] == "init" {
		fs := flag.NewFlagSet("init", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		configPath := fs.String("config", osConfigPath(), "")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() > 0 {
			fmt.Fprint(env.stderr, "usage: viiwork init [--config path]\n\nAsks a few questions, shows every file it will write, and on a yes writes them and starts the node.\n")
			return 2
		}
		return runInit(*configPath, env)
	}

	// The config file is the only input: v1's --section.key overrides are gone
	// (P6 Decision 4), so a v1 invocation fails here, loudly.
	fs := flag.NewFlagSet("viiwork", flag.ContinueOnError)
	fs.SetOutput(env.stderr)
	configPath := fs.String("config", osConfigPath(), "path to viiwork.yaml")
	showVersion := fs.Bool("version", false, "print the version and exit")
	engineReqs := fs.Bool("engine-requirements", false, "print the minimum engine versions this binary needs, as JSON, and exit")
	buildInfo := fs.Bool("build-info", false, "print the version and the llama.cpp pin as JSON, and exit")
	fs.Usage = func() {
		fmt.Fprintf(env.stderr, "usage: viiwork [--config path] [--version]\n       viiwork alias <command> ...\n       viiwork top [--node host:port] [--host name] [--once]\n       viiwork update [status|rollback] ...\n       viiwork join-code [--open] [--config path]\n       viiwork init\n       viiwork uninstall [--yes] [--delete-models] [--keep-images] [--from-config]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(env.stderr, "viiwork: unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return 2
	}
	if *showVersion {
		fmt.Fprintln(env.stdout, version)
		return 0
	}
	if *engineReqs {
		b, _ := json.Marshal(engine.Requirements())
		fmt.Fprintln(env.stdout, string(b))
		return 0
	}
	if *buildInfo {
		info := map[string]string{"version": version}
		if llamaCppPin != "" {
			info["llama_cpp"] = llamaCppPin
		}
		b, _ := json.Marshal(info)
		fmt.Fprintln(env.stdout, string(b))
		return 0
	}

	// Catch SIGHUP before anything slow: its default action ends the process,
	// which systemd's Restart=on-failure treats as a clean stop. A HUP during
	// startup waits here and reloads once the node is up.
	hup := catchHUP()
	defer signal.Stop(hup)

	if _, err := os.Stat(*configPath); os.IsNotExist(err) && env.interactive != nil && env.interactive() {
		fmt.Fprintf(env.stderr, "No config at %s: starting setup (viiwork init).\n\n", *configPath)
		return runInit(*configPath, env)
	}

	// The handover comes before the strict load: the release this launcher
	// hands over to may understand config keys this binary does not.
	if stateDir, enabled, err := config.PeekUpdate(*configPath); err == nil && enabled {
		if code, done := handover(stateDir, args, env); done {
			return code
		}
	}
	cost.LoadDotEnv(".env")
	cfg, err := config.Load(*configPath, env.lookupEnv)
	if err != nil {
		// A v1 file's error already names docs/migrating-to-v2.md.
		fmt.Fprintf(env.stderr, "viiwork: %v\n", err)
		return 1
	}
	n, err := node.New(cfg, node.Options{
		ConfigPath: *configPath, Version: version, Log: env.stdout,
		LookupEnv: env.lookupEnv, Hostname: env.hostname,
	})
	if err != nil {
		fmt.Fprintf(env.stderr, "viiwork: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				_ = n.Reload() // errors are logged by Reload
			}
		}
	}()
	err = n.Run(ctx)
	if errors.Is(err, node.ErrRestart) {
		return restart(ctx, cfg.Node.StateDir, args, env)
	}
	if err != nil {
		fmt.Fprintf(env.stderr, "viiwork: %v\n", err)
		return 1
	}
	return 0
}

// restart follows an update's ordered shutdown: start again at the launcher,
// which reads state.json and runs whatever it now names. A stop that arrived
// meanwhile (ctx ended: SIGTERM, docker stop) wins — exec'ing would start the
// pending release only for it to be killed at the grace timeout, burning one
// of its starts.
func restart(ctx context.Context, stateDir string, args []string, env runEnv) int {
	if ctx.Err() != nil {
		fmt.Fprintln(env.stderr, "viiwork: update: stopped during the restart; exiting instead")
		return 0
	}
	target, err := update.RestartTarget(update.ReleasesDir(stateDir))
	if err != nil {
		fmt.Fprintf(env.stderr, "viiwork: update: %v\n", err)
		return 1
	}
	xerr := env.exec(target, append([]string{target}, args...), os.Environ())
	fmt.Fprintf(env.stderr, "viiwork: update: exec %s: %v\n", target, xerr)
	return 1
}

// handover is the update launcher. Before the node is built, a node that takes
// part in updates runs the release its state names, exec'ing it in place so
// Docker, launchd and pid: host see one process.
func handover(stateDir string, args []string, env runEnv) (int, bool) {
	_, handedOver := env.lookupEnv(update.HandoverEnv)
	// A later restart must start at the launcher again, so the variable does
	// not outlive this check.
	os.Unsetenv(update.HandoverEnv)
	target, logs, err := update.Startup(update.StartupEnv{
		StateDir: stateDir, Running: version, HandedOver: handedOver, Self: update.SelfLauncher,
	})
	for _, l := range logs {
		fmt.Fprintf(env.stderr, "viiwork: update: %s\n", l)
	}
	if err != nil {
		fmt.Fprintf(env.stderr, "viiwork: update: %v\n", err)
		return 1, true
	}
	if target == "" {
		return 0, false
	}
	xerr := env.exec(target, append([]string{target}, args...), append(os.Environ(), update.HandoverEnv+"=1"))
	fmt.Fprintf(env.stderr, "viiwork: update: exec %s: %v\n", target, xerr)
	return 1, true
}

// runInit is the setup wizard. Its prompts read stdin, and nothing is written
// before its last confirmation.
func runInit(configPath string, env runEnv) int {
	in := env.stdin
	if in == nil {
		in = os.Stdin
	}
	err := setup.Run(context.Background(), setup.DefaultHost(version, llamaCppPin, env.stdout), prompt.NewLine(in, env.stdout), configPath)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, prompt.ErrAbort):
		fmt.Fprintln(env.stderr, "\nviiwork init: aborted")
		return 1
	default:
		fmt.Fprintf(env.stderr, "viiwork init: %v\n", err)
		return 1
	}
}

// defaultConfig is the config a node reads, and init writes, when --config is
// not given: /etc/viiwork on Linux, the user's ~/.config/viiwork on a Mac.
func defaultConfig(goos, home string) string { return setup.DefaultConfigPath(goos, home) }

func osConfigPath() string {
	home, _ := os.UserHomeDir()
	return defaultConfig(runtime.GOOS, home)
}

// launchAgentPlist is where a Mac install keeps the mesh secret, for
// join-code; "" elsewhere.
func launchAgentPlist() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	home, _ := os.UserHomeDir()
	return install.MacLayout(home).Plist
}

// catchHUP holds SIGHUP for the node's reload loop.
func catchHUP() chan os.Signal {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	return hup
}
