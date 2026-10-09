// Package updatetest builds signed fake releases for tests.
package updatetest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/janit/viiwork/v2/internal/release"
)

// Opts shapes a fake release.
type Opts struct {
	Reports  string // --version output; "" = the version
	Requires string // --engine-requirements JSON; "" = {}
	Shebang  string // "" = #!/bin/sh
	OtherKey bool   // sign with a key the node does not trust
	Tamper   bool   // change the archive after SHA256SUMS is written
	Unsigned bool   // no SHA256SUMS.sig
	Info     string // --build-info JSON; "" = an older binary without the flag
}

// Assets builds a signed release of a shell-script "viiwork" for this host's
// target: asset name → bytes, and the public key that verifies it (unless
// OtherKey).
func Assets(t testing.TB, version string, o Opts) (map[string][]byte, ed25519.PublicKey) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := priv
	if o.OtherKey {
		_, signer, _ = ed25519.GenerateKey(rand.Reader)
	}
	reports, requires, shebang := o.Reports, o.Requires, o.Shebang
	if reports == "" {
		reports = version
	}
	if requires == "" {
		requires = "{}"
	}
	if shebang == "" {
		shebang = "#!/bin/sh"
	}
	info := "exit 2"
	if o.Info != "" {
		info = "echo '" + o.Info + "'"
	}
	script := fmt.Sprintf("%s\ncase \"$1\" in\n--version) echo %s ;;\n--engine-requirements) echo '%s' ;;\n--build-info) %s ;;\nesac\n", shebang, reports, requires, info)
	target := release.Target{OS: runtime.GOOS, Arch: runtime.GOARCH}
	name := release.ArchiveName(version, target)
	top := strings.TrimSuffix(name, ".tar.gz")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: top + "/", Typeflag: tar.TypeDir, Mode: 0o755})
	for n, body := range map[string]string{"viiwork": script, "viiwork-accept": script, "LICENSE": "MIT"} {
		tw.WriteHeader(&tar.Header{Name: top + "/" + n, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	archive := buf.Bytes()
	var sums strings.Builder
	for _, tg := range release.Targets {
		n := release.ArchiveName(version, tg)
		sum := sha256.Sum256([]byte(n)) // other targets: any digest will do
		if tg == target {
			sum = sha256.Sum256(archive)
		}
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), n)
	}
	if o.Tamper {
		archive = append([]byte{}, archive...)
		archive[len(archive)/2] ^= 0xff
	}
	assets := map[string][]byte{name: archive, "SHA256SUMS": []byte(sums.String())}
	if !o.Unsigned {
		assets["SHA256SUMS.sig"] = release.Sign(signer, version, []byte(sums.String()))
	}
	return assets, pub
}

// WriteDir writes assets into a new directory and returns it.
func WriteDir(t testing.TB, assets map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	for name, b := range assets {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// Hits counts requests per asset name.
type Hits struct {
	mu sync.Mutex
	m  map[string]int
}

func (h *Hits) add(asset string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.m == nil {
		h.m = map[string]int{}
	}
	h.m[asset]++
}

// Get is how often asset was requested.
func (h *Hits) Get(asset string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.m[asset]
}

// Total is how many requests were made.
func (h *Hits) Total() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.m {
		n += c
	}
	return n
}

// Serve serves assets at <url>/<version>/<asset>.
func Serve(t testing.TB, version string, assets map[string][]byte) (string, *Hits) {
	t.Helper()
	hits := &Hits{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asset := strings.TrimPrefix(r.URL.Path, "/"+version+"/")
		hits.add(asset)
		b, ok := assets[asset]
		if !ok || !strings.HasPrefix(r.URL.Path, "/"+version+"/") {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, hits
}
