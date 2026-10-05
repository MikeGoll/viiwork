// Package nodectl is `viiwork stop` and `viiwork start`: this machine's node,
// stopped or started through whatever `viiwork init` installed to run it —
// the LaunchAgent on a Mac, the compose project and the engine helper on
// Linux. Like uninstall it goes by the install manifest and never guesses.
package nodectl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/janit/viiwork/v2/internal/setup/install"
)

// Host is the machine the command runs on. Tests replace every field.
type Host struct {
	GOOS    string
	Root    string // prefixed to every path; "" in production
	Home    string
	Euid    int
	UID     int // for launchctl's gui/<uid> domain
	Exec    install.Exec
	HTTP    *http.Client
	NodeAPI string
	Out     io.Writer
	// Wait bounds how long stop waits for the API to go quiet and start
	// waits for it to answer; Poll is the interval between checks.
	Wait, Poll time.Duration
}

func (h Host) path(p string) string { return filepath.Join(h.Root, p) }

func (h Host) exists(p string) bool {
	_, err := os.Stat(h.path(p))
	return err == nil
}

func (h Host) mac() install.MacPaths { return install.MacLayout(h.Home) }

func (h Host) target() string { return "gui/" + strconv.Itoa(h.UID) + "/" + install.LaunchAgent }

func (h Host) run(ctx context.Context, name string, args ...string) error {
	return h.Exec(ctx, h.Out, name, args...)
}

func (h Host) compose(m install.Manifest, a ...string) []string {
	return append([]string{"compose", "-f", h.path(m.Compose.File), "-p", m.Compose.Project}, a...)
}

// helperUnits are the engine helper's units that start it: the path unit and
// the timer. Its service is started by them, never directly.
var helperUnits = []string{install.EnginePath, install.EngineTimer}

// helper reports whether this install has the engine helper's units on disk.
func (h Host) helper(m install.Manifest) bool {
	if h.GOOS != "linux" || m.EngineHelper == nil {
		return false
	}
	for _, u := range install.EngineUnits {
		if h.exists(filepath.Join(install.SystemdDir, u)) {
			return true
		}
	}
	return false
}

func (h Host) manifest(verb string) (install.Manifest, error) {
	if h.GOOS == "darwin" && h.Euid == 0 {
		return install.Manifest{}, fmt.Errorf("run without sudo: a Mac node runs as your user's LaunchAgent")
	}
	if h.GOOS == "linux" && h.Euid != 0 {
		return install.Manifest{}, fmt.Errorf("run as root (sudo viiwork %s): the node runs under Docker and systemd", verb)
	}
	path := install.ManifestFile
	if h.GOOS == "darwin" {
		path = h.mac().ManifestFile
	}
	m, err := install.ReadManifest(h.path(path))
	if errors.Is(err, fs.ErrNotExist) {
		return m, fmt.Errorf("no install manifest at %s: this machine was not set up by `viiwork init`, so %s will not guess how its node runs. "+
			"Use what started it (systemctl %s viiwork, docker compose, launchctl)", path, verb, verb)
	}
	if err != nil {
		return m, err
	}
	if m.LaunchAgent == "" && m.Compose == nil {
		return m, fmt.Errorf("%s records neither a launch agent nor a compose project: nothing to %s", path, verb)
	}
	return m, nil
}

