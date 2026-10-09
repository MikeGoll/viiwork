package update

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

// sharedPending is a Service and a Confirmer over one Store, with v2.6.0
// pending (last good v2.5.0) and the confirmer running v2.6.0.
func sharedPending(t *testing.T) (*Service, *Confirmer, *atomic.Int32) {
	t.Helper()
	s, restarts := service(t, true, allow(true))
	s.Running = "v2.6.0"
	if err := SaveState(s.Store.Dir, State{Current: "v2.6.0", LastGood: "v2.5.0",
		Pending: &Pending{Version: "v2.6.0", Baseline: []string{"m/0"}}}); err != nil {
		t.Fatal(err)
	}
	c := &Confirmer{Store: s.Store, Running: "v2.6.0", Window: 10 * time.Minute,
		Now: func() time.Time { return time.Unix(1790000000, 0) }, Restart: func() { restarts.Add(1) }, Log: func(string, ...any) {}}
	return s, c, restarts
}

// The operator's rollback lands between the confirmer reading the backends
// and deciding: the confirmer must not then confirm the release it no longer
// owns, or the restart would come back on the release just rolled back.
func TestConfirmerDoesNotUndoARollback(t *testing.T) {
	s, c, _ := sharedPending(t)
	c.Backends = func() []meshapi.ModelStatus {
		if code, body := call(s, http.MethodPost, meshapi.PathUpdateRollback, `{}`); code != 202 {
			t.Fatalf("rollback: %d %s", code, body)
		}
		return backends(map[string]string{"m/0": meshapi.StatusHealthy}, "")()
	}
	c.Step()
	st, err := s.Store.Load()
	if err != nil || st.Current != "v2.5.0" || st.LastGood != "v2.5.0" || st.Pending != nil {
		t.Fatalf("the rollback was undone: %+v, %v", st, err)
	}
}

// Once the confirmer has rolled back, the node is restarting: no write may
// follow, or an activate answered 202 would be lost in the restart.
func TestConfirmerRollbackBlocksWrites(t *testing.T) {
	s, c, restarts := sharedPending(t)
	c.Backends = backends(map[string]string{"m/0": meshapi.StatusDead}, "")
	c.Step()
	if restarts.Load() != 1 {
		t.Fatalf("restarts = %d", restarts.Load())
	}
	if code, _ := call(s, http.MethodPost, meshapi.PathUpdateRollback, `{}`); code != 409 {
		t.Errorf("a write after the confirmer's rollback: %d", code)
	}
	if err := s.Store.Update(func(*State) (bool, error) { return false, nil }); !errors.Is(err, ErrRestarting) {
		t.Errorf("Store.Update while restarting: %v", err)
	}
}

// A stage can take minutes. It must not hold up an operator's rollback, and a
// second stage meanwhile is told so at once.
func TestStageDoesNotBlockRollback(t *testing.T) {
	s, restarts := service(t, true, allow(true))
	got := make(chan struct{}, 1)
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- struct{}{}:
		default:
		}
		<-release
		http.NotFound(w, r)
	}))
	defer slow.Close()
	defer close(release)
	s.Stager.Source = slow.URL
	SaveState(s.Store.Dir, State{Current: "v2.6.0", LastGood: "v2.5.0"})

	done := make(chan int, 1)
	go func() {
		code, _ := call(s, http.MethodPost, meshapi.PathUpdateStage, `{"version":"v2.7.0"}`)
		done <- code
	}()
	<-got // the stage is downloading
	if code, body := call(s, http.MethodPost, meshapi.PathUpdateStage, `{"version":"v2.7.0"}`); code != 409 || !strings.Contains(body, "in progress") {
		t.Errorf("second stage: %d %s", code, body)
	}
	answered := make(chan int, 1)
	go func() {
		code, _ := call(s, http.MethodPost, meshapi.PathUpdateRollback, `{}`)
		answered <- code
	}()
	select {
	case code := <-answered:
		if code != 202 {
			t.Errorf("rollback during a stage: %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rollback waited for the stage")
	}
	waitRestarts(t, restarts, 1)
}
