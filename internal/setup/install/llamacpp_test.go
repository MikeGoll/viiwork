package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type entry struct {
	name, link string
	typ        byte
	body       string
}

func tarball(t *testing.T, entries []entry) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Linkname: e.link, Mode: 0o755, Size: int64(len(e.body))}
		if e.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func goodBuild() []entry {
	return []entry{
		{name: "llama-b1/", typ: tar.TypeDir},
		{name: "llama-b1/llama-server", typ: tar.TypeReg, body: "#!server"},
		{name: "llama-b1/libllama.0.1.0.dylib", typ: tar.TypeReg, body: "lib"},
		{name: "llama-b1/libllama.dylib", typ: tar.TypeSymlink, link: "libllama.0.1.0.dylib"},
	}
}

// release serves the GitHub release JSON and the asset. digest "" omits the
// field; "wrong" publishes a digest that does not match.
func release(t *testing.T, archive []byte, digest string) (*httptest.Server, *atomic.Int32) {
	var hits atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/tags/b1":
			asset := map[string]string{"name": "llama-b1-bin-macos-arm64.tar.gz", "browser_download_url": srv.URL + "/dl/asset.tgz"}
			sum := sha256.Sum256(archive)
			switch digest {
			case "right":
				asset["digest"] = "sha256:" + hex.EncodeToString(sum[:])
			case "wrong":
				asset["digest"] = "sha256:" + strings.Repeat("0", 64)
			}
			json.NewEncoder(w).Encode(map[string]any{"assets": []any{asset}})
		case "/dl/asset.tgz":
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func fetcher(srv *httptest.Server) (Fetch, *[]string) {
	var calls []string
	return Fetch{HTTP: srv.Client(), API: srv.URL + "/tags/", Out: io.Discard,
		Exec: func(_ context.Context, _ io.Writer, name string, args ...string) error {
			calls = append(calls, name+" "+strings.Join(args, " "))
			return nil
		}}, &calls
}

func TestFetchVerifiesAndExtracts(t *testing.T) {
	srv, _ := release(t, tarball(t, goodBuild()), "right")
	f, calls := fetcher(srv)
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "b1"), 0o755) // the empty dir Write leaves
	server, verified, err := f.Llama(context.Background(), "b1", root)
	if err != nil {
		t.Fatal(err)
	}
	if server != LlamaServerPath(root, "b1") || !verified {
		t.Errorf("server %s verified %v", server, verified)
	}
	if b, err := os.ReadFile(filepath.Join(root, "b1", "llama-b1", "libllama.dylib")); err != nil || string(b) != "lib" {
		t.Errorf("symlink: %q %v", b, err)
	}
	if len(*calls) != 1 || !strings.HasPrefix((*calls)[0], "xattr -dr com.apple.quarantine ") {
		t.Errorf("calls %q", *calls)
	}
	if left, _ := filepath.Glob(filepath.Join(root, ".fetch-*")); len(left) != 0 {
		t.Errorf("temporary files left: %q", left)
	}
}

func TestFetchReusesAnExistingBuild(t *testing.T) {
	srv, hits := release(t, nil, "right")
	f, _ := fetcher(srv)
	root := t.TempDir()
	server := LlamaServerPath(root, "b1")
	os.MkdirAll(filepath.Dir(server), 0o755)
	os.WriteFile(server, []byte("#!server"), 0o755)
	got, verified, err := f.Llama(context.Background(), "b1", root)
	if err != nil || got != server || hits.Load() != 0 || verified {
		t.Errorf("got %s, %v, %d requests", got, err, hits.Load())
	}
}

func TestFetchRefusesADigestMismatch(t *testing.T) {
	srv, _ := release(t, tarball(t, goodBuild()), "wrong")
	f, _ := fetcher(srv)
	root := t.TempDir()
	_, _, err := f.Llama(context.Background(), "b1", root)
	if err == nil || !strings.Contains(err.Error(), "/dl/asset.tgz") {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "b1")); err == nil {
		t.Error("something was left at the destination")
	}
}

