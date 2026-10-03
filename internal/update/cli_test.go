package update

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cliScript is a CLI that reports version.
func cliScript(version string) string { return "#!/bin/sh\necho " + version + "\n" }

func TestInstallCLIKeepsPrev(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "viiwork")
	os.WriteFile(p, []byte("one"), 0o755)
	os.WriteFile(p+".prev", []byte("zero"), 0o755)
	if err := InstallCLI(p, []byte("two")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "two" {
		t.Errorf("cli %q", b)
	}
	if b, _ := os.ReadFile(p + ".prev"); string(b) != "one" {
		t.Errorf("prev %q", b)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o755 {
		t.Errorf("mode %v", fi.Mode())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("left behind: %v", entries)
	}

	// A first install has nothing to keep.
	q := filepath.Join(dir, "fresh")
	if err := InstallCLI(q, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(q + ".prev"); err == nil {
		t.Error("a .prev out of nothing")
	}

	// A link is followed to the file it names, which is replaced in place.
	real := filepath.Join(dir, "real")
	os.WriteFile(real, []byte("old"), 0o755)
	link := filepath.Join(dir, "link")
	os.Symlink(real, link)
	if err := InstallCLI(link, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(real); string(b) != "new" {
		t.Errorf("through the link: %q", b)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the link was replaced by a file")
	}
}

func TestInstallCLIFailureLeavesBoth(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "viiwork")
	os.WriteFile(p, []byte("one"), 0o755)
	os.WriteFile(p+".prev", []byte("zero"), 0o755)
	os.Chmod(dir, 0o555)
	defer os.Chmod(dir, 0o755)
	if os.Geteuid() == 0 {
		t.Skip("root writes a read-only directory")
	}
	if err := InstallCLI(p, []byte("two")); err == nil {
		t.Fatal("installed into a read-only directory")
	}
	if b, _ := os.ReadFile(p); string(b) != "one" {
		t.Errorf("cli %q", b)
	}
	if b, _ := os.ReadFile(p + ".prev"); string(b) != "zero" {
		t.Errorf("prev %q", b)
	}
}

func TestCLIBehind(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o755)
		return p
	}
	for _, c := range []struct {
		name, body string
		behind     bool
	}{
		{"older", cliScript("v2.6.0-beta3"), true},
		{"same", cliScript("v2.7.2"), false},
		{"newer", cliScript("v2.8.0"), false},
		{"dev", cliScript("dev"), false},
		{"broken", "not a program", true},
	} {
		behind, _ := CLIBehind(ctx, write(c.name, c.body), "v2.7.2")
		if behind != c.behind {
			t.Errorf("%s: behind %v", c.name, behind)
		}
	}
	if behind, _ := CLIBehind(ctx, filepath.Join(dir, "missing"), "v2.7.2"); !behind {
		t.Error("a missing CLI is not behind")
	}
}

