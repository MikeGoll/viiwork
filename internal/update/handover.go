package update

import "fmt"

// HandoverEnv is set by a launcher on the staged binary it execs. It can only
// make a binary skip handing over, which is always safe: at worst the floor
// runs. main removes it before any later exec, so a restart starts at the
// launcher again.
const HandoverEnv = "VIIWORK_HANDOVER"

// maxAttempts is how many starts a pending release gets to confirm itself.
const maxAttempts = 3

// Decision is what the launcher does at startup.
type Decision struct {
	Exec    string // the staged binary to exec; "" runs this binary
	State   State  // saved before acting when Changed
	Changed bool
	Logs    []string
}

// Decide is the launcher's startup rule, pure. running is the launcher's own
// version; staged resolves a version to its re-verified binary.
func Decide(s State, running string, staged func(version string) (string, error)) Decision {
	d := Decision{State: s}
	if s.Current == Builtin {
		return d
	}
	// A newer floor wins. The fleet is also upgraded out of band (a new image
	// laid over the old one, a rebuilt binary); without this the new floor
	// would hand over to the older staged release.
	if c, ok := Compare(running, s.Current); ok && c >= 0 {
		d.State = State{Current: Builtin, LastGood: Builtin, Launcher: s.Launcher}
		d.Changed = true
		d.Logs = append(d.Logs, fmt.Sprintf("this binary (%s) is at least staged %s: running it, staged releases forgotten", running, s.Current))
		return d
	}
	if p := s.Pending; p != nil && p.Version == s.Current {
		next := *p
		next.Attempts++
		d.Changed = true
		if next.Attempts >= maxAttempts {
			d.State.Current, d.State.Pending = s.LastGood, nil
			d.Logs = append(d.Logs, fmt.Sprintf("%s did not confirm in %d starts: back to %s", p.Version, maxAttempts, s.LastGood))
			if d.State.Current == Builtin {
				return d
			}
		} else {
			d.State.Pending = &next
		}
	}
	path, err := staged(d.State.Current)
	if err != nil {
		bad := d.State.Current
		d.State.Current, d.State.Pending = Builtin, nil
		if d.State.LastGood == bad {
			d.State.LastGood = Builtin
		}
		d.Changed = true
		d.Logs = append(d.Logs, fmt.Sprintf("not running %s: %v; running this binary", bad, err))
		return d
	}
	d.Exec = path
	return d
}

// StartupEnv is what the handover needs from main.
type StartupEnv struct {
	StateDir   string
	Running    string
	HandedOver bool // HandoverEnv was set: this is the staged binary its launcher chose
	Self       func() (Launcher, error)
}

// Startup is the handover, run by main before the node is built. It records
// the launcher, applies Decide and saves the state; exec is the staged
// binary to run in place of this one, or "".
func Startup(e StartupEnv) (exec string, logs []string, err error) {
	dir := ReleasesDir(e.StateDir)
	s, err := LoadState(dir)
	if err != nil {
		return "", nil, err
	}
	if e.HandedOver {
		return "", nil, nil
	}
	changed := false
	self, err := e.Self()
	if err != nil {
		return "", nil, fmt.Errorf("describing this binary as the launcher: %w", err)
	}
	if s.Launcher == nil || *s.Launcher != self {
		s.Launcher, changed = &self, true
	}
	d := Decide(s, e.Running, func(v string) (string, error) { return Binary(dir, v) })
	if d.Changed || changed {
		if err := SaveState(dir, d.State); err != nil {
			return "", d.Logs, err
		}
	}
	return d.Exec, d.Logs, nil
}
