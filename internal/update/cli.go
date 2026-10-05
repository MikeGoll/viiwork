package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/janit/viiwork/v2/internal/durable"
	"github.com/janit/viiwork/v2/internal/release"
)

// The host's CLI follows the release (docs/releases.md): on a Docker install
// the engine helper installs the release it verified itself, on a Mac the
// node installs the staged release it confirmed, and `viiwork update cli`
// does it by hand anywhere. All three go through InstallCLI and CLIBehind.

// cliFile records the CLI a Mac node installed by following a release: its
// version and sha256. A launcher that is exactly that file was put there by
// the node, not installed out of band, so the state still decides what runs
// (see decide). A file of its own, like previous: state.json's reader in
// older launchers refuses fields it does not know.
const cliFile = "cli.json"

type cliRecord struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

func loadCLIRecord(dir string) (cliRecord, bool) {
	b, err := os.ReadFile(filepath.Join(dir, cliFile))
	if err != nil {
		return cliRecord{}, false
	}
	var r cliRecord
	if json.Unmarshal(b, &r) != nil || !release.ValidVersion(r.Version) || len(r.SHA256) != 64 {
		return cliRecord{}, false
	}
	return r, true
}

// InstallCLI replaces the file at path with data, mode 0755, keeping the one
// it replaces as <path>.prev (an older .prev is overwritten). The new file is
// written and synced under a temporary name in the same directory first and
// renamed over the old one last, so a failure at any step leaves the old CLI
// in place, and a crash leaves either the old file or the new one. A link is
// followed to the file it names, which is replaced in its own directory.
func InstallCLI(path string, data []byte) (err error) {
	if p, err := filepath.EvalSymlinks(path); err == nil {
		path = p
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	dir, name := filepath.Dir(path), filepath.Base(path)
	f, err := os.CreateTemp(dir, "."+name+".new-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			os.Remove(tmp)
		}
	}()
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(0o755)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	old, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err = durable.WriteFileMode(dir, name+".prev", old, 0o755); err != nil {
			return fmt.Errorf("keeping %s.prev: %w", name, err)
		}
	case errors.Is(err, fs.ErrNotExist):
		err = nil
	default:
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// CLIBehind reports whether the CLI at path should be replaced by release
// want, and what it is now. Only ever forward: a CLI that reports want, a
// newer release, or something that is not a release version at all (a
// developer's build) is left alone. A missing CLI, or one that does not run,
// is behind.
func CLIBehind(ctx context.Context, path, want string) (bool, string) {
	out, err := runStaged(ctx, path, "--version")
	if err != nil {
		if _, serr := os.Stat(path); errors.Is(serr, fs.ErrNotExist) {
			return true, "none"
		}
		return true, fmt.Sprintf("one that does not run (%v)", err)
	}
	got := strings.TrimSpace(out)
	if got == want {
		return false, got
	}
	c, ok := Compare(got, want)
	return ok && c < 0, got
}

// stagedBytes is a staged version's viiwork, read once and checked against
// the sha256 recorded when it was staged: the bytes returned are the ones
// checked.
func stagedBytes(dir, version string) ([]byte, string, error) {
	if !release.ValidVersion(version) {
		return nil, "", fmt.Errorf("not a release version: %q", version)
	}
	want, err := os.ReadFile(filepath.Join(dir, version, sumName))
	if err != nil {
		return nil, "", fmt.Errorf("%s is not staged: %w", version, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, version, binName))
	if err != nil {
		return nil, "", fmt.Errorf("%s is not staged: %w", version, err)
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if strings.TrimSpace(string(want)) != got {
		return nil, "", fmt.Errorf("staged %s: the binary no longer matches its recorded sha256", version)
	}
	return data, got, nil
}

// errUnchanged skips a Store.Update that has nothing to save.
var errUnchanged = errors.New("unchanged")

// FollowCLI is a Mac node's half of the CLI following the release: once
// version has confirmed, its staged binary — the one this process was
// handed over to, re-checked against its recorded sha256 — is installed at
// cli, which on a Mac install is also the LaunchAgent's binary, the
// launcher. It records what it installed (cliFile), so the next start knows
// the floor is the node's own and not an out-of-band install, and moves the
// recorded launcher's sha256 with the file, so a later restart execs it
// rather than exiting over a changed launcher. A CLI already at or past
// version is left alone.
func (s *Store) FollowCLI(ctx context.Context, version, cli string, logf func(string, ...any)) error {
	data, sum, err := stagedBytes(s.Dir, version)
	if err != nil {
		return err
	}
	behind, was := CLIBehind(ctx, cli, version)
	if !behind {
		return nil
	}
	if err := InstallCLI(cli, data); err != nil {
		return fmt.Errorf("installing %s at %s: %w", version, cli, err)
	}
	b, _ := json.Marshal(cliRecord{Version: version, SHA256: sum})
	if err := durable.WriteFile(s.Dir, cliFile, append(b, '\n')); err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(cli)
	if err != nil {
		resolved = cli
	}
	err = s.Update(func(st *State) (bool, error) {
		if st.Launcher == nil || st.Launcher.Path != resolved {
			return false, errUnchanged
		}
		st.Launcher.SHA256 = sum
		return false, nil
	})
	if err != nil && !errors.Is(err, errUnchanged) && !errors.Is(err, ErrRestarting) {
		return err
	}
	logf("update: %s: %s -> %s (the release this node confirmed; the old one is %s.prev)", cli, was, version, filepath.Base(cli))
	return nil
}
