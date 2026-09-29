package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/janit/viiwork/v2/internal/httpjson"
	"github.com/janit/viiwork/v2/internal/release"
	"github.com/janit/viiwork/v2/meshapi"
)

// Authorizer is the mesh-write rule — alias.Authorizer: a meshauth signature
// with a fresh nonce in a secured mesh, loopback in an open one.
type Authorizer interface {
	Authorize(r *http.Request, body []byte) (status int, message string, ok bool)
}

const (
	maxBody      = 4 << 10
	stageTimeout = 15 * time.Minute
)

// Service is /v1/update. GET reports; stage, activate and rollback are POSTs
// that need update.enabled and the Authorizer. Activate and rollback save the
// new state, answer 202, then Restart: the node's ordered shutdown, after
// which main execs the launcher.
type Service struct {
	Enabled  bool
	Running  string
	Store    *Store
	Stager   *Stager
	Auth     Authorizer
	Backends func() []meshapi.ModelStatus
	Engines  func() map[string]string
	Deadline func() (time.Time, bool)
	Restart  func()
	Log      func(format string, args ...any)

	// Managed is a Docker install whose host helper makes the image follow
	// current (ManagedInstall). There the release is the image: activate and
	// rollback save the state and answer 202, and the helper's compose up
	// restarts the node on the new image. If this process is still running
	// HelperWait later, the helper did not act, and the state it had is
	// restored: a host without a working helper must not sit with a pending
	// release it will never start.
	Managed    bool
	HelperWait time.Duration   // 0 = 2 minutes
	Stopping   <-chan struct{} // closed when the node shuts down: the helper acted

	// staging is held for a whole stage, which can take minutes. It is
	// separate from the state transitions so a rollback is always answered
	// at once; a second stage meanwhile gets 409.
	staging sync.Mutex
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == meshapi.PathUpdate && r.Method == http.MethodGet:
		s.status(w)
	case r.Method != http.MethodPost:
		w.Header().Set("Allow", "POST")
		httpjson.Error(w, http.StatusMethodNotAllowed, "update_error", "method not allowed")
	case r.URL.Path == meshapi.PathUpdateStage:
		s.write(w, r, s.stage)
	case r.URL.Path == meshapi.PathUpdateActivate:
		s.write(w, r, s.activate)
	case r.URL.Path == meshapi.PathUpdateRollback:
		s.write(w, r, s.rollback)
	default:
		http.NotFound(w, r)
	}
}

func (s *Service) status(w http.ResponseWriter) {
	st, err := s.Store.Load()
	if err != nil {
		httpjson.Error(w, http.StatusInternalServerError, "update_error", err.Error())
		return
	}
	out := meshapi.UpdateStatus{Enabled: s.Enabled, Running: s.Running, Current: st.Current, LastGood: st.LastGood, Staged: Staged(s.Store.Dir)}
	// Reported only while it is there to go back to. Staged is enough here:
	// the rollback itself re-verifies the binary, and a status poll should
	// not hash one.
	if st.Previous == Builtin || slices.Contains(out.Staged, st.Previous) {
		out.Previous = st.Previous
	}
	if s.Engines != nil {
		if e := s.Engines(); len(e) > 0 {
			out.Engines = e
		}
	}
	if p := st.Pending; p != nil {
		out.Pending = &meshapi.UpdatePending{Version: p.Version, Attempts: p.Attempts}
		if s.Deadline != nil && p.Version == s.Running {
			if d, ok := s.Deadline(); ok {
				out.Pending.Deadline = d.UTC().Format(time.RFC3339)
			}
		}
	}
	httpjson.Write(w, http.StatusOK, out)
}

type handlerFunc func(ctx context.Context, req meshapi.UpdateRequest) (int, any)

