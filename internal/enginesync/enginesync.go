// Package enginesync is `viiwork engine-sync`, the engine helper of a Docker
// install made by `viiwork init`: run as root on the host by systemd, it
// makes the compose file's image follow the release the node's state names
// (docs/releases.md). A container cannot replace its own image without the
// Docker socket, which is root on the host, so the node only ever asks.
//
// It runs as root on files a container can write, so it trusts none of them:
// from the node's releases directory it takes state.json's version names and
// an image request's version and id, each validated as exactly that, and
// nothing else. The repository comes from the host's compose file, the
// download root from the host's config, and every release is verified here,
// with this binary's own keys, before its image is used.
package enginesync

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/durable"
	"github.com/janit/viiwork/v2/internal/release"
	"github.com/janit/viiwork/v2/internal/setup/install"
	"github.com/janit/viiwork/v2/internal/update"
)

// Repos are the published engine images (scripts/release-images.sh). The
// helper swaps only an image from one of them, and only for another tag of
// the same one.
var Repos = []string{
	"ghcr.io/janit/viiwork-llamacpp-cuda",
	"ghcr.io/janit/viiwork-llamacpp-vulkan",
	"ghcr.io/janit/viiwork-vllm",
}

// Paths are the host's. Tests point them into a temporary directory.
type Paths struct {
	Manifest string // install.json: the compose file and project
	Config   string // viiwork.yaml: update.source
	StateDir string // mounted as the container's state_dir: container-writable
	Dir      string // the helper's own, root-only
	Binary   string // the host's viiwork, which the helper keeps up to date
}

// DefaultPaths are a Linux install's.
func DefaultPaths() Paths {
	return Paths{Manifest: install.ManifestFile, Config: install.ConfigFile, StateDir: install.StateDir,
		Dir: install.EngineDir, Binary: install.BinaryPath}
}

// Runner runs a command and returns its stdout.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Command is the real Runner; stderr goes to log.
func Command(log io.Writer) Runner {
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Stderr = log
		return cmd.Output()
	}
}

// Helper is one run of engine-sync.
type Helper struct {
	Paths
	Version string // this binary's
	Target  release.Target
	Keys    []ed25519.PublicKey
	Client  *http.Client
	Run     Runner
	Log     func(format string, args ...any)
	// Source replaces update.source, which accepts only GitHub. Test seam:
	// "" = the config's.
	Source string
	// Local asks the host's viiwork-parrot for a release, as the node's
	// stager does: the archive from there, the signed files from GitHub
	// unless GitHub is unreachable. nil = GitHub only.
	Local func(ctx context.Context, version string) (dir string, err error)
}

const (
	recordFile = "sync.json"
	lockFile   = "lock"
	// maxNodeFile bounds what is read from the node's directory.
	maxNodeFile = 64 << 10
	// maxBinary bounds the viiwork read out of an image.
	maxBinary = 512 << 20
	// imageBinary is where a release image carries viiwork.
	imageBinary = "/usr/local/bin/viiwork"
	// containerStateDir is where the compose file mounts StateDir, and the
	// state_dir the node must use for the two to be the same directory.
	containerStateDir = "/var/lib/viiwork"
)

// record is the helper's own state, in its root-only directory.
type record struct {
	// Original is the image tag the install had when the helper first ran:
	// what the node's "builtin" means. Kept where the container cannot write.
	Original string `json:"original"`
	Swapped  *swap  `json:"swapped,omitempty"`
	// History is every version the helper swapped to, oldest first, at most
	// historyMax: with Original, the releases this host has run, which are
	// the only ones it goes back to.
	History []string `json:"history,omitempty"`
	// Pulled is every image (repo:tag) the helper pulled and has not yet
	// removed. It removes only these: an image it did not pull — the one
	// the install started with, one an operator pulled — is not its to remove.
	Pulled []string `json:"pulled,omitempty"`
	// Prepared is the image last prepared for a node's stage and not yet
	// swapped in: the one pre-pull kept beside the images in use.
	Prepared string `json:"prepared,omitempty"`
}

