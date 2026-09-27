package install

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// LlamaReleaseAPI is where a release's assets and their digests are listed.
const LlamaReleaseAPI = "https://api.github.com/repos/ggml-org/llama.cpp/releases/tags/"

// Fetch downloads llama.cpp's macOS build, as scripts/macos/fetch-llama.sh
// does by hand.
type Fetch struct {
	HTTP *http.Client
	API  string // LlamaReleaseAPI in production
	Exec Exec
	Out  io.Writer
	// Stall abandons a request that receives nothing for this long; 0 means
	// defaultStall. A slow download is fine; a silent one is not.
	Stall time.Duration
}

const defaultStall = time.Minute

// permanent marks a failure a retry cannot fix, such as a 404.
type permanent struct{ error }

// LlamaServerPath is where the build of tag puts llama-server under root:
// the release tarball holds one directory, llama-<tag>.
func LlamaServerPath(root, tag string) string {
	return filepath.Join(root, tag, "llama-"+tag, "llama-server")
}

// Fetched reports whether root already holds an executable llama-server for
// tag, which Llama then reuses without a download.
func Fetched(root, tag string) bool {
	fi, err := os.Stat(LlamaServerPath(root, tag))
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o100 != 0
}

func asset(tag string) string { return "llama-" + tag + "-bin-macos-arm64.tar.gz" }

// Llama makes llama-server for tag available under root/tag and returns its
// path. A build already there is used as it is. Otherwise the release asset
// is downloaded, checked against the digest the release publishes (verified
// is false when it publishes none), unpacked into a temporary directory,
// cleared of the quarantine attribute and renamed into place. On any failure
// nothing is left at root/tag beyond what was there.
func (f Fetch) Llama(ctx context.Context, tag, root string) (server string, verified bool, err error) {
	server = LlamaServerPath(root, tag)
	if Fetched(root, tag) {
		return server, false, nil // reused as found: nothing here checked it
	}
	name := asset(tag)
	url, digest, err := f.find(ctx, tag, name)
	if err != nil {
		return "", false, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", false, err
	}
	archive, err := os.CreateTemp(root, ".fetch-*.tar.gz")
	if err != nil {
		return "", false, err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	sum, err := f.download(ctx, url, archive)
	if err != nil {
		return "", false, fmt.Errorf("downloading %s: %w", url, err)
	}
	if digest != "" {
		if !strings.EqualFold(sum, strings.TrimPrefix(digest, "sha256:")) {
			return "", false, fmt.Errorf("%s: sha256 %s, but the release publishes %s", url, sum, digest)
		}
		verified = true
	}
	tmp, err := os.MkdirTemp(root, ".fetch-*")
	if err != nil {
		return "", false, err
	}
	defer os.RemoveAll(tmp) // a no-op once renamed into place
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return "", false, err
	}
	if err := extract(archive, tmp); err != nil {
		return "", false, fmt.Errorf("unpacking %s: %w", name, err)
	}
	// A downloaded binary carries the quarantine attribute, and Gatekeeper
	// refuses to start an unsigned one that has it.
	f.Exec(ctx, io.Discard, "xattr", "-dr", "com.apple.quarantine", tmp)
	dest := filepath.Join(root, tag)
	os.Remove(dest) // the empty directory the install's Write created, if any
	if err := os.Rename(tmp, dest); err != nil {
		return "", false, err
	}
	if _, err := os.Stat(server); err != nil {
		return "", false, fmt.Errorf("%s holds no %s", name, strings.TrimPrefix(server, dest+string(filepath.Separator)))
	}
	return server, verified, nil
}

// find reads the release's asset list for the download URL and digest.
func (f Fetch) find(ctx context.Context, tag, name string) (url, digest string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.API+tag, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("llama.cpp release %s (for %s): %w", tag, name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("llama.cpp release %s (for %s): %s", tag, name, resp.Status)
	}
	var rel struct {
		Assets []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&rel); err != nil {
		return "", "", fmt.Errorf("llama.cpp release %s: %w", tag, err)
	}
	for _, a := range rel.Assets {
		if a.Name == name {
			return a.URL, a.Digest, nil
		}
	}
	return "", "", fmt.Errorf("llama.cpp release %s has no %s", tag, name)
}