func (s *Service) write(w http.ResponseWriter, r *http.Request, fn handlerFunc) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "update_error", err.Error())
		return
	}
	if len(body) > maxBody {
		httpjson.Error(w, http.StatusRequestEntityTooLarge, "update_error", "request body too large")
		return
	}
	if !s.Enabled {
		httpjson.Error(w, http.StatusForbidden, "update_error", "updates are not enabled on this node (update.enabled)")
		return
	}
	if status, msg, ok := s.Auth.Authorize(r, body); !ok {
		httpjson.Error(w, status, "update_error", msg)
		return
	}
	var req meshapi.UpdateRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			httpjson.Error(w, http.StatusBadRequest, "update_error", "the body must be a JSON object: "+err.Error())
			return
		}
	}
	if s.Store.Restarting() {
		httpjson.Error(w, http.StatusConflict, "update_error", ErrRestarting.Error())
		return
	}
	code, v := fn(r.Context(), req)
	if e, ok := v.(error); ok {
		httpjson.Error(w, code, "update_error", e.Error())
		return
	}
	httpjson.Write(w, code, v)
}

func (s *Service) stage(ctx context.Context, req meshapi.UpdateRequest) (int, any) {
	if !release.ValidVersion(req.Version) {
		return http.StatusBadRequest, fmt.Errorf("not a release version: %q", req.Version)
	}
	if !s.staging.TryLock() {
		return http.StatusConflict, errors.New("a stage is already in progress on this node")
	}
	defer s.staging.Unlock()
	ctx, cancel := context.WithTimeout(ctx, stageTimeout)
	defer cancel()
	err := s.Stager.Stage(ctx, req.Version)
	switch {
	case err == nil:
		s.Log("update: staged %s", req.Version)
		return http.StatusOK, map[string]string{"staged": req.Version}
	case errors.Is(err, ErrEngineTooOld):
		return http.StatusConflict, err
	case errors.Is(err, ErrUnverified):
		return http.StatusUnprocessableEntity, err
	}
	return http.StatusBadGateway, err
}

func (s *Service) activate(_ context.Context, req meshapi.UpdateRequest) (int, any) {
	if !release.ValidVersion(req.Version) {
		return http.StatusBadRequest, fmt.Errorf("not a release version: %q", req.Version)
	}
	if _, err := Binary(s.Store.Dir, req.Version); err != nil {
		return http.StatusConflict, fmt.Errorf("%s is not staged here: %v", req.Version, err)
	}
	if s.Managed {
		if err := ImagePrepared(s.Store.Dir, req.Version); err != nil {
			return http.StatusConflict, err
		}
	}
	if c, ok := Compare(req.Version, s.Running); ok {
		if c == 0 {
			return http.StatusConflict, fmt.Errorf("%s is already running", req.Version)
		}
		if c < 0 && !req.AllowDowngrade {
			return http.StatusConflict, fmt.Errorf("%s is older than the running %s: pass allow_downgrade, or roll back", req.Version, s.Running)
		}
	}
	baseline := []string{}
	for _, m := range s.Backends() {
		for _, b := range m.Backends {
			switch b.Status {
			case meshapi.StatusHealthy:
				baseline = append(baseline, b.ID)
			case meshapi.StatusStarting:
				// The baseline is what the new release must bring back. A
				// model still loading would be left out of it, so the release
				// would confirm without ever having to load it.
				return http.StatusConflict, fmt.Errorf("model %s is still loading (backend %s, %s): activate when it is healthy", m.Name, b.ID, b.Phase)
			}
		}
	}
	var prior, next State
	err := s.Store.Update(func(st *State) (bool, error) {
		prior = *st
		st.Current = req.Version
		st.Pending = &Pending{Version: req.Version, Baseline: baseline}
		next = *st
		return !s.Managed, nil
	})
	if code, e := transitionError(err); e != nil {
		return code, e
	}
	s.Log("update: activating %s (baseline: %d healthy backends)", req.Version, len(baseline))
	s.restart(prior, next, "activating "+req.Version)
	return http.StatusAccepted, map[string]string{"activating": req.Version}
}

