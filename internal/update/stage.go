package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/janit/viiwork/v2/internal/release"
)

// ErrUnverified marks a release whose signature, checksums or contents do
// not hold up.
var ErrUnverified = errors.New("release does not verify")

const defaultMaxArchive = 512 << 20

// Stager downloads, verifies and unpacks releases into Dir. Every check
// happens here, on the node: only signed code for this exact version, whose
// archive matches the signed SHA256SUMS and passes release.ReadArchive, that
// runs on this host and says it is that version, and whose engine
// requirements this host meets, is ever staged.
type Stager struct {
	Dir    string
	Source string // update.source
	Client *http.Client
	Keys   []ed25519.PublicKey
	Target release.Target
	// Local, when set, supplies the archive from a directory holding
	// SHA256SUMS, SHA256SUMS.sig and the archives — viiwork-parrot's copy.
	// The signed files are always Source's when Source answers; Local's are
	// used only while Source is unreachable (see unreachable). nil = Source
	// only.
	Local func(ctx context.Context, version string) (dir string, err error)
	// Log reports a fallback between Local and Source; nil logs nothing.
	Log func(format string, args ...any)
	// MetaTimeout bounds each fetch of SHA256SUMS and its signature from
	// Source; exceeding it counts as unreachable. 0 = 30s.
	MetaTimeout time.Duration
	// Engines checks the release's engine requirements; nil checks nothing.
	Engines func(ctx context.Context, required map[string]string) error
	// PrepareLlama makes the llama.cpp build the staged release is pinned to
	// available before anything is checked against it: a Mac install fetches
	// it beside the running one. It receives the staged binary's build
	// information (zero pin fields when the binary predates --build-info) and
	// may return the engine check to run in place of Engines, judged against
	// the build the release will run rather than the one running now. nil
	// prepares nothing.
	PrepareLlama func(ctx context.Context, info BuildInfo) (engines func(ctx context.Context, required map[string]string) error, err error)
	// PrepareImage, on a Docker install whose host helper swaps the image,
	// has the helper pull and verify version's image and checks the
	// requirements against the engine that image carries, in place of
	// Engines: there the release is the image, engine included.
	PrepareImage func(ctx context.Context, version string, required map[string]string) error
	MaxArchive   int64 // 0 = 512 MiB
}

// BuildInfo is what `viiwork --build-info` prints.
type BuildInfo struct {
	Version           string `json:"version"`
	LlamaCpp          string `json:"llama_cpp,omitempty"`
	LlamaCppMacSHA256 string `json:"llama_cpp_macos_sha256,omitempty"`
}

// ReadBuildInfo runs bin --build-info.
func ReadBuildInfo(ctx context.Context, bin string) (BuildInfo, error) {
	out, err := runStaged(ctx, bin, "--build-info")
	if err != nil {
		return BuildInfo{}, fmt.Errorf("%s --build-info: %v", bin, err)
	}
	var info BuildInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return BuildInfo{}, fmt.Errorf("%s --build-info: %v", bin, err)
	}
	return info, nil
}

