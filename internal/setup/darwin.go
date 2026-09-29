package setup

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/janit/viiwork/v2/internal/accept"
	"github.com/janit/viiwork/v2/internal/setup/install"
	"github.com/janit/viiwork/v2/internal/setup/prompt"
	"github.com/janit/viiwork/v2/internal/setup/render"
)

// The Mac half of the wizard: a native node under a user LaunchAgent, with
// llama.cpp fetched at the pin this binary was built against. Everything is
// written under the home directory, so no sudo is needed or wanted.

func (h Host) mac() install.Mac {
	return install.Mac{Root: h.Root, P: install.MacLayout(h.Home), UID: h.UID, Exec: h.Exec, Out: h.Out,
		Executable: h.Executable, Node: install.Node{HTTP: h.HTTP, API: h.NodeAPI}}
}

func preflightMac(ctx context.Context, h Host, configPath string) error {
	m := h.mac()
	if h.GOARCH != "arm64" {
		return errors.New("a Mac node needs Apple Silicon: llama.cpp runs on Metal, which an Intel Mac lacks here")
	}
	if h.Euid == 0 {
		return errors.New("run without sudo: a Mac install lives in your home directory and runs as your user")
	}
	if configPath != m.P.ConfigFile {
		return fmt.Errorf("a Mac install writes %s, which its launch agent reads; run init without --config", m.P.ConfigFile)
	}
	if h.LlamaPin == "" {
		return errors.New("this build does not know its llama.cpp pin; use a release, or build with scripts/gobuild.sh")
	}
	if _, err := os.Stat(h.path(m.P.ManifestFile)); err == nil {
		return fmt.Errorf("an earlier install is recorded in %s: remove it with `viiwork uninstall` first", m.P.ManifestFile)
	}
	for _, f := range []string{m.P.ConfigFile, m.P.Plist} {
		if _, err := os.Stat(h.path(f)); err == nil {
			return fmt.Errorf("%s already exists, and init never overwrites a file it did not create", f)
		}
	}
	// A LaunchAgent needs this user's GUI session: over SSH with nobody
	// logged in at the Mac, bootstrap fails only after everything is written.
	if h.Exec(ctx, io.Discard, "launchctl", "print", fmt.Sprintf("gui/%d", h.UID)) != nil {
		return errors.New("there is no GUI session for your user (an SSH login?): a launch agent runs in it, so log in at the Mac, or run init from its Terminal")
	}
	if m.Loaded(ctx) {
		return fmt.Errorf("the launch agent %s is already loaded (a node set up by hand?); init leaves it alone", install.LaunchAgent)
	}
	for _, pt := range []struct {
		network string
		port    int
	}{{"tcp", 8086}, {"tcp", 7946}, {"udp", 7946}} {
		if !h.PortFree(pt.network, pt.port) {
			return fmt.Errorf("port %d/%s is in use, and a viiwork node needs it", pt.port, pt.network)
		}
	}
	return nil
}