// rollback leaves a release that has not confirmed for the last good one.
// With nothing pending and the node on its last good release, it goes one
// further, to the release that was last good before it, which becomes last
// good again: it already confirmed on this host once. Both are decided under
// the store's lock, so a confirmation landing meanwhile is either wholly
// before the decision or refused after it.
func (s *Service) rollback(context.Context, meshapi.UpdateRequest) (int, any) {
	var to string
	var prior, next State
	err := s.Store.Update(func(st *State) (bool, error) {
		prior = *st
		defer func() { next = *st }()
		if st.Pending != nil || st.Current != st.LastGood {
			st.Current, st.Pending = st.LastGood, nil
			to = st.Current
			return !s.Managed, nil
		}
		if st.Previous == "" {
			return false, fmt.Errorf("%w (%s), and no earlier release is remembered here", errNothingToRollBack, st.LastGood)
		}
		if st.Previous != Builtin {
			if _, err := Binary(s.Store.Dir, st.Previous); err != nil {
				return false, fmt.Errorf("%w (%s), and the release before it cannot be run: %v", errNothingToRollBack, st.LastGood, err)
			}
		}
		st.Current, st.LastGood, st.Previous = st.Previous, st.Previous, ""
		to = st.Current
		return !s.Managed, nil
	})
	if code, e := transitionError(err); e != nil {
		return code, e
	}
	s.Log("update: rolling back to %s", to)
	s.restart(prior, next, "rolling back to "+to)
	return http.StatusAccepted, map[string]string{"rolling_back_to": to}
}

// restart follows a saved transition: the node's own restart, or on a
// managed install the helper's, watched.
func (s *Service) restart(prior, next State, what string) {
	if !s.Managed {
		go s.Restart()
		return
	}
	go s.awaitHelper(prior, next, what)
}

// awaitHelper restores prior when this process is still running HelperWait
// after it saved next — unless the state has moved on since, which is then
// someone else's decision. It does not compare versions: the helper's swap
// ends this process, so still being here is the evidence.
func (s *Service) awaitHelper(prior, next State, what string) {
	wait := s.HelperWait
	if wait <= 0 {
		wait = 2 * time.Minute
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-s.Stopping:
		return
	case <-t.C:
	}
	err := s.Store.Update(func(st *State) (bool, error) {
		if !sameTransition(*st, next) {
			return false, errNotOwned
		}
		launcher := st.Launcher
		*st = prior
		st.Launcher = launcher
		return false, nil
	})
	switch {
	case errors.Is(err, errNotOwned), errors.Is(err, ErrRestarting):
		return
	case err != nil:
		s.Log("update: restoring the state after %s: %v", what, err)
		return
	}
	s.Log("update: the host's engine helper did not restart this node within %s of %s: back to current %s. "+
		"Is viiwork-engine.path enabled on the host (systemctl status viiwork-engine.path; sudo viiwork init adds it)?", wait, what, prior.Current)
}

// WatchPending is awaitHelper for a node that starts, on a managed install,
// with a pending release it is not running: it restarted before the helper
// acted. Given the same time, it then goes back to last good.
func (s *Service) WatchPending() {
	if !s.Managed {
		return
	}
	st, err := s.Store.Load()
	if err != nil || st.Pending == nil || st.Pending.Version == s.Running {
		return
	}
	prior := st
	prior.Current, prior.Pending = st.LastGood, nil
	go s.awaitHelper(prior, st, "starting with "+st.Pending.Version+" pending")
}

// sameTransition reports whether st is still the state a transition saved.
func sameTransition(st, saved State) bool {
	if st.Current != saved.Current || st.LastGood != saved.LastGood || (st.Pending == nil) != (saved.Pending == nil) {
		return false
	}
	return st.Pending == nil || st.Pending.Version == saved.Pending.Version
}

var errNothingToRollBack = errors.New("already on the last good release")

// transitionError maps a Store.Update failure to its answer.
func transitionError(err error) (int, error) {
	switch {
	case err == nil:
		return 0, nil
	case errors.Is(err, ErrRestarting), errors.Is(err, errNothingToRollBack):
		return http.StatusConflict, err
	}
	return http.StatusInternalServerError, err
}
