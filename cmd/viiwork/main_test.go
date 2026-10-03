package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/update"
)

func runWith(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, runEnv{
		stdout:    &stdout,
		stderr:    &stderr,
		lookupEnv: func(string) (string, bool) { return "", false },
		hostname:  func() (string, error) { return "testhost", nil },
	})
	return code, stdout.String(), stderr.String()
}

func runWithEuid(euid int, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, runEnv{stdout: &stdout, stderr: &stderr, euid: func() int { return euid },
		lookupEnv: func(string) (string, bool) { return "", false }})
	return code, stdout.String(), stderr.String()
}

func TestRun(t *testing.T) {
	version = "v2-test"
	if code, out, _ := runWith("--version"); code != 0 || strings.TrimSpace(out) != "v2-test" {
		t.Errorf("E1: exit %d, stdout %q", code, out)
	}
	if code, _, errOut := runWith("--config", "testdata/v1.yaml"); code != 1 || !strings.Contains(errOut, "docs/migrating-to-v2.md") {
		t.Errorf("E2: exit %d, stderr %q", code, errOut)
	}
	if code, _, errOut := runWith("--gpus.count", "4"); code != 2 || !strings.Contains(errOut, "-config") {
		t.Errorf("E3: exit %d, stderr %q", code, errOut)
	}
	if code, _, errOut := runWith("alias", "frobnicate"); code != 2 || !strings.Contains(errOut, "usage: viiwork alias") {
		t.Errorf("E4: exit %d, stderr %q", code, errOut)
	}
	if code, _, errOut := runWith("--config", "missing.yaml"); code != 1 || !strings.Contains(errOut, "reading config") {
		t.Errorf("E5: exit %d, stderr %q", code, errOut)
	}
	if code, _, errOut := runWith("top", "--frobnicate"); code != 2 || !strings.Contains(errOut, "usage: viiwork top") {
		t.Errorf("E6: exit %d, stderr %q", code, errOut)
	}
	if code, out, _ := runWith("--engine-requirements"); code != 0 || !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("E7: exit %d, stdout %q", code, out)
	}
	if code, _, errOut := runWith("update", "--frobnicate"); code != 2 || !strings.Contains(errOut, "usage: viiwork update") {
		t.Errorf("E8: exit %d, stderr %q", code, errOut)
	}
	for _, verb := range []string{"down", "up"} {
		if code, _, errOut := runWith(verb, "--frobnicate"); code != 2 || !strings.Contains(errOut, "usage: viiwork down") {
			t.Errorf("E8 %s: exit %d, stderr %q", verb, code, errOut)
		}
	}
	llamaCppPin = ""
	if code, out, _ := runWith("--build-info"); code != 0 || strings.Contains(out, "llama_cpp") || !strings.Contains(out, `"version":"v2-test"`) {
		t.Errorf("E9 unstamped: exit %d, stdout %q", code, out)
	}
	llamaCppPin, llamaCppMacSHA256 = "b10437", "40e8"
	if code, out, _ := runWith("--build-info"); code != 0 || !strings.Contains(out, `"llama_cpp":"b10437"`) || !strings.Contains(out, `"llama_cpp_macos_sha256":"40e8"`) {
		t.Errorf("E9 stamped: exit %d, stdout %q", code, out)
	}
	llamaCppPin, llamaCppMacSHA256 = "", ""
	if code, _, errOut := runWith("join-code", "--frobnicate"); code != 2 || !strings.Contains(errOut, "usage: viiwork join-code") {
		t.Errorf("E10: exit %d, stderr %q", code, errOut)
	}
	if code, _, errOut := runWithEuid(1000, "engine-sync"); code != 1 || !strings.Contains(errOut, "root") {
		t.Errorf("engine-sync not as root: exit %d, stderr %q", code, errOut)
	}
	if code, _, errOut := runWithEuid(1000, "engine-sync", "--now"); code != 2 || !strings.Contains(errOut, "usage: viiwork engine-sync") {
		t.Errorf("engine-sync with an argument: exit %d, stderr %q", code, errOut)
	}
	if code, _, errOut := runWith("serve"); code != 2 || !strings.Contains(errOut, "serve") {
		t.Errorf("a stray argument: exit %d, stderr %q", code, errOut)
	}
}

