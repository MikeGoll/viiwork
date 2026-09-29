package update

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/janit/viiwork/v2/internal/durable"
	"github.com/janit/viiwork/v2/internal/release"
)

const (
	stateFile    = "state.json"
	previousFile = "previous"
	binName      = "viiwork"
	sumName      = "viiwork.sha256"
)

// Launcher is the floor binary: the one a restart always execs first, so a
// later start reads state.json again from the top. Recorded with its sha256,
// never taken from the environment: an inherited variable must not choose
// what a node executes.
type Launcher struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Pending is an activated release that has not yet confirmed itself.
type Pending struct {
	Version  string   `json:"version"`
	Attempts int      `json:"attempts"`
	Baseline []string `json:"baseline"` // backends healthy at activation
}

// State is <state_dir>/releases/state.json, plus Previous.
type State struct {
	Current  string    `json:"current"`
	LastGood string    `json:"last_good"`
	Pending  *Pending  `json:"pending,omitempty"`
	Launcher *Launcher `json:"launcher,omitempty"`

	// Previous is the release that was last good before LastGood confirmed:
	// where a rollback goes once there is nothing newer to leave. It is kept
	// in a file of its own, never in state.json, because the fleet's
	// launchers are v2.6.0-beta4, whose state.json reader refuses a field it
	// does not know, and a launcher that cannot read state.json cannot start
	// the node. "" is none.
	Previous string `json:"-"`
}

// previousRecord is the previous file: the release before last good, and the
// last good release it came before. It holds only while that is still the
// last good release, so a last good changed without it — a newer floor's
// reset, a crash between the two writes, a release that predates the file —
// leaves nothing to roll back to rather than somewhere wrong.
type previousRecord struct {
	Version string `json:"version"`
	Before  string `json:"before"`
}

// NewState is a node that has never staged anything.
func NewState() State { return State{Current: Builtin, LastGood: Builtin} }

// ReleasesDir is where staged releases and state.json live.
func ReleasesDir(stateDir string) string { return filepath.Join(stateDir, "releases") }

func validName(v string) bool { return v == Builtin || release.ValidVersion(v) }

// LoadState reads state.json and the previous file. A missing state.json is
// a fresh node; one that cannot be read, or names anything but builtin or a
// version, is an error: the node refuses to start rather than guess what to
// run. The previous file only ever offers a rollback, so whatever is wrong
// with it means there is none.
func LoadState(dir string) (State, error) {
	b, err := os.ReadFile(filepath.Join(dir, stateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return NewState(), nil
	}
	if err != nil {
		return State{}, err
	}
	// Unknown fields are ignored, never refused: the launcher is the floor,
	// the oldest binary, and it reads a file a newer release may have written
	// with fields it added. Refusing them would leave a rolled-back node unable
	// to start. What this release acts on is validated below.
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}, fmt.Errorf("%s: %w", filepath.Join(dir, stateFile), err)
	}
	if !validName(s.Current) || !validName(s.LastGood) || s.Pending != nil && !release.ValidVersion(s.Pending.Version) {
		return State{}, fmt.Errorf("%s: names something that is neither %q nor a release version", filepath.Join(dir, stateFile), Builtin)
	}
	s.Previous = loadPrevious(dir, s.LastGood)
	return s, nil
}

func loadPrevious(dir, lastGood string) string {
	b, err := os.ReadFile(filepath.Join(dir, previousFile))
	if err != nil || len(b) == 0 {
		return ""
	}
	var p previousRecord
	if json.Unmarshal(b, &p) != nil || !validName(p.Version) || p.Before != lastGood || p.Version == lastGood {
		return ""
	}
	return p.Version
}

// SaveState writes the previous file, then state.json, each durably. In that
// order a crash between the two can only lose the way back: the previous
// file names the last good release it belongs to, so it never applies to the
// state.json left behind.
func SaveState(dir string, s State) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var prev []byte
	if s.Previous != "" {
		b, err := json.Marshal(previousRecord{Version: s.Previous, Before: s.LastGood})
		if err != nil {
			return err
		}
		prev = append(b, '\n')
	}
	if err := durable.WriteFile(dir, previousFile, prev); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return durable.WriteFile(dir, stateFile, append(b, '\n'))
}