// historyMax bounds History. Rollback goes back one release at a time, so a
// short list covers every rollback a host makes in practice.
const historyMax = 8

// ran reports whether this host has run v: the install's own tag, or one the
// helper swapped to.
func (r *record) ran(v string) bool { return v == r.Original || slices.Contains(r.History, v) }

func (r *record) addHistory(v string) {
	r.History = append(slices.DeleteFunc(r.History, func(h string) bool { return h == v }), v)
	if n := len(r.History) - historyMax; n > 0 {
		r.History = r.History[n:]
	}
}

// mayMoveTo is the downgrade rule. state.json is the container's to write,
// so a compromised node could name any older signed release there and skip
// /v1/update's consent for a downgrade. The helper moves from the running
// tag to a version at least as new, or back to one this host has run
// (rollback, and builtin's original tag), and to nothing else.
func (r *record) mayMoveTo(from, to string) error {
	if c, ok := update.Compare(to, from); ok && c >= 0 {
		return nil
	}
	if r.ran(to) {
		return nil
	}
	return fmt.Errorf("%s is older than the running %s and this host has never run it: the helper goes back only to releases it has run here", to, from)
}

type swap struct {
	Version string    `json:"version"`
	Image   string    `json:"image"` // repo@digest, as verified and pulled
	At      time.Time `json:"at"`
}

// nodeState is what the helper reads from state.json: version names only.
type nodeState struct {
	Current, LastGood, Pending string
}

// Sync is one run: answer the node's image request, make the compose image
// follow current, and bring the host binary up to a confirmed release. It
// repeats while the node changed something during the run, which a path
// unit whose service is still running would not report.
func (h *Helper) Sync(ctx context.Context) error {
	if err := h.ownDir(); err != nil {
		return err
	}
	unlock, err := h.lock()
	if err != nil {
		return err
	}
	defer unlock()
	c, err := h.install()
	if err != nil {
		return err
	}
	var errs []error
	for range 3 {
		st, err := h.nodeState()
		if err != nil {
			return err
		}
		req, _ := h.request()
		errs = errs[:0]
		rec, err := h.loadRecord()
		if err != nil {
			return err
		}
		if rec.Original == "" {
			img, err := h.composeImage(c)
			if err != nil {
				return err
			}
			rec.Original = img.tag
			if err := h.saveRecord(rec); err != nil {
				return err
			}
		}
		if err := h.answer(ctx, c, &rec); err != nil {
			errs = append(errs, err)
		}
		if err := h.follow(ctx, c, st, &rec); err != nil {
			errs = append(errs, err)
		}
		h.gc(ctx, c, &rec)
		if err := h.saveRecord(rec); err != nil {
			return err
		}
		st2, err := h.nodeState()
		if err != nil {
			return err
		}
		req2, _ := h.request()
		if st2 == st && req2 == req {
			break
		}
	}
	return errors.Join(errs...)
}

// composeInstall is the host's compose project, from install.json.
type composeInstall struct {
	file, project, source string
}

func (h *Helper) install() (composeInstall, error) {
	m, err := install.ReadManifest(h.Manifest)
	if err != nil {
		return composeInstall{}, err
	}
	if m.Compose == nil || m.EngineHelper == nil || !filepath.IsAbs(m.Compose.File) || m.Compose.Project != install.Project {
		return composeInstall{}, fmt.Errorf("%s: not a Docker install with the engine helper; nothing to do", h.Manifest)
	}
	stateDir, _, err := config.PeekUpdate(h.Config)
	if err != nil {
		return composeInstall{}, err
	}
	if stateDir != containerStateDir {
		return composeInstall{}, fmt.Errorf("%s: node.state_dir is %s, but the compose file mounts %s there: the helper cannot find the node's state", h.Config, stateDir, containerStateDir)
	}
	src, err := config.PeekSource(h.Config)
	if err != nil {
		return composeInstall{}, fmt.Errorf("%s: %w", h.Config, err)
	}
	if h.Source != "" {
		src = h.Source
	}
	return composeInstall{file: m.Compose.File, project: m.Compose.Project, source: src}, nil
}