func TestHandoverExecsTheStagedRelease(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	rel := update.ReleasesDir(state)
	os.MkdirAll(filepath.Join(rel, "v9.0.0"), 0o755)
	bin := filepath.Join(rel, "v9.0.0", "viiwork")
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	sum := sha256.Sum256([]byte("#!/bin/sh\n"))
	os.WriteFile(filepath.Join(rel, "v9.0.0", "viiwork.sha256"), []byte(hex.EncodeToString(sum[:])+"\n"), 0o644)
	os.WriteFile(filepath.Join(rel, "state.json"), []byte(`{"current":"v9.0.0","last_good":"builtin"}`), 0o644)
	cfg := filepath.Join(dir, "viiwork.yaml")
	os.WriteFile(cfg, []byte("node:\n  name: n\n  state_dir: "+state+"\nmesh:\n  open: true\n  tailnet:\n    enabled: false\n"+
		"gpu:\n  vendor: none\npower:\n  source: none\nupdate:\n  enabled: true\n"), 0o644)

	var gotArgv, gotEnv []string
	var stderr bytes.Buffer
	code := run([]string{"--config", cfg}, runEnv{
		stdout: &bytes.Buffer{}, stderr: &stderr,
		lookupEnv: func(string) (string, bool) { return "", false },
		hostname:  func() (string, error) { return "testhost", nil },
		exec: func(path string, argv, envv []string) error {
			gotArgv, gotEnv = argv, envv
			return errors.New("exec stub")
		},
	})
	if code != 1 || len(gotArgv) != 3 || gotArgv[0] != bin || gotArgv[1] != "--config" || gotArgv[2] != cfg {
		t.Fatalf("exit %d, argv %v, stderr %q", code, gotArgv, stderr.String())
	}
	found := false
	for _, e := range gotEnv {
		found = found || e == update.HandoverEnv+"=1"
	}
	if !found {
		t.Errorf("the staged binary is not told it was handed over: %v", gotEnv)
	}
	st, err := update.LoadState(rel)
	if err != nil || st.Launcher == nil || st.Launcher.Path == "" {
		t.Errorf("launcher not recorded: %+v, %v", st, err)
	}
}

// A stop (docker stop, SIGTERM) that arrives during an update's ordered
// shutdown wins: the node exits instead of exec'ing the launcher, which would
// start the pending release only for it to be killed at the grace timeout —
// burning one of its three starts.
func TestRestartYieldsToAStop(t *testing.T) {
	state := t.TempDir()
	rel := update.ReleasesDir(state)
	self, err := update.SelfLauncher()
	if err != nil {
		t.Fatal(err)
	}
	st := update.NewState()
	st.Launcher = &self
	if err := update.SaveState(rel, st); err != nil {
		t.Fatal(err)
	}
	execs := 0
	env := runEnv{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		exec: func(string, []string, []string) error { execs++; return errors.New("exec stub") }}

	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	if code := restart(stopped, state, nil, env); code != 0 || execs != 0 {
		t.Errorf("stopped: exit %d, execs %d; want 0, 0", code, execs)
	}
	if code := restart(context.Background(), state, nil, env); code != 1 || execs != 1 {
		t.Errorf("running: exit %d, execs %d; want the launcher exec'd", code, execs)
	}
}

// The launcher is older than the release it hands over to, so the config may
// carry keys only that release knows. The handover must not depend on the
// launcher understanding them: it reads state_dir and update.enabled alone,
// and strict validation is left to the binary that actually runs.
func TestHandoverBeforeStrictConfig(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	rel := update.ReleasesDir(state)
	os.MkdirAll(filepath.Join(rel, "v9.0.0"), 0o755)
	bin := filepath.Join(rel, "v9.0.0", "viiwork")
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	sum := sha256.Sum256([]byte("#!/bin/sh\n"))
	os.WriteFile(filepath.Join(rel, "v9.0.0", "viiwork.sha256"), []byte(hex.EncodeToString(sum[:])+"\n"), 0o644)
	os.WriteFile(filepath.Join(rel, "state.json"), []byte(`{"current":"v9.0.0","last_good":"builtin"}`), 0o644)
	cfg := filepath.Join(dir, "viiwork.yaml")
	os.WriteFile(cfg, []byte("node:\n  name: n\n  state_dir: "+state+"\nmesh:\n  open: true\n"+
		"update:\n  enabled: true\n  a_key_from_the_future: 1\nfuture_block:\n  x: 1\n"), 0o644)
	var gotArgv []string
	code := run([]string{"--config", cfg}, runEnv{
		stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		lookupEnv: func(string) (string, bool) { return "", false },
		hostname:  func() (string, error) { return "testhost", nil },
		exec: func(path string, argv, envv []string) error {
			gotArgv = argv
			return errors.New("exec stub")
		},
	})
	if code != 1 || len(gotArgv) == 0 || gotArgv[0] != bin {
		t.Fatalf("exit %d, argv %v: the launcher did not hand over", code, gotArgv)
	}
}