// FileSHA256 is the hex sha256 of a file.
func FileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Binary is a staged version's viiwork, after re-checking it against the
// sha256 recorded when it was staged: nothing edited since is ever run.
func Binary(dir, version string) (string, error) {
	if !release.ValidVersion(version) {
		return "", fmt.Errorf("not a release version: %q", version)
	}
	p := filepath.Join(dir, version, binName)
	want, err := os.ReadFile(filepath.Join(dir, version, sumName))
	if err != nil {
		return "", fmt.Errorf("%s is not staged: %w", version, err)
	}
	got, err := FileSHA256(p)
	if err != nil {
		return "", fmt.Errorf("%s is not staged: %w", version, err)
	}
	if strings.TrimSpace(string(want)) != got {
		return "", fmt.Errorf("staged %s: the binary no longer matches its recorded sha256", version)
	}
	return p, nil
}

// Staged is every staged version, newest first. Directories that are not a
// version (an interrupted stage's temporary directory) are ignored.
func Staged(dir string) []string {
	entries, _ := os.ReadDir(dir)
	out := []string{}
	for _, e := range entries {
		if !e.IsDir() || !release.ValidVersion(e.Name()) {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), sumName)); err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		c, _ := Compare(out[i], out[j])
		return c > 0
	})
	return out
}

// Prune keeps the current, last good and previous releases and the newest
// other one, and deletes the rest.
func Prune(dir string, s State) error {
	keep := map[string]bool{s.Current: true, s.LastGood: true, s.Previous: true}
	extra := false
	for _, v := range Staged(dir) {
		if keep[v] {
			continue
		}
		if !extra {
			extra = true
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, v)); err != nil {
			return err
		}
	}
	return nil
}

// SelfLauncher describes the running binary, resolved through symlinks.
func SelfLauncher() (Launcher, error) {
	exe, err := os.Executable()
	if err != nil {
		return Launcher{}, err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return Launcher{}, err
	}
	sum, err := FileSHA256(exe)
	if err != nil {
		return Launcher{}, err
	}
	return Launcher{Path: exe, SHA256: sum}, nil
}

// RestartTarget is the launcher a restart execs, after checking it is still
// the file recorded. When it is not — the image or the install was replaced —
// the node exits instead and its supervisor starts the new floor, which then
// applies the newer-floor rule.
func RestartTarget(dir string) (string, error) {
	s, err := LoadState(dir)
	if err != nil {
		return "", err
	}
	if s.Launcher == nil {
		return "", errors.New("no launcher is recorded in state.json")
	}
	got, err := FileSHA256(s.Launcher.Path)
	if err != nil || got != s.Launcher.SHA256 {
		return "", fmt.Errorf("the launcher %s changed since this node started; exiting so the supervisor starts it", s.Launcher.Path)
	}
	return s.Launcher.Path, nil
}

// ErrRestarting refuses a state change once one has asked for a restart: the
// process is about to exec, and a later write would be lost in it.
var ErrRestarting = errors.New("this node is restarting")

// Store serialises every read-modify-write of state.json in this process. The
// API and the confirmer both change the state; without one lock around the
// whole read-modify-write, the confirmer could confirm a release an operator
// had just rolled back, and the restart would come back on it.
type Store struct {
	Dir        string
	mu         sync.Mutex
	restarting bool
}

// NewStore is the store for a releases directory.
func NewStore(dir string) *Store { return &Store{Dir: dir} }

// Load reads the state.
func (s *Store) Load() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return LoadState(s.Dir)
}

// Update applies fn to the current state under the lock and saves the result.
// When fn reports a restart, every later Update is refused with ErrRestarting.
// fn's error aborts without saving.
func (s *Store) Update(fn func(*State) (restart bool, err error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.restarting {
		return ErrRestarting
	}
	st, err := LoadState(s.Dir)
	if err != nil {
		return err
	}
	restart, err := fn(&st)
	if err != nil {
		return err
	}
	if err := SaveState(s.Dir, st); err != nil {
		return err
	}
	s.restarting = restart
	return nil
}

// Restarting reports whether a state change has asked for a restart.
func (s *Store) Restarting() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restarting
}