// Stop stops the node and every model it runs. The node leaves the mesh
// first and drains in-flight requests, so members route elsewhere at once.
// It stays stopped until `viiwork start`, or until the next login (Mac) or
// boot (Linux) starts it as usual.
func Stop(ctx context.Context, h Host) error {
	m, err := h.manifest("stop")
	if err != nil {
		return err
	}
	switch {
	case h.GOOS == "darwin" && m.LaunchAgent == install.LaunchAgent:
		if h.Exec(ctx, io.Discard, "launchctl", "print", h.target()) != nil {
			fmt.Fprintln(h.Out, "The node is not running.")
			return nil
		}
		h.stopping()
		// bootout, not kill: KeepAlive would start it again at once.
		if err := h.run(ctx, "launchctl", "bootout", h.target()); err != nil {
			return fmt.Errorf("launchctl bootout %s: %w", h.target(), err)
		}
	case h.GOOS == "linux" && m.Compose != nil:
		h.stopping()
		// The helper goes first: it runs `docker compose up`, and left
		// running it could start the node again.
		if h.helper(m) {
			if err := h.run(ctx, "systemctl", append([]string{"stop"}, append(helperUnits, install.EngineService)...)...); err != nil {
				return fmt.Errorf("could not stop the engine helper (%v): the node was left running", err)
			}
		}
		if !h.exists(m.Compose.File) {
			return fmt.Errorf("%s is missing: nothing was ever started from it", m.Compose.File)
		}
		if err := h.run(ctx, "docker", h.compose(m, "stop", "-t", "90")...); err != nil {
			return fmt.Errorf("docker compose stop: %w; is Docker running?", err)
		}
	default:
		return fmt.Errorf("the install manifest was written on %s, and this is %s", m.OS, h.GOOS)
	}
	if err := h.waitDown(ctx); err != nil {
		return err
	}
	next := "boot"
	if h.GOOS == "darwin" {
		next = "login"
	}
	fmt.Fprintf(h.Out, "Stopped. `viiwork start` starts it again, and so does the next %s.\n", next)
	return nil
}

func (h Host) stopping() {
	fmt.Fprintln(h.Out, "Stopping the node: it leaves the mesh, finishes in-flight requests and stops its models. This can take up to 90 s.")
}

// Start starts a node that `viiwork stop` (or anything else) stopped. It
// returns once the API answers; models load in the background.
func Start(ctx context.Context, h Host) error {
	m, err := h.manifest("start")
	if err != nil {
		return err
	}
	switch {
	case h.GOOS == "darwin" && m.LaunchAgent == install.LaunchAgent:
		if h.Exec(ctx, io.Discard, "launchctl", "print", h.target()) == nil {
			fmt.Fprintf(h.Out, "The node is already running. To restart it: launchctl kickstart -k %s\n", h.target())
			return nil
		}
		if err := h.run(ctx, "launchctl", "bootstrap", "gui/"+strconv.Itoa(h.UID), h.path(h.mac().Plist)); err != nil {
			return fmt.Errorf("launchctl bootstrap: %w", err)
		}
	case h.GOOS == "linux" && m.Compose != nil:
		if !h.exists(m.Compose.File) {
			return fmt.Errorf("%s is missing: run `sudo viiwork init` again", m.Compose.File)
		}
		if err := h.run(ctx, "docker", h.compose(m, "up", "-d")...); err != nil {
			return fmt.Errorf("docker compose up: %w; is Docker running?", err)
		}
		if h.helper(m) {
			if err := h.run(ctx, "systemctl", append([]string{"start"}, helperUnits...)...); err != nil {
				return fmt.Errorf("the node started, but the engine helper did not (%v): sudo systemctl start %s %s", err, helperUnits[0], helperUnits[1])
			}
		}
	default:
		return fmt.Errorf("the install manifest was written on %s, and this is %s", m.OS, h.GOOS)
	}
	node := install.Node{HTTP: h.HTTP, API: h.NodeAPI}
	if err := node.WaitUp(ctx, h.Wait); err != nil {
		return err
	}
	fmt.Fprintln(h.Out, "Started. Its models load in the background: watch them with `viiwork top`.")
	return nil
}

// waitDown waits until the node's API stops answering: launchd and compose
// return once they have signalled the process, not once it has exited.
func (h Host) waitDown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, h.Wait)
	defer cancel()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+h.NodeAPI+"/health", nil)
		if err != nil {
			return err
		}
		resp, err := h.HTTP.Do(req)
		if err == nil {
			resp.Body.Close()
		} else if ctx.Err() == nil {
			return nil // refused: the process is gone
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the node's API on %s still answers after %s: something else runs a node here", h.NodeAPI, h.Wait)
		case <-time.After(h.Poll):
		}
	}
}
