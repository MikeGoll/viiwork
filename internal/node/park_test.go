package node

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/alias"
	"github.com/janit/viiwork/v2/internal/meshauth"
	"github.com/janit/viiwork/v2/mesh/meshtest"
	"github.com/janit/viiwork/v2/meshapi"
)

// postPark sends a down or up to tn from loopback, as `viiwork down` on the
// node's own machine does in an open mesh.
func postPark(t *testing.T, tn *testNode, path, body string) (int, meshapi.ParkResponse, string) {
	t.Helper()
	resp, err := http.Post(tn.url(path), "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out meshapi.ParkResponse
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out, string(b)
}

func allGone(pids []int) bool {
	for _, pid := range pids {
		if pid != 0 && !processGone(pid) {
			return false
		}
	}
	return true
}

func TestParkDownAndUp(t *testing.T) {
	tn := startNode(t, meshtest.NewNetwork(), "n1", []fakeModel{{name: "m"}, {name: "n"}}, "", nil)
	until(t, 10*time.Second, "m and n healthy", func() bool {
		st := tn.status(t)
		return healthyModel(st, "m") && healthyModel(st, "n")
	})
	mPIDs, nPIDs := pidsOf(tn.status(t), "m"), pidsOf(tn.status(t), "n")

	code, resp, body := postPark(t, tn, meshapi.PathModelsDown, `{"models":["m"]}`)
	if code != 200 || resp.Node != "n1" || !reflect.DeepEqual(resp.Models, []meshapi.ModelPark{{Name: "m", Parked: true, Changed: true}}) {
		t.Fatalf("down m: %d %s", code, body)
	}
	until(t, 10*time.Second, "m's engine processes gone", func() bool { return allGone(mPIDs) })

	st := tn.status(t)
	m, ok := modelOf(st, "m")
	if !ok || !m.Parked || len(m.Backends) != 0 || m.Slots != 0 || m.Engine != "llamacpp" {
		t.Errorf("a parked model's status = %+v (listed %v), want parked with no backends", m, ok)
	}
	if !healthyModel(st, "n") || !reflect.DeepEqual(pidsOf(st, "n"), nPIDs) {
		t.Errorf("down m touched n: %+v", st.Models)
	}
	if n, _ := modelOf(st, "n"); n.Parked {
		t.Error("n reads as parked")
	}
	if c, ok := capacityOf(t, tn)["m"]; !ok || c.Slots != 0 || c.HealthyBackends != 0 || c.Backends != 0 {
		t.Errorf("a parked model's capacity = %+v (listed %v), want listed with no slots", c, ok)
	}
	if _, ok := modelsListed(t, tn)["m"]; !ok {
		t.Error("/v1/models dropped the parked model: it should look like a model with no healthy backend")
	}
	if code := getJSON(t, tn.url(meshapi.PathHealth), nil); code != 200 {
		t.Errorf("/health = %d with n healthy and m parked", code)
	}

	// A second down changes nothing.
	if code, resp, body := postPark(t, tn, meshapi.PathModelsDown, `{"models":["m"]}`); code != 200 || resp.Models[0].Changed {
		t.Errorf("repeat down: %d %s", code, body)
	}

	code, resp, body = postPark(t, tn, meshapi.PathModelsUp, `{"models":["m"]}`)
	if code != 200 || !reflect.DeepEqual(resp.Models, []meshapi.ModelPark{{Name: "m", Parked: false, Changed: true}}) {
		t.Fatalf("up m: %d %s", code, body)
	}
	until(t, 10*time.Second, "m healthy again", func() bool { return healthyModel(tn.status(t), "m") })
	if m, _ := modelOf(tn.status(t), "m"); m.Parked {
		t.Error("m still reads as parked after up")
	}
	if !reflect.DeepEqual(pidsOf(tn.status(t), "n"), nPIDs) {
		t.Error("up m restarted n")
	}
}

func TestParkEmptyListMeansAll(t *testing.T) {
	tn := startNode(t, meshtest.NewNetwork(), "n1", []fakeModel{{name: "m"}, {name: "n"}}, "", nil)
	until(t, 10*time.Second, "m and n healthy", func() bool {
		st := tn.status(t)
		return healthyModel(st, "m") && healthyModel(st, "n")
	})
	pids := append(pidsOf(tn.status(t), "m"), pidsOf(tn.status(t), "n")...)

	code, resp, body := postPark(t, tn, meshapi.PathModelsDown, `{}`)
	if code != 200 || len(resp.Models) != 2 {
		t.Fatalf("down all: %d %s", code, body)
	}
	until(t, 10*time.Second, "every engine process gone", func() bool { return allGone(pids) })
	for _, name := range []string{"m", "n"} {
		if m, _ := modelOf(tn.status(t), name); !m.Parked {
			t.Errorf("%s not parked", name)
		}
	}
	// Every model parked on purpose is a node serving nothing by choice, not a
	// broken one: /health stays 200, as for a node with no models.
	if code := getJSON(t, tn.url(meshapi.PathHealth), nil); code != 200 {
		t.Errorf("/health = %d with every model parked", code)
	}

	// A bodiless up is the same as an empty list.
	if code, resp, body := postPark(t, tn, meshapi.PathModelsUp, ``); code != 200 || len(resp.Models) != 2 {
		t.Fatalf("up all: %d %s", code, body)
	}
	until(t, 10*time.Second, "m and n healthy again", func() bool {
		st := tn.status(t)
		return healthyModel(st, "m") && healthyModel(st, "n")
	})
}

func TestParkUnknownModelChangesNothing(t *testing.T) {
	tn := startNode(t, meshtest.NewNetwork(), "n1", []fakeModel{{name: "m"}}, "", nil)
	until(t, 10*time.Second, "m healthy", func() bool { return healthyModel(tn.status(t), "m") })
	pids := pidsOf(tn.status(t), "m")

	code, _, body := postPark(t, tn, meshapi.PathModelsDown, `{"models":["m","nope","zilch"]}`)
	if code != 400 || !strings.Contains(body, "nope") || !strings.Contains(body, "zilch") {
		t.Fatalf("down with unknown names: %d %s", code, body)
	}
	time.Sleep(300 * time.Millisecond)
	st := tn.status(t)
	if m, _ := modelOf(st, "m"); m.Parked || !healthyModel(st, "m") || !reflect.DeepEqual(pidsOf(st, "m"), pids) {
		t.Errorf("a refused down changed m: %+v", m)
	}
	if code, _, _ := postPark(t, tn, meshapi.PathModelsDown, `{"models":`); code != 400 {
		t.Errorf("a malformed body: %d", code)
	}
	resp, err := http.Get(tn.url(meshapi.PathModelsDown))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET %s = %d, want 405", meshapi.PathModelsDown, resp.StatusCode)
	}
}

