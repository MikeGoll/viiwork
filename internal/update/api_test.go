package update

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

// waitRestarts waits for Restart, which the service calls in a goroutine
// after answering 202.
func waitRestarts(t *testing.T, n *atomic.Int32, want int32) {
	t.Helper()
	for i := 0; i < 100 && n.Load() < want; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if n.Load() != want {
		t.Fatalf("restarts = %d, want %d", n.Load(), want)
	}
}

type allow bool

func (a allow) Authorize(*http.Request, []byte) (int, string, bool) {
	if a {
		return 0, "", true
	}
	return http.StatusUnauthorized, "needs a signature", false
}

func service(t *testing.T, enabled bool, auth Authorizer) (*Service, *atomic.Int32) {
	t.Helper()
	dir := t.TempDir()
	src, pub := fakeRelease(t, "v2.6.0", releaseOpts{})
	restarts := &atomic.Int32{}
	return &Service{
		Enabled: enabled, Running: "v2.5.0", Store: NewStore(dir), Auth: auth,
		Stager:   stager(dir, src, pub),
		Backends: backends(map[string]string{"m/0": meshapi.StatusHealthy, "m/1": meshapi.StatusDead}, ""),
		Engines:  func() map[string]string { return map[string]string{"llamacpp": "b10437"} },
		Restart:  func() { restarts.Add(1) },
		Log:      func(string, ...any) {},
	}, restarts
}

func call(s *Service, method, path, body string) (int, string) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestStatus(t *testing.T) {
	s, _ := service(t, false, allow(true))
	code, body := call(s, http.MethodGet, meshapi.PathUpdate, "")
	var st meshapi.UpdateStatus
	json.Unmarshal([]byte(body), &st)
	if code != 200 || st.Enabled || st.Running != "v2.5.0" || st.Current != meshapi.UpdateBuiltin || st.Staged == nil || st.Engines["llamacpp"] != "b10437" {
		t.Fatalf("status %d %+v", code, st)
	}
}

func TestWritesNeedEnabledAndAuth(t *testing.T) {
	s, _ := service(t, false, allow(true))
	if code, body := call(s, http.MethodPost, meshapi.PathUpdateStage, `{"version":"v2.6.0"}`); code != 403 || !strings.Contains(body, "update.enabled") {
		t.Errorf("disabled: %d %s", code, body)
	}
	s, _ = service(t, true, allow(false))
	if code, _ := call(s, http.MethodPost, meshapi.PathUpdateStage, `{"version":"v2.6.0"}`); code != 401 {
		t.Errorf("unauthorised: %d", code)
	}
	s, _ = service(t, true, allow(true))
	if code, _ := call(s, http.MethodPost, meshapi.PathUpdateStage, strings.Repeat("x", 8<<10)); code != 413 {
		t.Errorf("huge body: %d", code)
	}
	if code, _ := call(s, http.MethodPost, meshapi.PathUpdateStage, `[1,2]`); code != 400 {
		t.Errorf("not an object: %d", code)
	}
	if code, _ := call(s, http.MethodPost, meshapi.PathUpdateStage, `{"version":"latest"}`); code != 400 {
		t.Errorf("not a version: %d", code)
	}
	if code, _ := call(s, http.MethodDelete, meshapi.PathUpdateStage, ""); code != 405 {
		t.Errorf("DELETE: %d", code)
	}
}

func TestStageActivateRollback(t *testing.T) {
	s, restarts := service(t, true, allow(true))
	if code, body := call(s, http.MethodPost, meshapi.PathUpdateActivate, `{"version":"v2.6.0"}`); code != 409 || !strings.Contains(body, "not staged") {
		t.Errorf("activate unstaged: %d %s", code, body)
	}
	if code, body := call(s, http.MethodPost, meshapi.PathUpdateStage, `{"version":"v2.6.0"}`); code != 200 {
		t.Fatalf("stage: %d %s", code, body)
	}
	if code, body := call(s, http.MethodPost, meshapi.PathUpdateActivate, `{"version":"v2.6.0"}`); code != 202 {
		t.Fatalf("activate: %d %s", code, body)
	}
	waitRestarts(t, restarts, 1)
	st, _ := LoadState(s.Store.Dir)
	if st.Current != "v2.6.0" || st.Pending == nil || len(st.Pending.Baseline) != 1 || st.Pending.Baseline[0] != "m/0" {
		t.Errorf("state after activate: %+v", st)
	}
	// One restart: everything else waits for it.
	if code, _ := call(s, http.MethodPost, meshapi.PathUpdateRollback, `{}`); code != 409 {
		t.Errorf("second write while restarting: %d", code)
	}
}

func TestActivateRefusesADowngrade(t *testing.T) {
	s, _ := service(t, true, allow(true))
	s.Running = "v2.7.0"
	call(s, http.MethodPost, meshapi.PathUpdateStage, `{"version":"v2.6.0"}`)
	if code, body := call(s, http.MethodPost, meshapi.PathUpdateActivate, `{"version":"v2.6.0"}`); code != 409 || !strings.Contains(body, "older") {
		t.Errorf("downgrade: %d %s", code, body)
	}
	if code, _ := call(s, http.MethodPost, meshapi.PathUpdateActivate, `{"version":"v2.6.0","allow_downgrade":true}`); code != 202 {
		t.Errorf("allowed downgrade: %d", code)
	}
}

func TestRollback(t *testing.T) {
	s, restarts := service(t, true, allow(true))
	if code, _ := call(s, http.MethodPost, meshapi.PathUpdateRollback, `{}`); code != 409 {
		t.Errorf("nothing to roll back: %d", code)
	}
	SaveState(s.Store.Dir, State{Current: "v2.6.0", LastGood: "v2.5.0"})
	if code, _ := call(s, http.MethodPost, meshapi.PathUpdateRollback, ``); code != 202 {
		t.Fatalf("rollback: %d", code)
	}
	waitRestarts(t, restarts, 1)
	if st, _ := LoadState(s.Store.Dir); st.Current != "v2.5.0" {
		t.Errorf("state: %+v", st)
	}
}