// On a Mac the CLI is the LaunchAgent's binary, the launcher: following a
// confirmed release installs its staged binary there, records it, and moves
// the recorded launcher's sha256 with it so the next restart execs it.
func TestFollowCLI(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	dir := ReleasesDir(stateDir)
	bin := stage(t, dir, "v2.7.2", cliScript("v2.7.2"), false)
	cliDir := t.TempDir()
	cli := filepath.Join(cliDir, "viiwork")
	os.WriteFile(cli, []byte(cliScript("v2.7.1")), 0o755)
	oldSum, _ := FileSHA256(cli)
	SaveState(dir, State{Current: "v2.7.2", LastGood: "v2.7.2", Launcher: &Launcher{Path: cli, SHA256: oldSum}})
	store := NewStore(dir)
	var logs strings.Builder
	logf := func(f string, a ...any) { logs.WriteString(f + "\n") }

	if err := store.FollowCLI(ctx, "v2.7.2", cli, logf); err != nil {
		t.Fatalf("%v\n%s", err, &logs)
	}
	want, _ := os.ReadFile(bin)
	if b, _ := os.ReadFile(cli); string(b) != string(want) {
		t.Errorf("cli %q", b)
	}
	if b, _ := os.ReadFile(cli + ".prev"); string(b) != cliScript("v2.7.1") {
		t.Errorf("prev %q", b)
	}
	newSum, _ := FileSHA256(cli)
	s, _ := LoadState(dir)
	if s.Launcher == nil || s.Launcher.SHA256 != newSum {
		t.Errorf("launcher not moved: %+v", s.Launcher)
	}
	if s.Current != "v2.7.2" || s.LastGood != "v2.7.2" {
		t.Errorf("state changed: %+v", s)
	}
	var rec cliRecord
	b, _ := os.ReadFile(filepath.Join(dir, cliFile))
	if json.Unmarshal(b, &rec) != nil || rec.Version != "v2.7.2" || rec.SHA256 != newSum {
		t.Errorf("record %s", b)
	}

	// Already there: nothing is written again.
	os.Remove(cli + ".prev")
	if err := store.FollowCLI(ctx, "v2.7.2", cli, logf); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cli + ".prev"); err == nil {
		t.Error("reinstalled a CLI that is the release")
	}

	// A staged binary edited since staging is never installed.
	stage(t, dir, "v2.7.3", cliScript("v2.7.3"), true)
	if err := store.FollowCLI(ctx, "v2.7.3", cli, logf); err == nil {
		t.Error("installed a tampered staged binary")
	}
	if b, _ := os.ReadFile(cli); string(b) != string(want) {
		t.Errorf("cli changed on a failure: %q", b)
	}

	// A launcher elsewhere keeps its record.
	other := &Launcher{Path: "/elsewhere/viiwork", SHA256: strings.Repeat("e", 64)}
	SaveState(dir, State{Current: "v2.7.4", LastGood: "v2.7.4", Launcher: other})
	stage(t, dir, "v2.7.4", cliScript("v2.7.4"), false)
	if err := store.FollowCLI(ctx, "v2.7.4", cli, logf); err != nil {
		t.Fatal(err)
	}
	if s, _ := LoadState(dir); *s.Launcher != *other {
		t.Errorf("another launcher's record changed: %+v", s.Launcher)
	}
}

// A floor the node put there itself by following a release is not an
// out-of-band install: the state still decides, so a rollback below it runs.
func TestStartupFollowedFloor(t *testing.T) {
	stateDir := t.TempDir()
	dir := ReleasesDir(stateDir)
	w := stage(t, dir, "v2.7.1", "W", false)
	stage(t, dir, "v2.7.2", "X", false)
	floor := Launcher{Path: "/Users/u/.local/bin/viiwork", SHA256: strings.Repeat("f", 64)}
	self := func() (Launcher, error) { return floor, nil }
	writeRecord := func(sum string) {
		b, _ := json.Marshal(cliRecord{Version: "v2.7.2", SHA256: sum})
		os.WriteFile(filepath.Join(dir, cliFile), b, 0o644)
	}

	// Rolled back below the followed floor: the rollback runs.
	writeRecord(floor.SHA256)
	SaveState(dir, State{Current: "v2.7.1", LastGood: "v2.7.1", Launcher: &floor})
	exec, _, err := Startup(StartupEnv{StateDir: stateDir, Running: "v2.7.2", Self: self})
	if err != nil || exec != w {
		t.Fatalf("rollback under a followed floor: %q, %v", exec, err)
	}
	if s, _ := LoadState(dir); s.Current != "v2.7.1" {
		t.Errorf("state reset: %+v", s)
	}

	// On the followed release itself: the floor runs, the state is kept.
	SaveState(dir, State{Current: "v2.7.2", LastGood: "v2.7.2", Previous: "v2.7.1", Launcher: &floor})
	exec, _, err = Startup(StartupEnv{StateDir: stateDir, Running: "v2.7.2", Self: self})
	if err != nil || exec != "" {
		t.Fatalf("on the followed release: %q, %v", exec, err)
	}
	if s, _ := LoadState(dir); s.Current != "v2.7.2" || s.LastGood != "v2.7.2" || s.Previous != "v2.7.1" {
		t.Errorf("state reset: %+v", s)
	}

	// A floor that is not the recorded one is out of band: newer wins.
	writeRecord(strings.Repeat("0", 64))
	SaveState(dir, State{Current: "v2.7.1", LastGood: "v2.7.1", Launcher: &floor})
	exec, _, err = Startup(StartupEnv{StateDir: stateDir, Running: "v2.7.2", Self: self})
	if err != nil || exec != "" {
		t.Fatalf("out-of-band floor: %q, %v", exec, err)
	}
	if s, _ := LoadState(dir); s.Current != Builtin {
		t.Errorf("an out-of-band floor did not win: %+v", s)
	}
}