func TestMissingConfigOnATerminalStartsSetup(t *testing.T) {
	var stderr bytes.Buffer
	missing := filepath.Join(t.TempDir(), "viiwork.yaml")
	code := run([]string{"--config", missing}, runEnv{stdout: io.Discard, stderr: &stderr,
		lookupEnv: func(string) (string, bool) { return "", false }, hostname: os.Hostname,
		stdin: strings.NewReader(""), interactive: func() bool { return true }})
	// The wizard starts, then refuses a --config other than the install's.
	if code != 1 || !strings.Contains(stderr.String(), "starting setup") || !strings.Contains(stderr.String(), "--config") {
		t.Errorf("code %d, stderr %q", code, stderr.String())
	}
}

func TestMissingConfigWithoutATerminalIsAnError(t *testing.T) {
	var stderr bytes.Buffer
	missing := filepath.Join(t.TempDir(), "viiwork.yaml")
	code := run([]string{"--config", missing}, runEnv{stdout: io.Discard, stderr: &stderr,
		lookupEnv: func(string) (string, bool) { return "", false }, hostname: os.Hostname,
		interactive: func() bool { return false }})
	if code != 1 || strings.Contains(stderr.String(), "starting setup") || !strings.Contains(stderr.String(), "reading config") {
		t.Errorf("code %d, stderr %q", code, stderr.String())
	}
}

func TestInitUsage(t *testing.T) {
	var stderr bytes.Buffer
	code := run([]string{"init", "--bogus"}, runEnv{stdout: io.Discard, stderr: &stderr})
	if code != 2 || !strings.Contains(stderr.String(), "viiwork init") {
		t.Errorf("code %d, stderr %q", code, stderr.String())
	}
}

// A container without -i and a systemd unit give the process /dev/null, which
// is a character device but no terminal: a missing config there must stay the
// plain error it always was.
func TestDevNullIsNotInteractive(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if interactive(null, null) {
		t.Error("/dev/null counted as a terminal")
	}
}

func TestUninstallUsage(t *testing.T) {
	var stderr bytes.Buffer
	code := run([]string{"uninstall", "--bogus"}, runEnv{stdout: io.Discard, stderr: &stderr})
	if code != 2 || !strings.Contains(stderr.String(), "--delete-models") {
		t.Errorf("code %d, stderr %q", code, stderr.String())
	}
}

// A Mac set up by init keeps its config under the home directory, so a bare
// `viiwork` there must read it rather than start setup again.
func TestDefaultConfigFollowsTheOS(t *testing.T) {
	if got := defaultConfig("darwin", "/Users/u"); got != "/Users/u/.config/viiwork/viiwork.yaml" {
		t.Errorf("darwin: %s", got)
	}
	if got := defaultConfig("linux", "/root"); got != "/etc/viiwork/viiwork.yaml" {
		t.Errorf("linux: %s", got)
	}
}

// The usage lists what each command really takes: init takes no --config (the
// install decides the path), join-code has --open, uninstall --from-config.
func TestUsageMatchesTheCommands(t *testing.T) {
	var stderr bytes.Buffer
	run([]string{"--bogus-flag"}, runEnv{stdout: io.Discard, stderr: &stderr})
	u := stderr.String()
	for _, want := range []string{"viiwork init\n", "join-code [--open]", "--from-config", "down | up [model...]"} {
		if !strings.Contains(u, want) {
			t.Errorf("usage lacks %q:\n%s", want, u)
		}
	}
	if strings.Contains(u, "viiwork init [--config") {
		t.Errorf("usage offers init --config, which preflight refuses:\n%s", u)
	}
}

// SIGHUP's default action ends the process, and systemd counts that as a
// clean exit, so Restart=on-failure would not start the node again: a reload
// in the window before the node's handler existed stopped a native node for
// good. catchHUP is installed first thing; a HUP then waits for the node.
func TestEarlySIGHUPIsHeldForTheNode(t *testing.T) {
	hup := catchHUP()
	defer signal.Stop(hup)
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case <-hup:
	case <-time.After(5 * time.Second):
		t.Fatal("the HUP was not held")
	}
}
