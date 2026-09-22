package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/parrot"
)

// fakeResolver answers Ensure from next, numbering calls from 1.
type fakeResolver struct {
	mu    sync.Mutex
	calls int
	next  func(call int) parrot.Result
}

func (f *fakeResolver) Ensure(ctx context.Context, id string) parrot.Result {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	return f.next(n)
}

func (f *fakeResolver) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// sourcedModel is fakeModel with a viiwork-parrot source instead of a path.
func sourcedModel(name string, gpus []int, args ...string) config.Model {
	m := fakeModel(name, gpus, args...)
	m.Path = ""
	m.Source = config.SourceParrot + name
	return m
}

// weightsFile is an existing file standing in for a model parrot seeds.
func weightsFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "m.gguf")
	if err := os.WriteFile(p, []byte("gguf"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func newFetchSupervisor(t *testing.T, r Resolver) (*Supervisor, *eventRecorder, *syncBuffer) {
	t.Helper()
	events, log := &eventRecorder{}, &syncBuffer{}
	deps := testDeps(log)
	deps.Events = events
	deps.Resolver = r
	deps.Timing.FetchPoll = 20 * time.Millisecond
	deps.Timing.FetchRetryMax = 80 * time.Millisecond
	deps.Timing.FetchLogEvery = time.Hour
	s := New(deps)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.Shutdown(ctx)
	})
	return s, events, log
}

func pending(pct float64) parrot.Result {
	return parrot.Result{Kind: parrot.Pending, Code: 202, State: "downloading", Percent: pct, DownRate: 85e6}
}

func TestFetchF1PendingThenReadyLaunchesWithTheResolvedPath(t *testing.T) {
	weights := weightsFile(t)
	var release atomic.Bool
	r := &fakeResolver{next: func(n int) parrot.Result {
		if !release.Load() {
			return pending(float64(min(n, 99)))
		}
		return parrot.Result{Kind: parrot.Ready, Code: 200, Path: weights}
	}}
	s, _, log := newFetchSupervisor(t, r)
	mustApply(t, s, sourcedModel("f1", []int{0}))

	m, _ := s.Model("f1")
	b := m.Backends()[0]
	within(t, 2*time.Second, "fetching phase", func() bool { return b.Phase() == "fetching" })
	st := s.Status()[0]
	if st.Slots != 0 || st.Backends[0].Status != "starting" || st.Backends[0].Phase != "fetching" {
		t.Errorf("while fetching: slots %d, status %q, phase %q", st.Slots, st.Backends[0].Status, st.Backends[0].Phase)
	}
	if n := len(fakeCallsFor("f1")); n != 0 {
		t.Fatalf("launched %d times while fetching", n)
	}

	release.Store(true)
	modelHealthy(t, s, "f1", 5*time.Second)
	calls := fakeCallsFor("f1")
	if len(calls) != 1 || calls[0].Spec.Path != weights {
		t.Fatalf("calls = %+v, want one launch with path %s", calls, weights)
	}
	if !strings.Contains(log.String(), "fetching viiwork-parrot:f1") {
		t.Errorf("no progress line in the log:\n%s", log.String())
	}
}

func TestFetchF2RefusalMarksEveryBackendDead(t *testing.T) {
	for _, tc := range []struct {
		code int
		want string
	}{
		{404, "unknown model"},
		{409, "seed_only_existing"},
		{500, "sha256 mismatch"},
		{507, "insufficient disk"},
	} {
		t.Run(strconv.Itoa(tc.code), func(t *testing.T) {
			name := "f2-" + strconv.Itoa(tc.code)
			r := &fakeResolver{next: func(int) parrot.Result {
				msg := map[int]string{404: "unknown model", 409: "never downloads", 500: "sha256 mismatch", 507: "insufficient disk"}[tc.code]
				return parrot.Result{Kind: parrot.Refused, Code: tc.code, Message: msg}
			}}
			s, events, _ := newFetchSupervisor(t, r)
			mustApply(t, s, singleGPUBackends(sourcedModel(name, []int{0, 1})))
			m, _ := s.Model(name)
			for _, b := range m.Backends() {
				within(t, 2*time.Second, b.ID()+" dead", func() bool { return b.State() == StateDead })
			}
			if !events.has("viiwork-parrot refused") || !events.has(tc.want) {
				t.Errorf("events lack the refusal naming %q", tc.want)
			}
			if n := len(fakeCallsFor(name)); n != 0 {
				t.Errorf("launched %d times after a refusal", n)
			}
			if n := r.Calls(); n != 1 {
				t.Errorf("Ensure called %d times, want 1 (the backends share one fetch)", n)
			}
		})
	}
}

