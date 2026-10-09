package install

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"time"
)

// Linux installs a node that runs in Docker.
type Linux struct {
	Root       string // prefixed to every path; "" in production
	Exec       Exec
	Out        io.Writer
	Executable string // the running binary, copied to BinaryPath
	HTTP       *http.Client
	NodeAPI    string // the node's API on loopback

	beforeFile func(path string) // tests: called before each file is written
}

func (l Linux) host(p string) string { return filepath.Join(l.Root, p) }

func (l Linux) node() Node { return Node{HTTP: l.HTTP, API: l.NodeAPI} }

// ComposeArgs is `docker` plus these arguments for this install's project.
func (l Linux) ComposeArgs(a ...string) []string {
	return append([]string{"compose", "-f", l.host(ComposeFile), "-p", Project}, a...)
}

// Write writes the install manifest-first (see writer.write), with the
// binary at BinaryPath and the manifest at ManifestFile.
func (l Linux) Write(ctx context.Context, dirs []string, files []File, m Manifest) (Manifest, error) {
	w := writer{root: l.Root, os: "linux", manifest: ManifestFile, binary: BinaryPath, executable: l.Executable, beforeFile: l.beforeFile}
	return w.write(ctx, dirs, files, m)
}

// Start pulls the image with its progress shown, then brings the project up.
func (l Linux) Start(ctx context.Context, image string) error {
	fmt.Fprintf(l.Out, "Pulling %s …\n", image)
	if err := l.Exec(ctx, l.Out, "docker", "pull", image); err != nil {
		return fmt.Errorf("docker pull %s: %w", image, err)
	}
	if err := l.Exec(ctx, l.Out, "docker", l.ComposeArgs("up", "-d")...); err != nil {
		return fmt.Errorf("docker compose up: %w", err)
	}
	return nil
}

// Diagnose prints every backend that is not healthy, then the container's
// last log lines.
func (l Linux) Diagnose(ctx context.Context) {
	l.node().unhealthy(ctx, l.Out)
	fmt.Fprintln(l.Out, "Last log lines:")
	l.Exec(ctx, l.Out, "docker", l.ComposeArgs("logs", "--tail", "20")...)
}

// WaitUp is Node.WaitUp on this install's node.
func (l Linux) WaitUp(ctx context.Context, timeout time.Duration) error {
	return l.node().WaitUp(ctx, timeout)
}

// WaitModels is Node.WaitModels on this install's node.
func (l Linux) WaitModels(ctx context.Context, names []string, timeout, poll time.Duration) error {
	return l.node().WaitModels(ctx, names, timeout, poll)
}