// ownDir makes the helper's directory, and refuses one that is not a plain
// directory of this user's.
func (h *Helper) ownDir() error {
	if err := os.MkdirAll(h.Dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(h.Dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%s is not a directory owned by this user", h.Dir)
	}
	return os.Chmod(h.Dir, 0o700)
}

// lock keeps two runs — the timer's and the path unit's, or one by hand —
// from swapping at once.
func (h *Helper) lock() (func(), error) {
	f, err := os.OpenFile(filepath.Join(h.Dir, lockFile), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

// readNode reads a file from the node's releases directory. The directory is
// the container's to write, so no link there is followed: the state
// directory is opened as an os.Root, which refuses a path that leaves it;
// neither releases nor the file may be a link at all (os.Root would follow
// one that stays inside, so each is checked with Lstat, and the file opened
// must be the one checked); and it is opened without blocking, so a FIFO
// cannot stall the helper. Only a small regular file is read.
func (h *Helper) readNode(name string) ([]byte, error) {
	root, err := h.stateRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if fi, err := root.Lstat("releases"); err != nil {
		return nil, err
	} else if !fi.IsDir() {
		return nil, errors.New("releases is not a directory")
	}
	p := filepath.Join("releases", name)
	li, err := root.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !li.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	f, err := root.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || !os.SameFile(li, fi) {
		return nil, fmt.Errorf("%s changed while it was opened", p)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxNodeFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxNodeFile {
		return nil, fmt.Errorf("releases/%s is larger than %d bytes", name, maxNodeFile)
	}
	return b, nil
}

func (h *Helper) stateRoot() (*os.Root, error) {
	fi, err := os.Lstat(h.StateDir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", h.StateDir)
	}
	return os.OpenRoot(h.StateDir)
}

// writeNode replaces a file in the node's releases directory with one of
// root's, readable by the node. It is written under a fresh name and renamed
// over the old: a rename replaces a link rather than following it.
func (h *Helper) writeNode(name string, data []byte) error {
	root, err := h.stateRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	if fi, err := root.Lstat("releases"); err != nil {
		return err
	} else if !fi.IsDir() {
		return errors.New("releases is not a directory")
	}
	rel, err := root.OpenRoot("releases")
	if err != nil {
		return err
	}
	defer rel.Close()
	var raw [8]byte
	rand.Read(raw[:])
	tmp := "." + name + ".tmp-" + hex.EncodeToString(raw[:])
	f, err := rel.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(0o644)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = rel.Rename(tmp, name)
	}
	if err != nil {
		rel.Remove(tmp)
	}
	return err
}

func validName(v string) bool { return v == update.Builtin || release.ValidVersion(v) }

// nodeState reads state.json's version names; a missing file is a node that
// never staged anything.
func (h *Helper) nodeState() (nodeState, error) {
	b, err := h.readNode("state.json")
	if errors.Is(err, fs.ErrNotExist) {
		return nodeState{Current: update.Builtin, LastGood: update.Builtin}, nil
	}
	if err != nil {
		return nodeState{}, fmt.Errorf("reading the node's state.json: %w", err)
	}
	var raw struct {
		Current  string `json:"current"`
		LastGood string `json:"last_good"`
		Pending  *struct {
			Version string `json:"version"`
		} `json:"pending"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nodeState{}, fmt.Errorf("the node's state.json: %w", err)
	}
	st := nodeState{Current: raw.Current, LastGood: raw.LastGood}
	if raw.Pending != nil {
		st.Pending = raw.Pending.Version
	}
	if !validName(st.Current) || !validName(st.LastGood) || st.Pending != "" && !release.ValidVersion(st.Pending) {
		return nodeState{}, errors.New("the node's state.json names something that is neither builtin nor a release version")
	}
	return st, nil
}

// request is the node's image request, or the zero value when there is none
// or it is not exactly a version and an id.
func (h *Helper) request() (update.ImageRequest, error) {
	b, err := h.readNode(update.ImageRequestFile)
	if err != nil {
		return update.ImageRequest{}, err
	}
	var req update.ImageRequest
	if json.Unmarshal(b, &req) != nil || !release.ValidVersion(req.Version) || !update.ValidRequestID(req.ID) {
		return update.ImageRequest{}, errors.New("the node's image request is not a release version and a request id")
	}
	return update.ImageRequest{Version: req.Version, ID: req.ID}, nil
}

// answer prepares the image a pending request names and writes the result,
// failure included, so that the node's stage ends with the reason.
func (h *Helper) answer(ctx context.Context, c composeInstall, rec *record) error {
	req, err := h.request()
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		h.Log("engine-sync: %v; ignored", err)
		return nil
	}
	if b, err := h.readNode(update.ImageResultFile); err == nil {
		var prev update.ImageResult
		if json.Unmarshal(b, &prev) == nil && prev.ID == req.ID {
			return nil // answered
		}
	}
	res := update.ImageResult{Version: req.Version, ID: req.ID}
	img, err := h.composeImage(c)
	if err == nil {
		err = rec.mayMoveTo(img.tag, req.Version)
	}
	if err == nil {
		var p prepared
		p, err = h.prepare(ctx, c, img.repo, req.Version, rec)
		res.Image, res.Engines = p.image, p.engines
	}
	if err == nil {
		// The newest stage wins: gc removes the image an earlier one prepared.
		rec.Prepared = img.repo + ":" + req.Version
	}
	if err != nil {
		res.Error = err.Error()
		h.Log("engine-sync: preparing the %s image: %v", req.Version, err)
	} else {
		res.OK = true
		h.Log("engine-sync: prepared %s for the node's stage", res.Image)
	}
	b, _ := json.Marshal(res)
	if werr := h.writeNode(update.ImageResultFile, append(b, '\n')); werr != nil {
		return fmt.Errorf("answering the node's image request: %w", werr)
	}
	return err
}

// follow is the helper's one rule: the compose image's tag is the node's
// current release, or for builtin the tag the install had.
func (h *Helper) follow(ctx context.Context, c composeInstall, st nodeState, rec *record) error {
	img, err := h.composeImage(c)
	if err != nil {
		return err
	}
	target := st.Current
	if target == update.Builtin {
		target = rec.Original
	}
	if target != img.tag {
		if !release.ValidVersion(target) {
			return fmt.Errorf("the install's own image tag %q is not a release version, so it cannot be verified; leaving the image alone", target)
		}
		if err := rec.mayMoveTo(img.tag, target); err != nil {
			return fmt.Errorf("not swapping to %s: %w", target, err)
		}
		p, err := h.prepare(ctx, c, img.repo, target, rec)
		if err != nil {
			return fmt.Errorf("not swapping to %s: %w", target, err)
		}
		if err := h.swap(ctx, c, img, target); err != nil {
			return err
		}
		rec.Swapped = &swap{Version: target, Image: p.image, At: time.Now().UTC()}
		rec.addHistory(target)
		if rec.Prepared == img.repo+":"+target {
			rec.Prepared = ""
		}
		if err := h.saveRecord(*rec); err != nil {
			return err
		}
		h.Log("engine-sync: %s: %s -> %s (%s); the node restarts on it", c.file, img.tag, target, p.image)
		img.tag = target
	}
	h.prune(st, *rec)
	return h.selfUpdate(ctx, c, st, img.tag)
}

// selfUpdate installs the verified release binary as the host's viiwork
// once the node has confirmed the release the image carries, so that the
// host's CLI and this helper's own rules move with the release. Only ever
// forward, and only ever from the helper's own verification.
func (h *Helper) selfUpdate(ctx context.Context, c composeInstall, st nodeState, tag string) error {
	if !release.ValidVersion(st.Current) || st.Pending != "" || st.LastGood != st.Current || tag != st.Current {
		return nil
	}
	if cmp, ok := update.Compare(h.Version, st.Current); !ok || cmp >= 0 {
		return nil
	}
	bin, err := h.verify(ctx, c, st.Current)
	if err != nil {
		return fmt.Errorf("updating %s to %s: %w", h.Binary, st.Current, err)
	}
	data, err := os.ReadFile(bin)
	if err != nil {
		return err
	}
	dir, name := filepath.Dir(h.Binary), filepath.Base(h.Binary)
	if old, err := os.ReadFile(h.Binary); err == nil {
		if err := durable.WriteFileMode(dir, name+".bak", old, 0o755); err != nil {
			return err
		}
	}
	if err := durable.WriteFileMode(dir, name, data, 0o755); err != nil {
		return err
	}
	h.Log("engine-sync: %s %s -> %s (the release the node confirmed; the old one is %s.bak)", h.Binary, h.Version, st.Current, name)
	return nil
}

// prepared is a verified, pulled image.
type prepared struct {
	image   string            // repo@digest
	engines map[string]string // engine versions it carries
}

// verify stages version into the helper's own directory with the same code
// a node stages with, against this binary's keys, and returns the verified
// viiwork.
func (h *Helper) verify(ctx context.Context, c composeInstall, version string) (string, error) {
	dir := filepath.Join(h.Dir, "releases")
	st := &update.Stager{Dir: dir, Source: c.source, Client: h.Client, Keys: h.Keys, Target: h.Target,
		Local: h.Local, Log: h.Log}
	if err := st.Stage(ctx, version); err != nil {
		return "", err
	}
	return update.Binary(dir, version)
}

// prepare verifies version, pulls repo:version, and requires the image's
// viiwork to be byte for byte the signed release's. The image is read without
// being run: a container is created, never started, and the file copied out
// of it as a tar stream. The engine layers under it are trusted as at
// install time, by registry and tag.
// An image already on this host is used as it is, verified the same way:
// a released tag is written once, and a pull is gigabytes. One the helper
// pulls is recorded in rec, so that gc can remove it again.
func (h *Helper) prepare(ctx context.Context, c composeInstall, repo, version string, rec *record) (prepared, error) {
	bin, err := h.verify(ctx, c, version)
	if err != nil {
		return prepared{}, err
	}
	want, err := update.FileSHA256(bin)
	if err != nil {
		return prepared{}, err
	}
	ref := repo + ":" + version
	if _, err := h.Run(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", ref); err != nil {
		if _, err := h.Run(ctx, "docker", "pull", ref); err != nil {
			return prepared{}, fmt.Errorf("docker pull %s: %w", ref, err)
		}
		if !slices.Contains(rec.Pulled, ref) {
			rec.Pulled = append(rec.Pulled, ref)
		}
	}
	out, err := h.Run(ctx, "docker", "create", ref)
	if err != nil {
		return prepared{}, fmt.Errorf("docker create %s: %w", ref, err)
	}
	id := lastLine(out)
	if !containerID.MatchString(id) {
		return prepared{}, fmt.Errorf("docker create %s: no container id in its output", ref)
	}
	defer h.Run(context.WithoutCancel(ctx), "docker", "rm", "-f", id)
	stream, err := h.Run(ctx, "docker", "cp", id+":"+imageBinary, "-")
	if err != nil {
		return prepared{}, fmt.Errorf("reading %s out of %s: %w", imageBinary, ref, err)
	}
	got, err := tarredFileSHA256(stream)
	if err != nil {
		return prepared{}, fmt.Errorf("%s in %s: %w", imageBinary, ref, err)
	}
	if got != want {
		return prepared{}, fmt.Errorf("%w: the viiwork in %s is not the signed %s release's (sha256 %s, signed %s)", update.ErrUnverified, ref, version, got, want)
	}
	p := prepared{engines: map[string]string{}}
	if out, err := h.Run(ctx, "docker", "image", "inspect", "--format", "{{json .RepoDigests}}", ref); err == nil {
		var digests []string
		json.Unmarshal(out, &digests)
		for _, d := range digests {
			if strings.HasPrefix(d, repo+"@sha256:") {
				p.image = d
			}
		}
	}
	if p.image == "" {
		return prepared{}, fmt.Errorf("docker image inspect %s: no digest from %s", ref, repo)
	}
	if out, err := h.Run(ctx, "docker", "image", "inspect", "--format", "{{json .Config.Labels}}", ref); err == nil {
		var labels map[string]string
		json.Unmarshal(out, &labels)
		p.engines = enginesOf(repo, labels)
	}
	return p, nil
}

var containerID = regexp.MustCompile(`^[0-9a-f]{12,64}$`)

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// tarredFileSHA256 is the sha256 of the one regular file `docker cp - `
// streams. Anything else — a link, a directory, a second entry — is refused.
func tarredFileSHA256(stream []byte) (string, error) {
	tr := tar.NewReader(bytes.NewReader(stream))
	hdr, err := tr.Next()
	if err != nil {
		return "", err
	}
	if hdr.Typeflag != tar.TypeReg || hdr.Size > maxBinary {
		return "", errors.New("not a regular file")
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(tr, maxBinary)); err != nil {
		return "", err
	}
	if _, err := tr.Next(); !errors.Is(err, io.EOF) {
		return "", errors.New("more than one file")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

var llamaBuild = regexp.MustCompile(`(?:^|-)(b[0-9]+)$`)

// enginesOf is the engine version an image carries, read from the base image
// it records (org.opencontainers.image.base.name, set by release-images.sh)
// and spelt as that engine's Versioner spells it. An image that does not say
// reports nothing, which a requirement then refuses.
func enginesOf(repo string, labels map[string]string) map[string]string {
	out := map[string]string{}
	base := labels["org.opencontainers.image.base.name"]
	i := strings.LastIndex(base, ":")
	if i < 0 || strings.Contains(base[i:], "/") {
		return out
	}
	tag := base[i+1:]
	switch {
	case strings.HasPrefix(repo, "ghcr.io/janit/viiwork-llamacpp-"):
		if m := llamaBuild.FindStringSubmatch(tag); m != nil {
			out["llamacpp"] = m[1]
		}
	case repo == "ghcr.io/janit/viiwork-vllm":
		if v := strings.TrimPrefix(tag, "v"); regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+`).MatchString(v) {
			out["vllm"] = v
		}
	}
	return out
}

// image is the compose file's one image line.
type image struct {
	repo, tag string
	line      []int // the tag's byte range in the file
	data      []byte
	mode      fs.FileMode
}

var imageLine = regexp.MustCompile(`(?m)^[ \t]+image:[ \t]*"?([^\s"#]+?)"?[ \t]*$`)

// composeImage finds the one image line in the host's compose file, which
// must name a published engine image by tag.
func (h *Helper) composeImage(c composeInstall) (image, error) {
	fi, err := os.Lstat(c.file)
	if err != nil {
		return image{}, err
	}
	if !fi.Mode().IsRegular() {
		return image{}, fmt.Errorf("%s is not a regular file", c.file)
	}
	data, err := os.ReadFile(c.file)
	if err != nil {
		return image{}, err
	}
	m := imageLine.FindAllSubmatchIndex(data, -1)
	if len(m) != 1 {
		return image{}, fmt.Errorf("%s: want exactly one image: line, found %d", c.file, len(m))
	}
	ref := string(data[m[0][2]:m[0][3]])
	i := strings.LastIndex(ref, ":")
	if i < 0 || strings.Contains(ref[i:], "/") || strings.Contains(ref, "@") {
		return image{}, fmt.Errorf("%s: image %q is not a repository and tag", c.file, ref)
	}
	img := image{repo: ref[:i], tag: ref[i+1:], line: []int{m[0][2] + i + 1, m[0][3]}, data: data, mode: fi.Mode().Perm()}
	if !slices.Contains(Repos, img.repo) {
		return image{}, fmt.Errorf("%s: image %s is not a published viiwork image (%s)", c.file, img.repo, strings.Join(Repos, ", "))
	}
	return img, nil
}

// swap rewrites the image line's tag, keeping a backup of the file as it
// was, and brings the project up on it, which stops the node through its
// ordinary shutdown.
func (h *Helper) swap(ctx context.Context, c composeInstall, img image, tag string) error {
	dir, name := filepath.Dir(c.file), filepath.Base(c.file)
	if err := durable.WriteFileMode(dir, name+".bak", img.data, img.mode); err != nil {
		return err
	}
	var next []byte
	next = append(next, img.data[:img.line[0]]...)
	next = append(next, tag...)
	next = append(next, img.data[img.line[1]:]...)
	if err := durable.WriteFileMode(dir, name, next, img.mode); err != nil {
		return err
	}
	if _, err := h.Run(ctx, "docker", "compose", "-f", c.file, "-p", c.project, "up", "-d"); err != nil {
		// Put the file back: left on the new tag, the next run would find
		// nothing to do while the old container keeps running.
		if rerr := durable.WriteFileMode(dir, name, img.data, img.mode); rerr != nil {
			return fmt.Errorf("docker compose up on %s: %w; restoring %s: %v (the previous file is %s.bak)", tag, err, name, rerr, name)
		}
		return fmt.Errorf("docker compose up on %s: %w", tag, err)
	}
	return nil
}

func (h *Helper) loadRecord() (record, error) {
	b, err := os.ReadFile(filepath.Join(h.Dir, recordFile))
	if errors.Is(err, fs.ErrNotExist) {
		return record{}, nil
	}
	if err != nil {
		return record{}, err
	}
	var r record
	if err := json.Unmarshal(b, &r); err != nil {
		return record{}, fmt.Errorf("%s: %w", filepath.Join(h.Dir, recordFile), err)
	}
	return r, nil
}

func (h *Helper) saveRecord(r record) error {
	b, _ := json.MarshalIndent(r, "", "  ")
	return durable.WriteFileMode(h.Dir, recordFile, append(b, '\n'), 0o600)
}

// gc removes every image the helper pulled that nothing needs: all but the
// compose file's tag, the original, the last two releases it ran and the one
// prepared for a stage. Without it every pull — which a node can ask for —
// would stay on disk, gigabytes each. A failed removal (an image a container
// still uses) only logs, and is tried again next run.
func (h *Helper) gc(ctx context.Context, c composeInstall, rec *record) {
	img, err := h.composeImage(c)
	if err != nil {
		return
	}
	keep := map[string]bool{img.repo + ":" + img.tag: true, img.repo + ":" + rec.Original: true, rec.Prepared: true}
	for _, v := range rec.History[max(0, len(rec.History)-2):] {
		keep[img.repo+":"+v] = true
	}
	var left []string
	for _, ref := range rec.Pulled {
		if keep[ref] {
			left = append(left, ref)
			continue
		}
		if _, err := h.Run(ctx, "docker", "image", "rm", ref); err != nil {
			h.Log("engine-sync: removing %s: %v; kept for now", ref, err)
			left = append(left, ref)
			continue
		}
		h.Log("engine-sync: removed %s, which nothing here uses any more", ref)
	}
	rec.Pulled = left
}

// prune keeps the verified releases a swap or a self-update can still use.
func (h *Helper) prune(st nodeState, rec record) {
	s := update.State{Current: st.Current, LastGood: st.LastGood, Previous: rec.Original}
	if err := update.Prune(filepath.Join(h.Dir, "releases"), s); err != nil {
		h.Log("engine-sync: pruning verified releases: %v", err)
	}
}