func TestParkSurvivesReload(t *testing.T) {
	m, n := fakeModel{name: "m"}, fakeModel{name: "n"}
	tn := startNode(t, meshtest.NewNetwork(), "n1", []fakeModel{m}, "", nil)
	until(t, 10*time.Second, "m healthy", func() bool { return healthyModel(tn.status(t), "m") })
	if code, _, body := postPark(t, tn, meshapi.PathModelsDown, `{"models":["m"]}`); code != 200 {
		t.Fatalf("down: %d %s", code, body)
	}

	// Adding n starts n and leaves m parked.
	writeFile(t, tn.cfgPath, nodeConfig("n1", tn.stateDir, []fakeModel{m, n}, ""))
	if err := tn.Reload(); err != nil {
		t.Fatal(err)
	}
	until(t, 10*time.Second, "n healthy", func() bool { return healthyModel(tn.status(t), "n") })
	if st, _ := modelOf(tn.status(t), "m"); !st.Parked || len(st.Backends) != 0 {
		t.Fatalf("a reload unparked m: %+v", st)
	}

	// Changing m's config keeps it parked: up loads the new one.
	m.args = []string{"--seed", "1"}
	writeFile(t, tn.cfgPath, nodeConfig("n1", tn.stateDir, []fakeModel{m, n}, ""))
	if err := tn.Reload(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if st, _ := modelOf(tn.status(t), "m"); !st.Parked || len(st.Backends) != 0 {
		t.Fatalf("a changed config unparked m: %+v", st)
	}

	// Removing m from the config drops it from the parked set: added back,
	// it starts.
	writeFile(t, tn.cfgPath, nodeConfig("n1", tn.stateDir, []fakeModel{n}, ""))
	if err := tn.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, ok := modelOf(tn.status(t), "m"); ok {
		t.Fatal("a model removed from the config is still listed")
	}
	writeFile(t, tn.cfgPath, nodeConfig("n1", tn.stateDir, []fakeModel{m, n}, ""))
	if err := tn.Reload(); err != nil {
		t.Fatal(err)
	}
	until(t, 10*time.Second, "m healthy after being configured again", func() bool { return healthyModel(tn.status(t), "m") })
}

func TestParkDrainsInFlight(t *testing.T) {
	tn := startNode(t, meshtest.NewNetwork(), "n1", []fakeModel{{name: "m", hold: "1s"}}, "", nil)
	until(t, 10*time.Second, "m healthy", func() bool { return healthyModel(tn.status(t), "m") })
	pids := pidsOf(tn.status(t), "m")

	done := make(chan int, 1)
	go func() {
		resp, err := http.Post(tn.url(meshapi.PathChatCompletions), "application/json", strings.NewReader(`{"model":"m"}`))
		if err != nil {
			done <- 0
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	until(t, 5*time.Second, "the request in flight", func() bool { m, _ := modelOf(tn.status(t), "m"); return m.Busy > 0 })
	if code, _, body := postPark(t, tn, meshapi.PathModelsDown, `{"models":["m"]}`); code != 200 {
		t.Fatalf("down: %d %s", code, body)
	}
	select {
	case code := <-done:
		if code != 200 {
			t.Errorf("the in-flight request ended %d, want 200: parking must drain it", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight request did not finish")
	}
	until(t, 10*time.Second, "m's engine processes gone", func() bool { return allGone(pids) })

	// A new request finds no slot on this node, like any model with no
	// healthy backend: it queues, then times out.
	resp, err := http.Post(tn.url(meshapi.PathChatCompletions), "application/json", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == 200 || resp.StatusCode == 404 {
		t.Errorf("a request for a parked model = %d, want a no-slot answer", resp.StatusCode)
	}
}

func modelsListed(t *testing.T, tn *testNode) map[string]string {
	t.Helper()
	var resp meshapi.ModelsResponse
	getJSON(t, tn.url(meshapi.PathModels), &resp)
	out := map[string]string{}
	for _, e := range resp.Data {
		out[e.ID] = e.OwnedBy
	}
	return out
}

// fakePark records what the handler asked of the node.
type fakePark struct {
	calls int
	last  []string
	down  bool
}

func (f *fakePark) park(names []string, down bool) (meshapi.ParkResponse, error) {
	f.calls++
	f.last, f.down = names, down
	if len(names) == 1 && names[0] == "nope" {
		return meshapi.ParkResponse{}, &unknownModelsError{names: []string{"nope"}}
	}
	return meshapi.ParkResponse{Node: "n1"}, nil
}

func TestParkHandlerAuthorization(t *testing.T) {
	secret := bytes.Repeat([]byte{7}, 32)

	t.Run("secured: unsigned refused", func(t *testing.T) {
		auth, err := alias.NewAuthorizer("n1", secret, nil)
		if err != nil {
			t.Fatal(err)
		}
		f := &fakePark{}
		h := &parkHandler{auth: auth, park: f.park}
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8086"+meshapi.PathModelsDown, strings.NewReader(`{}`))
		req.RemoteAddr = "127.0.0.1:5555"
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized || f.calls != 0 {
			t.Errorf("unsigned in a secured mesh: %d, %d calls", rec.Code, f.calls)
		}
	})

	t.Run("secured: signed accepted", func(t *testing.T) {
		auth, err := alias.NewAuthorizer("n1", secret, nil)
		if err != nil {
			t.Fatal(err)
		}
		f := &fakePark{}
		h := &parkHandler{auth: auth, park: f.park}
		body := []byte(`{"models":["m"]}`)
		req := httptest.NewRequest(http.MethodPost, "http://10.0.0.9:8086"+meshapi.PathModelsUp, bytes.NewReader(body))
		req.RemoteAddr = "10.0.0.5:5555"
		req.Header.Set("Content-Type", "application/json")
		signer, err := meshauth.NewSigner(secret, "viiwork-cli@test")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := signer.SignRequest(req, body); err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || f.calls != 1 || f.down || !reflect.DeepEqual(f.last, []string{"m"}) {
			t.Errorf("signed up: %d %s, calls %d down %v last %v", rec.Code, rec.Body, f.calls, f.down, f.last)
		}
	})

	t.Run("open: not from this machine refused", func(t *testing.T) {
		auth, _ := alias.NewAuthorizer("n1", nil, nil)
		f := &fakePark{}
		h := &parkHandler{auth: auth, park: f.park}
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8086"+meshapi.PathModelsDown, strings.NewReader(`{}`))
		req.RemoteAddr = "100.64.0.7:5555"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden || f.calls != 0 {
			t.Errorf("non-loopback in an open mesh: %d, %d calls", rec.Code, f.calls)
		}
	})

	t.Run("open: loopback accepted, unknown is 400", func(t *testing.T) {
		auth, _ := alias.NewAuthorizer("n1", nil, nil)
		f := &fakePark{}
		h := &parkHandler{auth: auth, park: f.park}
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8086"+meshapi.PathModelsDown, strings.NewReader(`{"models":["nope"]}`))
		req.RemoteAddr = "127.0.0.1:5555"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "nope") || !f.down {
			t.Errorf("unknown model: %d %s", rec.Code, rec.Body)
		}
	})
}

func TestParkNeedsJSON(t *testing.T) {
	auth, _ := alias.NewAuthorizer("n1", nil, nil)
	f := &fakePark{}
	srv := NewServer(ServerDeps{Self: "n1", Park: &parkHandler{auth: auth, park: f.park}})
	for _, path := range []string{meshapi.PathModelsDown, meshapi.PathModelsUp} {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8086"+path, strings.NewReader(`{}`))
		req.RemoteAddr = "127.0.0.1:5555"
		req.Header.Set("Content-Type", "text/plain")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("%s as text/plain: %d", path, rec.Code)
		}
	}
	if f.calls != 0 {
		t.Errorf("a refused write reached the node %d times", f.calls)
	}
}
