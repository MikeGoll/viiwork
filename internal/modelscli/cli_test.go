package modelscli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/janit/viiwork/v2/internal/updatecli"
	"github.com/janit/viiwork/v2/meshapi"
)

// fakeNode is one node's /v1/models/down and /up.
type fakeNode struct {
	mu     sync.Mutex
	paths  []string
	bodies []meshapi.ParkRequest
	auth   []string
	ctype  []string
	parked map[string]bool
	models []string
	status int // 0 = answer as a node would
	msg    string
}

func (f *fakeNode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, r.URL.Path)
	f.auth = append(f.auth, r.Header.Get("X-Viiwork-Auth"))
	f.ctype = append(f.ctype, r.Header.Get("Content-Type"))
	var req meshapi.ParkRequest
	b, _ := io.ReadAll(r.Body)
	json.Unmarshal(b, &req)
	f.bodies = append(f.bodies, req)
	if f.status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		json.NewEncoder(w).Encode(meshapi.ErrorResponse{Error: meshapi.ErrorBody{Message: f.msg, Type: "invalid_request"}})
		return
	}
	down := r.URL.Path == meshapi.PathModelsDown
	names := req.Models
	if len(names) == 0 {
		names = f.models
	}
	resp := meshapi.ParkResponse{Node: "gb2"}
	for _, n := range names {
		resp.Models = append(resp.Models, meshapi.ModelPark{Name: n, Parked: down, Changed: f.parked[n] != down})
		f.parked[n] = down
	}
	json.NewEncoder(w).Encode(resp)
}

func run(t *testing.T, f *fakeNode, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	verb, rest := args[0], args[1:]
	rest = append([]string{"--node", strings.TrimPrefix(srv.URL, "http://")}, rest...)
	var out, errb bytes.Buffer
	code := Run(context.Background(), verb, rest, updatecli.Env{
		Stdout: &out, Stderr: &errb, Client: srv.Client(),
		LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
		Hostname:  func() (string, error) { return "test", nil },
	})
	return code, out.String(), errb.String()
}

func TestDownNamedModels(t *testing.T) {
	f := &fakeNode{parked: map[string]bool{}, models: []string{"m", "n", "o"}}
	code, out, errs := run(t, f, nil, "down", "m", "n")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if !reflect.DeepEqual(f.paths, []string{meshapi.PathModelsDown}) || !reflect.DeepEqual(f.bodies[0].Models, []string{"m", "n"}) {
		t.Fatalf("sent %v %+v", f.paths, f.bodies)
	}
	if f.ctype[0] != "application/json" {
		t.Errorf("Content-Type %q", f.ctype[0])
	}
	if f.auth[0] != "" {
		t.Errorf("signed without a secret: %q", f.auth[0])
	}
	for _, want := range []string{"gb2", "m", "n", "down"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "o ") {
		t.Errorf("output names a model it did not touch:\n%s", out)
	}

	// Again: nothing changes, and the output says so.
	_, out, _ = run(t, f, nil, "down", "m")
	if !strings.Contains(out, "already down") {
		t.Errorf("repeat down output:\n%s", out)
	}
}

func TestUpAllModels(t *testing.T) {
	f := &fakeNode{parked: map[string]bool{"m": true, "n": true}, models: []string{"m", "n"}}
	code, out, errs := run(t, f, nil, "up")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if f.paths[0] != meshapi.PathModelsUp || len(f.bodies[0].Models) != 0 {
		t.Fatalf("sent %v %+v", f.paths, f.bodies)
	}
	if !strings.Contains(out, "m") || !strings.Contains(out, "n") || !strings.Contains(out, "loading") {
		t.Errorf("output:\n%s", out)
	}
}

func TestSignsWithTheMeshSecret(t *testing.T) {
	f := &fakeNode{parked: map[string]bool{}, models: []string{"m"}}
	secret := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	if code, _, errs := run(t, f, map[string]string{"VIIWORK_MESH_SECRET": secret}, "down", "m"); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if f.auth[0] == "" {
		t.Error("a write with the mesh secret loaded went unsigned")
	}
	f2 := &fakeNode{parked: map[string]bool{}, models: []string{"m"}}
	if code, _, errs := run(t, f2, map[string]string{"OTHER": secret}, "down", "--secret-env", "OTHER", "m"); code != 0 || f2.auth[0] == "" {
		t.Errorf("--secret-env: exit %d, auth %q, %s", code, f2.auth, errs)
	}
}

func TestRefusalsAndUsage(t *testing.T) {
	f := &fakeNode{status: 400, msg: "not configured on this node: nope (configured: m); nothing changed"}
	code, _, errs := run(t, f, nil, "down", "nope")
	if code != 1 || !strings.Contains(errs, "nope") {
		t.Errorf("unknown model: exit %d, %s", code, errs)
	}

	f = &fakeNode{status: 401, msg: "alias writes need a valid X-Viiwork-Auth signature"}
	code, _, errs = run(t, f, nil, "up")
	if code != 1 || !strings.Contains(errs, "mesh secret") {
		t.Errorf("unsigned in a secured mesh: exit %d, %s", code, errs)
	}

	f = &fakeNode{parked: map[string]bool{}}
	if code, _, errs := run(t, f, nil, "down", "--bogus"); code != 2 || !strings.Contains(errs, "usage: viiwork down") {
		t.Errorf("bad flag: exit %d, %s", code, errs)
	}
	if len(f.paths) != 0 {
		t.Error("a usage error reached the node")
	}
	if code, _, _ := run(t, f, map[string]string{"VIIWORK_MESH_SECRET": "short"}, "down"); code != 2 {
		t.Errorf("a malformed secret: exit %d", code)
	}
}
