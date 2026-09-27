package install

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// LaunchAgent is the label of the node's user agent on a Mac.
const LaunchAgent = "fi.viiwork.node"

// MacPaths is where a Mac install puts things: all under the home directory,
// all absolute, since launchd expands neither ~ nor $HOME.
type MacPaths struct {
	ConfigDir, ConfigFile, ManifestFile string
	Plist, BinaryPath, StateDir         string
	LogDir, LogFile, LlamaRoot          string
}

// MacLayout is the Mac install's paths for a home directory.
func MacLayout(home string) MacPaths {
	cfg := filepath.Join(home, ".config", "viiwork")
	logs := filepath.Join(home, "Library", "Logs", "viiwork")
	return MacPaths{
		ConfigDir:    cfg,
		ConfigFile:   filepath.Join(cfg, "viiwork.yaml"),
		ManifestFile: filepath.Join(cfg, "install.json"),
		Plist:        filepath.Join(home, "Library", "LaunchAgents", LaunchAgent+".plist"),
		BinaryPath:   filepath.Join(home, ".local", "bin", "viiwork"),
		StateDir:     filepath.Join(home, ".local", "state", "viiwork"),
		LogDir:       logs,
		LogFile:      filepath.Join(logs, "viiwork.log"),
		LlamaRoot:    filepath.Join(home, ".local", "share", "viiwork", "llama.cpp"),
	}
}

// Mac installs a node that runs natively under a user LaunchAgent.
type Mac struct {
	Root       string // prefixed to every path; "" in production
	P          MacPaths
	UID        int
	Exec       Exec
	Out        io.Writer
	Executable string
	Node       Node
}

func (m Mac) host(p string) string { return filepath.Join(m.Root, p) }

func (m Mac) target() string { return "gui/" + strconv.Itoa(m.UID) + "/" + LaunchAgent }

// Write writes the install manifest-first (see writer.write).
func (m Mac) Write(ctx context.Context, dirs []string, files []File, man Manifest) (Manifest, error) {
	man.LaunchAgent = LaunchAgent
	w := writer{root: m.Root, os: "darwin", manifest: m.P.ManifestFile, binary: m.P.BinaryPath, executable: m.Executable}
	return w.write(ctx, dirs, files, man)
}

// Bootstrap loads the agent, which starts the node.
func (m Mac) Bootstrap(ctx context.Context) error {
	if err := m.Exec(ctx, m.Out, "launchctl", "bootstrap", "gui/"+strconv.Itoa(m.UID), m.host(m.P.Plist)); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w", err)
	}
	return nil
}

// Loaded reports whether the agent is loaded in this user's session.
func (m Mac) Loaded(ctx context.Context) bool {
	return m.Exec(ctx, io.Discard, "launchctl", "print", m.target()) == nil
}

// Kickstart is the command that restarts the node.
func (m Mac) Kickstart() string { return "launchctl kickstart -k " + m.target() }

// Diagnose prints every backend that is not healthy, then the log's last 20
// lines.
func (m Mac) Diagnose(ctx context.Context) {
	m.Node.unhealthy(ctx, m.Out)
	data, err := os.ReadFile(m.host(m.P.LogFile))
	if err != nil {
		fmt.Fprintf(m.Out, "  %s: %v\n", m.P.LogFile, err)
		return
	}
	lines := bytes.SplitAfter(bytes.TrimRight(data, "\n"), []byte("\n"))
	fmt.Fprintf(m.Out, "Last lines of %s:\n", m.P.LogFile)
	for _, l := range lines[max(0, len(lines)-20):] {
		m.Out.Write(l)
	}
	fmt.Fprintln(m.Out)
}