// Stage makes version runnable from Dir/<version>, replacing an earlier stage
// of it in one rename. On any failure nothing is left behind.
func (st *Stager) Stage(ctx context.Context, version string) error {
	if !release.ValidVersion(version) {
		return fmt.Errorf("%w: not a release version %q", ErrUnverified, version)
	}
	if !slices.Contains(release.Targets, st.Target) {
		return fmt.Errorf("no release is built for %s/%s", st.Target.OS, st.Target.Arch)
	}
	max := st.MaxArchive
	if max <= 0 {
		max = defaultMaxArchive
	}
	name := release.ArchiveName(version, st.Target)
	local := &localRelease{st: st, version: version}
	defer local.close()

	// The signed files: GitHub's whenever GitHub answers.
	sums, sig, err := st.signedFiles(ctx, version)
	fromSource := err == nil
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if st.Local == nil || !unreachable(err) {
			return err
		}
		st.logf("update: GitHub unreachable (%v), using viiwork-parrot's signed files for %s", err, version)
		if sums, err = local.read(ctx, "SHA256SUMS", 64<<10); err == nil {
			sig, err = local.read(ctx, "SHA256SUMS.sig", 4<<10)
		}
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			return fmt.Errorf("GitHub is unreachable and viiwork-parrot cannot provide %s: %w", version, err)
		}
	}
	parsed, err := verifySums(version, st.Keys, sums, sig)
	if err != nil {
		return err
	}

	// The archive: parrot's when it matches the signed sums, else GitHub's.
	var archive []byte
	if st.Local != nil {
		a, err := local.read(ctx, name, max)
		if err == nil {
			if cerr := parsed.Check(name, bytes.NewReader(a)); cerr != nil {
				err = fmt.Errorf("%w: %v", ErrUnverified, cerr)
			}
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		switch {
		case err == nil:
			archive = a
		case !fromSource:
			return fmt.Errorf("GitHub is unreachable and viiwork-parrot's %s is unusable: %w", name, err)
		default:
			st.logf("update: %s not from viiwork-parrot, downloading it from GitHub: %v", name, err)
		}
	}
	if archive == nil {
		a, err := st.get(ctx, version, name, max)
		if err != nil {
			return err
		}
		if err := parsed.Check(name, bytes.NewReader(a)); err != nil {
			return fmt.Errorf("%w: %v", ErrUnverified, err)
		}
		archive = a
	}
	files, err := release.ReadArchive(bytes.NewReader(archive), strings.TrimSuffix(name, ".tar.gz"), max)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrUnverified, name, err)
	}
	bin, ok := files[binName]
	if !ok {
		return fmt.Errorf("%w: %s holds no %s", ErrUnverified, name, binName)
	}
	sum := sha256.Sum256(bin)
	// A signed version has fixed bytes: when they are already staged and
	// intact, there is nothing to do — and nothing to replace, so a retry can
	// never open a window in which the version has no directory.
	// The image is still prepared: it may never have been, when the release
	// was staged before the install had a helper.
	if p, err := Binary(st.Dir, version); err == nil {
		if got, err := FileSHA256(p); err == nil && got == hex.EncodeToString(sum[:]) {
			if st.PrepareImage == nil {
				return nil
			}
			return st.checkEngines(ctx, version, p, st.Engines)
		}
	}

	if err := os.MkdirAll(st.Dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(st.Dir, "."+version+".staging-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp) // a no-op once renamed into place
	for _, n := range []string{binName, "viiwork-accept"} {
		if b, ok := files[n]; ok {
			if err := os.WriteFile(filepath.Join(tmp, n), b, 0o755); err != nil {
				return err
			}
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, sumName), []byte(hex.EncodeToString(sum[:])+"\n"), 0o644); err != nil {
		return err
	}

	// It must run here — a state_dir mounted noexec, or the wrong
	// architecture, fails now rather than as a crash loop after activation —
	// and say it is the version it was staged as.
	out, err := runStaged(ctx, filepath.Join(tmp, binName), "--version")
	if err != nil {
		return fmt.Errorf("staged %s cannot run on this host (is state_dir mounted noexec?): %v", version, err)
	}
	if got := strings.TrimSpace(out); got != version {
		return fmt.Errorf("%w: the %s binary reports version %q", ErrUnverified, version, got)
	}
	engines := st.Engines
	if st.PrepareLlama != nil {
		// A binary without --build-info predates the pin following it, so
		// it runs the engine as configured and there is nothing to prepare.
		info, err := ReadBuildInfo(ctx, filepath.Join(tmp, binName))
		if err != nil {
			info = BuildInfo{Version: version}
		}
		check, err := st.PrepareLlama(ctx, info)
		if err != nil {
			return fmt.Errorf("preparing llama.cpp for %s: %w", version, err)
		}
		if check != nil {
			engines = check
		}
	}
	if err := st.checkEngines(ctx, version, filepath.Join(tmp, binName), engines); err != nil {
		return err
	}

	final := filepath.Join(st.Dir, version)
	// Replace a damaged earlier stage by moving it aside first and deleting it
	// only once the new one is in place.
	aside := ""
	if _, err := os.Stat(final); err == nil {
		aside = tmp + ".old"
		if err := os.Rename(final, aside); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, final); err != nil {
		if aside != "" {
			if rerr := os.Rename(aside, final); rerr != nil {
				return errors.Join(err, fmt.Errorf("restoring the earlier stage: %w", rerr))
			}
		}
		return err
	}
	if aside != "" {
		os.RemoveAll(aside)
	}
	return nil
}

// signedFiles fetches SHA256SUMS and its signature from Source, each within
// MetaTimeout.
func (st *Stager) signedFiles(ctx context.Context, version string) (sums, sig []byte, err error) {
	timeout := st.MetaTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	get := func(asset string, max int64) ([]byte, error) {
		c, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return st.get(c, version, asset, max)
	}
	if sums, err = get("SHA256SUMS", 64<<10); err != nil {
		return nil, nil, err
	}
	if sig, err = get("SHA256SUMS.sig", 4<<10); err != nil {
		return nil, nil, err
	}
	return sums, sig, nil
}

// verifySums requires SHA256SUMS to list exactly version's archives and to
// be signed for exactly version by a compiled-in key, whoever carried it.
func verifySums(version string, keys []ed25519.PublicKey, sums, sig []byte) (release.Sums, error) {
	parsed, err := release.ParseSums(sums)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnverified, err)
	}
	if err := release.CheckSumsFor(version, parsed); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnverified, err)
	}
	if err := release.Verify(keys, version, sums, sig); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnverified, err)
	}
	return parsed, nil
}