func TestFetchF3UnavailableRetriesThenSucceeds(t *testing.T) {
	weights := weightsFile(t)
	r := &fakeResolver{next: func(n int) parrot.Result {
		if n <= 3 {
			return parrot.Result{Kind: parrot.Unavailable, Message: "connection refused"}
		}
		return parrot.Result{Kind: parrot.Ready, Code: 200, Path: weights}
	}}
	s, _, log := newFetchSupervisor(t, r)
	mustApply(t, s, sourcedModel("f3", []int{0}))
	modelHealthy(t, s, "f3", 5*time.Second)
	if n := strings.Count(log.String(), "waiting for viiwork-parrot: connection refused"); n != 1 {
		t.Errorf("the unchanged error was logged %d times, want once", n)
	}
}

// Decision: a fetch holds no load ticket. The sourced model is listed first
// and never resolves; the plain model behind it must still load.
func TestFetchF4AFetchingModelDoesNotBlockTheGate(t *testing.T) {
	r := &fakeResolver{next: func(n int) parrot.Result { return pending(1) }}
	s, _, _ := newFetchSupervisor(t, r)
	mustApply(t, s, sourcedModel("f4-sourced", []int{0}), fakeModel("f4-plain", []int{1}, "ready_after=100ms"))
	modelHealthy(t, s, "f4-plain", 5*time.Second)
	m, _ := s.Model("f4-sourced")
	if p := m.Backends()[0].Phase(); p != "fetching" {
		t.Errorf("sourced phase = %q, want fetching", p)
	}
}

func TestFetchF5ShutdownDuringAFetchIsPrompt(t *testing.T) {
	r := &fakeResolver{next: func(n int) parrot.Result { return pending(1) }}
	s, _, _ := newFetchSupervisor(t, r)
	mustApply(t, s, sourcedModel("f5", []int{0}))
	m, _ := s.Model("f5")
	within(t, 2*time.Second, "fetching", func() bool { return m.Backends()[0].Phase() == "fetching" })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	s.Shutdown(ctx)
	if d := time.Since(start); d > time.Second {
		t.Errorf("shutdown took %v with a fetch in progress", d)
	}
}

func TestFetchF6RespawnDoesNotResolveAgain(t *testing.T) {
	weights := weightsFile(t)
	r := &fakeResolver{next: func(int) parrot.Result { return parrot.Result{Kind: parrot.Ready, Code: 200, Path: weights} }}
	s, _, _ := newFetchSupervisor(t, r)
	mustApply(t, s, sourcedModel("f6", []int{0}, "exit_after=700ms"))
	within(t, 10*time.Second, "a relaunch", func() bool { return len(fakeCallsFor("f6")) >= 2 })
	for i, c := range fakeCallsFor("f6") {
		if c.Spec.Path != weights {
			t.Errorf("launch %d path %q, want %q", i, c.Spec.Path, weights)
		}
	}
	if n := r.Calls(); n != 1 {
		t.Errorf("Ensure called %d times, want 1", n)
	}
}

func TestFetchF7PathMissingHereIsDead(t *testing.T) {
	r := &fakeResolver{next: func(int) parrot.Result {
		return parrot.Result{Kind: parrot.Ready, Code: 200, Path: "/nonexistent/parrot/m.gguf"}
	}}
	s, events, _ := newFetchSupervisor(t, r)
	mustApply(t, s, sourcedModel("f7", []int{0}))
	m, _ := s.Model("f7")
	within(t, 2*time.Second, "dead", func() bool { return m.Backends()[0].State() == StateDead })
	if !events.has("does not exist here") {
		t.Error("the event must say the path is missing from this side")
	}
}

func TestFetchF8BackendsShareOneResolve(t *testing.T) {
	weights := weightsFile(t)
	r := &fakeResolver{next: func(int) parrot.Result { return parrot.Result{Kind: parrot.Ready, Code: 200, Path: weights} }}
	s, _, _ := newFetchSupervisor(t, r)
	mustApply(t, s, singleGPUBackends(sourcedModel("f8", []int{0, 1})))
	modelHealthy(t, s, "f8", 5*time.Second)
	if n := r.Calls(); n != 1 {
		t.Errorf("Ensure called %d times for two backends, want 1", n)
	}
}

func TestFetchF9NoResolverIsDead(t *testing.T) {
	s, events, _ := newFetchSupervisor(t, nil)
	mustApply(t, s, sourcedModel("f9", []int{0}))
	m, _ := s.Model("f9")
	within(t, 2*time.Second, "dead", func() bool { return m.Backends()[0].State() == StateDead })
	if !events.has("no viiwork-parrot resolver") {
		t.Error("the event must name the missing resolver")
	}
}