func TestFetchRefusesEscapes(t *testing.T) {
	cases := map[string]entry{
		"dotdot":        {name: "llama-b1/../evil", typ: tar.TypeReg, body: "x"},
		"absolute":      {name: "/abs", typ: tar.TypeReg, body: "x"},
		"absolute link": {name: "llama-b1/l", typ: tar.TypeSymlink, link: "/etc/passwd"},
		"escaping link": {name: "llama-b1/l", typ: tar.TypeSymlink, link: "../../x"},
		"fifo":          {name: "llama-b1/f", typ: tar.TypeFifo},
		"hard link":     {name: "llama-b1/h", typ: tar.TypeLink, link: "llama-b1/llama-server"},
	}
	for name, bad := range cases {
		srv, _ := release(t, tarball(t, append(goodBuild(), bad)), "right")
		f, _ := fetcher(srv)
		root := t.TempDir()
		if _, _, err := f.Llama(context.Background(), "b1", root); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := os.Stat(filepath.Join(root, "b1")); err == nil {
			t.Errorf("%s: something was left at the destination", name)
		}
	}
}

func TestFetchWithoutDigestIsUnverified(t *testing.T) {
	srv, _ := release(t, tarball(t, goodBuild()), "")
	f, _ := fetcher(srv)
	_, verified, err := f.Llama(context.Background(), "b1", t.TempDir())
	if err != nil || verified {
		t.Errorf("verified %v, %v", verified, err)
	}
}

func TestFetchMissingAsset(t *testing.T) {
	srv, _ := release(t, nil, "right")
	f, _ := fetcher(srv)
	_, _, err := f.Llama(context.Background(), "b2", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "llama-b2-bin-macos-arm64.tar.gz") {
		t.Errorf("err %v", err)
	}
}

// Symlinks that each look inside when read as text can chain out on disk:
// a -> . makes a/a/e the same file as e, whose target then climbs two levels.
func TestFetchRefusesAChainedSymlinkEscape(t *testing.T) {
	chain := []entry{
		{name: "a", typ: tar.TypeSymlink, link: "."},
		{name: "a/a/e", typ: tar.TypeSymlink, link: "../../outside"},
		{name: "e/evil", typ: tar.TypeReg, body: "x"},
	}
	srv, _ := release(t, tarball(t, append(goodBuild(), chain...)), "right")
	f, _ := fetcher(srv)
	root := t.TempDir()
	// The escape lands in a directory that exists, as ~/Library/LaunchAgents
	// would on a real Mac.
	os.MkdirAll(filepath.Join(filepath.Dir(root), "outside"), 0o755)
	if _, _, err := f.Llama(context.Background(), "b1", root); err == nil {
		t.Error("accepted")
	}
	for _, p := range []string{filepath.Join(filepath.Dir(root), "outside", "evil"), filepath.Join(root, "outside", "evil"), filepath.Join(root, "b1")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s exists", p)
		}
	}
}

// A download that stops sending is abandoned, not waited on until Ctrl-C.
func TestFetchGivesUpOnAStalledDownload(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tags/b1":
			json.NewEncoder(w).Encode(map[string]any{"assets": []any{map[string]string{
				"name": "llama-b1-bin-macos-arm64.tar.gz", "browser_download_url": srv.URL + "/dl"}}})
		case "/dl":
			w.Header().Set("Content-Length", "1000")
			w.Write([]byte("partial"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}
	}))
	defer srv.Close()
	f, _ := fetcher(srv)
	f.Stall = 100 * time.Millisecond
	start := time.Now()
	_, _, err := f.Llama(context.Background(), "b1", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Errorf("err %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %v", d)
	}
}

// A 404 is an answer, not a network hiccup: it is not retried.
func TestFetchDoesNotRetryAClientError(t *testing.T) {
	var hits atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tags/b1" {
			json.NewEncoder(w).Encode(map[string]any{"assets": []any{map[string]string{
				"name": "llama-b1-bin-macos-arm64.tar.gz", "browser_download_url": srv.URL + "/gone"}}})
			return
		}
		hits.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()
	f, _ := fetcher(srv)
	if _, _, err := f.Llama(context.Background(), "b1", t.TempDir()); err == nil || hits.Load() != 1 {
		t.Errorf("err %v after %d requests", err, hits.Load())
	}
}