// statusError is an answer from Source other than 200.
type statusError struct {
	url  string
	code int
}

func (e *statusError) Error() string { return fmt.Sprintf("%s: HTTP %d", e.url, e.code) }

// unreachable reports whether err means Source could not answer: no
// connection, a timeout, a connection dropped mid-answer, HTTP 5xx or 429.
// Only then are viiwork-parrot's signed files used. It lists what counts
// rather than what does not: anything else is an answer — a 404, a redirect
// off GitHub, signed files over their size cap — and is final, so a release
// withdrawn from GitHub cannot come back through the swarm. The caller checks
// its own context first: a cancelled stage is not "unreachable".
func unreachable(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.code >= 500 || se.code == http.StatusTooManyRequests
	}
	// Look past the URL wrapper: *url.Error is itself a net.Error, and it
	// also carries the redirect guard's refusal.
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var ne net.Error
	return errors.As(err, &ne) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, syscall.ECONNRESET)
}

// localRelease reads files from the directory Stager.Local names, asking
// for it at most once per stage. Only regular files are read, through an
// os.Root, so a link is refused rather than followed.
type localRelease struct {
	st      *Stager
	version string
	asked   bool
	dir     string
	root    *os.Root
	err     error
}

func (l *localRelease) read(ctx context.Context, asset string, max int64) ([]byte, error) {
	if !l.asked {
		l.asked = true
		if l.dir, l.err = l.st.Local(ctx, l.version); l.err == nil {
			l.root, l.err = os.OpenRoot(l.dir)
		}
	}
	if l.err != nil {
		return nil, l.err
	}
	fi, err := l.root.Lstat(asset)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s/%s is not a regular file", l.dir, asset)
	}
	f, err := l.root.Open(asset)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s/%s is larger than %d bytes", l.dir, asset, max)
	}
	return b, nil
}

func (l *localRelease) close() {
	if l.root != nil {
		l.root.Close()
	}
}

func (st *Stager) logf(format string, args ...any) {
	if st.Log != nil {
		st.Log(format, args...)
	}
}

// checkEngines reads the requirements of the staged binary bin and checks
// them: against the image's engine when there is a helper, else against the
// engines installed here.
func (st *Stager) checkEngines(ctx context.Context, version, bin string, engines func(context.Context, map[string]string) error) error {
	if engines == nil && st.PrepareImage == nil {
		return nil
	}
	out, err := runStaged(ctx, bin, "--engine-requirements")
	if err != nil {
		return fmt.Errorf("staged %s --engine-requirements: %v", version, err)
	}
	var req map[string]string
	if err := json.Unmarshal([]byte(out), &req); err != nil {
		return fmt.Errorf("%w: %s --engine-requirements: %v", ErrUnverified, version, err)
	}
	if st.PrepareImage != nil {
		return st.PrepareImage(ctx, version, req)
	}
	return engines(ctx, req)
}

func (st *Stager) get(ctx context.Context, version, asset string, max int64) ([]byte, error) {
	u := strings.TrimSuffix(st.Source, "/") + "/" + version + "/" + asset
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := st.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &statusError{url: u, code: resp.StatusCode}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", u, err)
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", u, max)
	}
	return b, nil
}

// client is st.Client with GitHub's redirect rule: a download may be
// redirected on the source's own host, or over https to GitHub's asset hosts
// (release-assets.githubusercontent.com today), and nowhere else. The
// signature would refuse whatever another host served; this keeps the fleet
// from asking it at all.
func (st *Stager) client() *http.Client {
	c := http.Client{}
	if st.Client != nil {
		c = *st.Client
	}
	src, _ := url.Parse(st.Source)
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		u := req.URL
		if src != nil && u.Scheme == src.Scheme && u.Host == src.Host {
			return nil
		}
		if h := u.Hostname(); u.Scheme == "https" && (h == "github.com" || strings.HasSuffix(h, ".githubusercontent.com")) {
			return nil
		}
		return fmt.Errorf("%w: redirected to %s, which is not GitHub", ErrUnverified, u.Host)
	}
	return &c
}

func runStaged(ctx context.Context, bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	return string(out), err
}