func writeAndStartMac(ctx context.Context, h Host, p prompt.Prompter, s *state) error {
	m := h.mac()
	cfg, err := render.Config(s.answers(), s.secret)
	if err != nil {
		return fmt.Errorf("the generated config was refused: %w", err)
	}
	plist := render.Plist(render.PlistInput{Binary: m.P.BinaryPath, Config: m.P.ConfigFile, Log: m.P.LogFile, Secret: s.secret})
	files := []install.File{
		{Path: m.P.ConfigFile, Mode: 0o644, Data: cfg},
		{Path: m.P.Plist, Mode: 0o600, Data: plist}, // it holds the secret
	}
	llamaDir := strings.TrimSuffix(s.llama, "/llama-"+h.LlamaPin+"/llama-server")

	p.Say("\nThese files will be written:")
	for _, f := range files {
		p.Say("\n── %s (mode %04o)", f.Path, f.Mode)
		text := strings.TrimRight(string(f.Data), "\n")
		if s.secret != nil {
			text = strings.ReplaceAll(text, base64.StdEncoding.EncodeToString(s.secret), "******** (the mesh secret, 32 bytes)")
		}
		p.Say("%s", text)
	}
	if install.Fetched(h.path(m.P.LlamaRoot), h.LlamaPin) {
		p.Say("\n── %s (llama.cpp %s, already here: reused as found, and not claimed by the install)", llamaDir, h.LlamaPin)
	} else {
		check := "the release's sha256"
		if h.LlamaSHA256 != "" {
			check = "the sha256 this build is pinned to"
		}
		p.Say("\n── %s (llama.cpp %s from github.com/ggml-org/llama.cpp, checked against %s)", llamaDir, h.LlamaPin, check)
	}
	p.Say("── %s (a copy of this binary)", m.P.BinaryPath)
	p.Say("── %s and %s (node state and its log)", m.P.StateDir, m.P.LogDir)
	p.Say("── %s (what uninstall will remove)", m.P.ManifestFile)
	ok, err := p.Confirm("Write these files and start the node?", true)
	if err != nil {
		return err
	}
	if !ok {
		return prompt.ErrAbort
	}

	if h.Signals != nil {
		var stop context.CancelFunc
		ctx, stop = h.Signals(ctx)
		defer stop()
	}
	dirs := []string{m.P.ConfigDir, m.P.StateDir, m.P.LogDir, llamaDir}
	if _, err := m.Write(ctx, dirs, files, install.Manifest{InstalledBy: h.Version, ModelDirs: []string{s.modelsDir}}); err != nil {
		return err
	}
	p.Say("Fetching llama.cpp %s …", h.LlamaPin)
	f := install.Fetch{HTTP: h.Download, API: h.LlamaAPI, Releases: h.LlamaRelease, Token: h.GitHubToken, Exec: h.Exec, Out: h.Out}
	reused := install.Fetched(h.path(m.P.LlamaRoot), h.LlamaPin)
	server, verified, err := f.Llama(ctx, h.LlamaPin, h.path(m.P.LlamaRoot), h.LlamaSHA256)
	if err != nil {
		return notStarted(p, h, m, err)
	}
	if server != h.path(s.llama) {
		return notStarted(p, h, m, fmt.Errorf("llama-server is at %s, not at %s as the config says", server, s.llama))
	}
	if !verified && !reused {
		p.Say("Note: the llama.cpp release publishes no sha256, so this build is unverified.")
	}
	if err := m.Bootstrap(ctx); err != nil {
		return notStarted(p, h, m, err)
	}
	if err := m.Node.WaitUp(ctx, h.UpTimeout); err != nil {
		return failedMac(ctx, p, m, err)
	}
	p.Say("The node is up. Waiting for every model to load (a large one takes minutes)…")
	var names []string
	for _, pl := range s.placed {
		names = append(names, pl.Name)
	}
	if err := m.Node.WaitModels(ctx, names, h.ReadyTimeout, h.Poll); err != nil {
		return failedMac(ctx, p, m, err)
	}
	ready := accept.WaitReady(ctx, h.Accept, h.NodeAPI, time.Minute, h.Poll)
	target := fmt.Sprintf("gui/%d/%s", h.UID, install.LaunchAgent)
	notes := []string{
		"Reload after editing " + m.P.ConfigFile + ": launchctl kill HUP " + target,
		"Restart: " + m.Kickstart(),
		"Log: " + m.P.LogFile,
		fmt.Sprintf("Files: %s, %s; node state in %s", m.P.ConfigDir, m.P.BinaryPath, m.P.StateDir),
		"A sleeping Mac leaves the mesh: run `caffeinate -s` while it serves on power.",
		"Power and energy read as unavailable on a Mac (reading them needs root).",
	}
	// ~/.local/bin is not on a Mac's default PATH: without it, the commands
	// above are "command not found".
	if !slices.Contains(filepath.SplitList(h.Path), filepath.Dir(m.P.BinaryPath)) {
		notes = append(notes, "viiwork is in "+filepath.Dir(m.P.BinaryPath)+", which is not on your PATH. Add it once:",
			`  echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.zprofile && . ~/.zprofile`)
	}
	return finish(ctx, h, p, s, ready, h.path(m.P.ConfigFile), "", notes)
}

// notStarted is a failure before the agent was loaded: there is nothing to
// restart or diagnose, only files to undo.
func notStarted(p prompt.Prompter, h Host, m install.Mac, err error) error {
	p.Say("\nSetup stopped: %v", err)
	// launchd loads every plist in LaunchAgents at login: one left here would
	// start a node with no llama-server that still joins the mesh with the
	// secret. The manifest still lists it, so uninstall is unaffected.
	if rerr := os.Remove(h.path(m.P.Plist)); rerr == nil {
		p.Say("The launch agent was removed, so nothing starts at your next login.")
	}
	p.Say("Nothing was started. The files written so far are recorded: run `viiwork uninstall`, then `viiwork init` again.")
	return err
}

func failedMac(ctx context.Context, p prompt.Prompter, m install.Mac, err error) error {
	p.Say("\nThe node did not come up: %v", err)
	m.Diagnose(ctx)
	p.Say("The files are kept. Retry with `%s`, or undo everything with `viiwork uninstall`.", m.Kickstart())
	return err
}
