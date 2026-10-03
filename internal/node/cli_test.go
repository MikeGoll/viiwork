package node

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/janit/viiwork/v2/internal/setup/install"
)

// Only a Mac install's recorded CLI follows the release from inside the
// node; on Linux the node never writes the host.
func TestInstalledCLI(t *testing.T) {
	dir := t.TempDir()
	write := func(m install.Manifest) string {
		p := filepath.Join(dir, m.OS+".json")
		b, _ := json.Marshal(m)
		os.WriteFile(p, b, 0o644)
		return p
	}
	mac := write(install.Manifest{Version: 1, OS: "darwin", Binary: "/Users/u/.local/bin/viiwork"})
	linux := write(install.Manifest{Version: 1, OS: "linux", Binary: "/usr/local/bin/viiwork"})
	for _, c := range []struct{ manifest, want string }{
		{mac, "/Users/u/.local/bin/viiwork"},
		{linux, ""},
		{"", ""},
		{filepath.Join(dir, "missing.json"), ""},
	} {
		n := &Node{o: Options{InstallManifest: c.manifest}}
		if got := n.installedCLI(); got != c.want {
			t.Errorf("%s: %q", c.manifest, got)
		}
	}
}
