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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
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
	// Engines checks the release's engine requirements; nil checks nothing.
	Engines    func(ctx context.Context, required map[string]string) error
	MaxArchive int64 // 0 = 512 MiB
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
	sums, err := st.get(ctx, version, "SHA256SUMS", 64<<10)
	if err != nil {
		return err
	}
	sig, err := st.get(ctx, version, "SHA256SUMS.sig", 4<<10)
	if err != nil {
		return err
	}
	parsed, err := release.ParseSums(sums)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnverified, err)
	}
	if err := release.CheckSumsFor(version, parsed); err != nil {
		return fmt.Errorf("%w: %v", ErrUnverified, err)
	}
	if err := release.Verify(st.Keys, version, sums, sig); err != nil {
		return fmt.Errorf("%w: %v", ErrUnverified, err)
	}
	name := release.ArchiveName(version, st.Target)
	archive, err := st.get(ctx, version, name, max)
	if err != nil {
		return err
	}
	if err := parsed.Check(name, bytes.NewReader(archive)); err != nil {
		return fmt.Errorf("%w: %v", ErrUnverified, err)
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
	if p, err := Binary(st.Dir, version); err == nil {
		if got, err := FileSHA256(p); err == nil && got == hex.EncodeToString(sum[:]) {
			return nil
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
	if st.Engines != nil {
		out, err := runStaged(ctx, filepath.Join(tmp, binName), "--engine-requirements")
		if err != nil {
			return fmt.Errorf("staged %s --engine-requirements: %v", version, err)
		}
		var req map[string]string
		if err := json.Unmarshal([]byte(out), &req); err != nil {
			return fmt.Errorf("%w: %s --engine-requirements: %v", ErrUnverified, version, err)
		}
		if err := st.Engines(ctx, req); err != nil {
			return err
		}
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

func (st *Stager) get(ctx context.Context, version, asset string, max int64) ([]byte, error) {
	u := strings.TrimSuffix(st.Source, "/") + "/" + version + "/" + asset
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := st.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", u, resp.StatusCode)
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

func runStaged(ctx context.Context, bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	return string(out), err
}
