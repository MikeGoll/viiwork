package mesh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
)

// errLocalAPIUnreachable marks a status request that never got an answer, as
// opposed to a daemon that answered with an error.
var errLocalAPIUnreachable = errors.New("tailscaled LocalAPI unreachable")

// errNoStatusFallback is what a platform with nowhere else to look returns.
var errNoStatusFallback = errors.New("no other tailscaled status source on this platform")

// statusFallback is where the status document comes from when the default
// socket is not there; per platform, and replaced in tests.
var statusFallback = tailnetStatusFallback

// readStatusDocument returns tailscaled's ipnstate.Status document. Only when
// socket is the default and nothing answers on it does statusFallback run:
// a configured socket is never second-guessed, and a daemon that answered,
// even with an error, is not swapped for another.
func readStatusDocument(ctx context.Context, socket string, isDefault bool) ([]byte, error) {
	body, err := fetchLocalAPIStatus(ctx, socket)
	if err == nil || !isDefault || !errors.Is(err, errLocalAPIUnreachable) {
		return body, err
	}
	body, fbErr := statusFallback(ctx)
	switch {
	case fbErr == nil:
		return body, nil
	case errors.Is(fbErr, errNoStatusFallback):
		return nil, err
	}
	return nil, fmt.Errorf("%w; %v", err, fbErr)
}

// fetchLocalAPIStatus asks tailscaled's LocalAPI over its unix socket. This
// keeps Tailscale's Go module out of the build. The request is bounded by ctx
// and its connection closed after use.
func fetchLocalAPIStatus(ctx context.Context, socket string) ([]byte, error) {
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		DisableKeepAlives: true,
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, localAPIStatusURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w at %s: %w", errLocalAPIUnreachable, socket, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxStatusBody))
	if err != nil {
		return nil, fmt.Errorf("tailscaled LocalAPI: reading status: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tailscaled LocalAPI answered HTTP %d: %s", resp.StatusCode, truncate(body, 200))
	}
	return body, nil
}

const (
	// darwinBrewSocket is where the Homebrew (open source) tailscaled listens.
	darwinBrewSocket = "/var/run/tailscaled.socket"
	// darwinAppCLI is the CLI inside the App Store and Standalone app bundles.
	// The App Store build is sandboxed: it has no socket at all and puts
	// nothing on PATH, so this is the only way to reach it.
	darwinAppCLI = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"
)

// darwinStatusSource is the macOS fallback chain, untagged so it is tested on
// every platform. `tailscale status --json` prints the same ipnstate.Status
// the LocalAPI returns, so the decoders are shared.
type darwinStatusSource struct {
	fetch    func(ctx context.Context, socket string) ([]byte, error)
	lookPath func(file string) (string, error)
	run      func(ctx context.Context, name string, args ...string) ([]byte, error)
}

func (s darwinStatusSource) read(ctx context.Context) ([]byte, error) {
	body, err := s.fetch(ctx, darwinBrewSocket)
	if err == nil {
		return body, nil
	}
	cli := darwinAppCLI
	if p, lookErr := s.lookPath("tailscale"); lookErr == nil {
		cli = p
	}
	body, cliErr := s.run(ctx, cli, "status", "--json")
	if cliErr != nil {
		return nil, fmt.Errorf("%v; %s status --json: %w", err, cli, cliErr)
	}
	if len(body) > maxStatusBody {
		return nil, fmt.Errorf("%s status --json: output over %d bytes", cli, maxStatusBody)
	}
	return body, nil
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}