// download writes url into out, retrying three times, and returns its sha256.
func (f Fetch) download(ctx context.Context, url string, out *os.File) (string, error) {
	var last error
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			fmt.Fprintf(f.Out, "retrying (%v)\n", last)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		if _, err := out.Seek(0, io.SeekStart); err != nil {
			return "", err
		}
		if err := out.Truncate(0); err != nil {
			return "", err
		}
		sum, err := f.once(ctx, url, out)
		if err == nil {
			return sum, nil
		}
		if p, ok := err.(permanent); ok {
			return "", p.error
		}
		last = err
	}
	return "", last
}

func (f Fetch) once(ctx context.Context, url string, out io.Writer) (string, error) {
	stall := f.Stall
	if stall <= 0 {
		stall = defaultStall
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The watchdog cancels the request when nothing arrives for stall:
	// headers, or the next piece of the body.
	var stalled atomic.Bool
	watchdog := time.AfterFunc(stall, func() { stalled.Store(true); cancel() })
	defer watchdog.Stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := f.HTTP.Do(req)
	if err != nil {
		if stalled.Load() {
			return "", fmt.Errorf("stalled: nothing received for %s", stall)
		}
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
		return "", permanent{errors.New(resp.Status)}
	}
	if resp.StatusCode != http.StatusOK {
		return "", errors.New(resp.Status)
	}
	h := sha256.New()
	p := &progress{w: f.Out, total: resp.ContentLength}
	body := readerFunc(func(b []byte) (int, error) {
		n, err := resp.Body.Read(b)
		if n > 0 {
			watchdog.Reset(stall)
		}
		return n, err
	})
	if _, err := io.Copy(io.MultiWriter(out, h, p), body); err != nil {
		if stalled.Load() {
			return "", fmt.Errorf("stalled: nothing received for %s", stall)
		}
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type readerFunc func([]byte) (int, error)

func (r readerFunc) Read(b []byte) (int, error) { return r(b) }

// progress prints a line at every 10% of a download of known size.
type progress struct {
	w           io.Writer
	total, done int64
	shown       int64
}

func (p *progress) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if p.total > 0 {
		if step := p.done * 10 / p.total; step > p.shown {
			p.shown = step
			fmt.Fprintf(p.w, "  %d%% of %.1f MiB\n", step*10, float64(p.total)/(1<<20))
		}
	}
	return len(b), nil
}

// extract unpacks a gzipped tar into dir. Only directories, regular files and
// symlinks are accepted. A name that is absolute or holds "..", and a symlink
// whose target is absolute or reads as leaving dir, fail the whole extract.
// Every write goes through an os.Root on dir, so a chain of symlinks that
// each look inside as text (a -> ., a/a/e -> ../../x, then e/file) still
// cannot place anything outside it.
func extract(r io.Reader, dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(h.Name)
		if path.IsAbs(h.Name) || slices.Contains(strings.Split(h.Name, "/"), "..") {
			return fmt.Errorf("%q is outside the archive", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
				return err
			}
			w, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(h.Mode).Perm()&0o755)
			if err != nil {
				return err
			}
			_, err = io.Copy(w, io.LimitReader(tr, h.Size))
			if cerr := w.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			resolved := path.Clean(path.Join(path.Dir(name), h.Linkname))
			if path.IsAbs(h.Linkname) || resolved == ".." || strings.HasPrefix(resolved, "../") {
				return fmt.Errorf("symlink %q -> %q leaves the archive", h.Name, h.Linkname)
			}
			if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
				return err
			}
			if err := root.Symlink(h.Linkname, name); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%q: entry type %q is not allowed", h.Name, h.Typeflag)
		}
	}
}
