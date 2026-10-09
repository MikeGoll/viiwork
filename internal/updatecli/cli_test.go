package updatecli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

// fakeNode is one member's /v1/update. behave scripts what a GET returns
// after an activate: it is called with the number of GETs since then.
type fakeNode struct {
	name  string
	srv   *httptest.Server
	mesh  *fakeMesh
	mu    sync.Mutex
	st    meshapi.UpdateStatus
	gone  bool // /v1/update answers 404: an older node
	dead  bool // not alive in the cluster view
	stage int  // status code for stage; 0 = 200
	// stageTook is how long a stage takes to answer.
	stageTook time.Duration
	activate  int      // status code for activate; 0 = 202
	loading   int      // this many activates answer 409: a model is still loading
	already   bool     // activate answers 409: another rollout put it on the release
	models    []string // what its status lists as served
	parked    []string // what its status lists as parked (viiwork down)
	drop      bool     // the GET that set it, and every one while set, gets a closed connection
	behave    func(n *fakeNode, polls int)
	splits    bool // once on a new release, the entry node sees it dead: it lost the mesh
	polls     int
	calls     []string
	auth      []string // X-Viiwork-Auth of every write
}

type fakeMesh struct {
	nodes          []*fakeNode
	entry          *fakeNode
	mu             sync.Mutex
	order          []string // activations, in order
	gh             *httptest.Server
	ghTag          string
	ghSig          bool
	open           bool   // an open mesh
	noSec          bool   // the CLI has no mesh secret
	entryAdvertise string // the entry node's advertised API address, when not its real one
}

func (n *fakeNode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n.mu.Lock()
	defer n.mu.Unlock()
	switch {
	case r.URL.Path == meshapi.PathCluster && n == n.mesh.entry:
		json.NewEncoder(w).Encode(n.mesh.cluster())
	case n.gone:
		http.NotFound(w, r)
	case r.URL.Path == meshapi.PathUpdate && r.Method == http.MethodGet:
		if n.behave != nil {
			n.polls++
			n.behave(n, n.polls)
		}
		if n.drop {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		json.NewEncoder(w).Encode(n.st)
	case r.Method == http.MethodPost:
		var req meshapi.UpdateRequest
		json.NewDecoder(r.Body).Decode(&req)
		n.auth = append(n.auth, r.Header.Get("X-Viiwork-Auth"))
		switch r.URL.Path {
		case meshapi.PathUpdateStage:
			n.calls = append(n.calls, "stage "+req.Version)
			time.Sleep(n.stageTook)
			if n.stage != 0 {
				w.WriteHeader(n.stage)
				json.NewEncoder(w).Encode(meshapi.ErrorResponse{Error: meshapi.ErrorBody{Message: "stage refused", Type: "update_error"}})
				return
			}
			n.st.Staged = append(n.st.Staged, req.Version)
		case meshapi.PathUpdateActivate:
			if n.loading > 0 {
				n.loading--
				n.calls = append(n.calls, "activate refused: loading")
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(meshapi.ErrorResponse{Error: meshapi.ErrorBody{Message: "model m is still loading (backend m/0, warming up): activate when it is healthy", Type: "update_error"}})
				return
			}
			if n.already {
				n.st.Running, n.st.Current, n.st.LastGood = req.Version, req.Version, req.Version
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(meshapi.ErrorResponse{Error: meshapi.ErrorBody{Message: req.Version + " is already running", Type: "update_error"}})
				return
			}
			if n.activate != 0 {
				w.WriteHeader(n.activate)
				json.NewEncoder(w).Encode(meshapi.ErrorResponse{Error: meshapi.ErrorBody{Message: "activate refused", Type: "update_error"}})
				return
			}
			n.calls = append(n.calls, "activate "+req.Version)
			n.mesh.mu.Lock()
			n.mesh.order = append(n.mesh.order, n.name)
			n.mesh.mu.Unlock()
			n.st.Current = req.Version
			n.st.Pending = &meshapi.UpdatePending{Version: req.Version}
			n.polls = 0
			w.WriteHeader(http.StatusAccepted)
		case meshapi.PathUpdateRollback:
			n.calls = append(n.calls, "rollback")
			to := n.st.LastGood
			if n.st.Pending == nil && n.st.Current == n.st.LastGood && n.st.Previous != "" {
				to = n.st.Previous
			}
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]string{"rolling_back_to": to})
		}
	default:
		http.NotFound(w, r)
	}
}

// confirms: the old process answers once, then the new one pending, then
// confirmed.
func confirms(n *fakeNode, polls int) {
	if n.st.Pending == nil {
		return
	}
	to := n.st.Pending.Version
	switch {
	case polls == 1: // the old process, still shutting down
	case polls == 2:
		n.st.Running = to
		n.st.Pending.Deadline = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	default:
		n.st.Running, n.st.LastGood, n.st.Pending = to, to, nil
	}
}

// rollsBack: comes up on the target, then rolls itself back.
func rollsBack(n *fakeNode, polls int) {
	if n.st.Pending == nil {
		return
	}
	switch {
	case polls == 2:
		n.st.Running = n.st.Pending.Version
	case polls >= 3:
		n.st.Running, n.st.Current, n.st.Pending = n.st.LastGood, n.st.LastGood, nil
	}
}

func newFakeMesh(t *testing.T, names ...string) *fakeMesh {
	t.Helper()
	m := &fakeMesh{ghTag: "v2.6.0", ghSig: true}
	for _, name := range names {
		n := &fakeNode{name: name, mesh: m, behave: confirms,
			st: meshapi.UpdateStatus{Enabled: true, Running: "v2.5.0", Current: meshapi.UpdateBuiltin, LastGood: meshapi.UpdateBuiltin, Staged: []string{}}}
		n.srv = httptest.NewServer(n)
		t.Cleanup(n.srv.Close)
		m.nodes = append(m.nodes, n)
	}
	m.entry = m.nodes[len(m.nodes)-1]
	m.gh = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/janit/viiwork/releases/latest" {
			http.NotFound(w, r)
			return
		}
		assets := []map[string]string{{"name": "SHA256SUMS"}}
		if m.ghSig {
			assets = append(assets, map[string]string{"name": "SHA256SUMS.sig"})
		}
		json.NewEncoder(w).Encode(map[string]any{"tag_name": m.ghTag, "assets": assets})
	}))
	t.Cleanup(m.gh.Close)
	return m
}

func (m *fakeMesh) cluster() meshapi.ClusterResponse {
	c := meshapi.ClusterResponse{View: m.entry.name, Mesh: meshapi.MeshSecured}
	if m.open {
		c.Mesh = meshapi.MeshOpen
	}
	for _, n := range m.nodes {
		// The entry node's own lock is held by the request being served;
		// every other node may be answering a concurrent poll.
		if n != m.entry {
			n.mu.Lock()
		}
		addr := strings.TrimPrefix(n.srv.URL, "http://")
		if n == m.entry && m.entryAdvertise != "" {
			addr = m.entryAdvertise
		}
		host, port, _ := net.SplitHostPort(addr)
		p, _ := strconv.Atoi(port)
		mem := meshapi.Member{Node: n.name, Addr: host, Role: meshapi.RoleNode, State: meshapi.MemberAlive,
			Status: &meshapi.NodeStatus{Node: n.name, Addr: host, APIPort: p, Ver: n.st.Running}}
		for _, model := range n.models {
			mem.Status.Models = append(mem.Status.Models, meshapi.ModelStatus{Name: model, Slots: 1})
		}
		for _, model := range n.parked {
			mem.Status.Models = append(mem.Status.Models, meshapi.ModelStatus{Name: model, Parked: true, Backends: []meshapi.BackendStatus{}})
		}
		if n.dead || n.splits && n.st.Running != "v2.5.0" {
			mem.State, mem.Status = meshapi.MemberDead, nil
		}
		if n != m.entry {
			n.mu.Unlock()
		}
		c.Members = append(c.Members, mem)
	}
	c.Members = append(c.Members, meshapi.Member{Node: "gw", Role: meshapi.RoleGateway, State: meshapi.MemberAlive})
	return c
}

func (m *fakeMesh) node(name string) *fakeNode {
	for _, n := range m.nodes {
		if n.name == name {
			return n
		}
	}
	return nil
}

func run(t *testing.T, m *fakeMesh, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	secret := strings.Repeat("A", 43) + "=" // 32 zero bytes, standard base64
	code := Run(context.Background(), append(args, "--node", strings.TrimPrefix(m.entry.srv.URL, "http://")), Env{
		Stdout: &out, Stderr: &errOut, Stdin: strings.NewReader(stdin),
		LookupEnv: func(k string) (string, bool) {
			if k == "VIIWORK_MESH_SECRET" && !m.noSec {
				return secret, true
			}
			return "", false
		},
		Hostname:  func() (string, error) { return "testhost", nil },
		Client:    &http.Client{},
		ReadFile:  func(string) ([]byte, error) { return nil, io.EOF },
		GitHubAPI: m.gh.URL, PollEvery: 5 * time.Millisecond, ComeBack: 2 * time.Second,
	})
	return code, out.String(), errOut.String()
}

func TestStatus(t *testing.T) {
	m := newFakeMesh(t, "node-a", "node-b", "node-c")
	m.node("node-b").gone = true
	m.node("node-a").st.Engines = map[string]string{"llamacpp": "b10437"}
	m.node("node-c").st.Previous = "v2.4.1"
	code, out, errOut := run(t, m, "", "status")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"node-a", "v2.5.0", "llamacpp b10437", "node-b", "no /v1/update", "node-c", "PREVIOUS", "v2.4.1"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "gw") {
		t.Errorf("a gateway listed as a host:\n%s", out)
	}
	code, out, _ = run(t, m, "", "status", "--json")
	var js map[string]json.RawMessage
	if code != 0 || json.Unmarshal([]byte(out), &js) != nil || len(js) != 3 {
		t.Errorf("--json: %d %s", code, out)
	}
}

func TestRollbackOneHost(t *testing.T) {
	m := newFakeMesh(t, "node-a", "node-b")
	code, out, errOut := run(t, m, "", "rollback", "--host", "node-a")
	if code != 0 || !strings.Contains(out, "node-a") {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	a := m.node("node-a")
	if len(a.calls) != 1 || a.calls[0] != "rollback" || a.auth[0] == "" {
		t.Errorf("node-a got %v, auth %v", a.calls, a.auth)
	}
	if len(m.node("node-b").calls) != 0 {
		t.Error("another host was touched")
	}
	if code, _, errOut := run(t, m, "", "rollback", "--host", "nope"); code != 1 || !strings.Contains(errOut, "nope") {
		t.Errorf("unknown host: %d %s", code, errOut)
	}
	if code, _, _ := run(t, m, "", "rollback"); code != 2 {
		t.Errorf("rollback without --host: %d", code)
	}
}

// A host on a release it confirmed goes back to the one before it, and the
// CLI says where the node said it is going.
func TestRollbackPastAConfirmedRelease(t *testing.T) {
	m := newFakeMesh(t, "node-a")
	m.node("node-a").st.Previous = "v2.4.1"
	code, out, errOut := run(t, m, "", "rollback", "--host", "node-a")
	if code != 0 || !strings.Contains(out, "rolling back to v2.4.1") {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
}

func TestRollout(t *testing.T) {
	m := newFakeMesh(t, "node-a", "node-b", "node-c") // node-c is the entry node
	code, out, errOut := run(t, m, Phrase+"\n")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	for _, n := range m.nodes {
		if len(n.calls) != 2 || n.calls[0] != "stage v2.6.0" || n.calls[1] != "activate v2.6.0" {
			t.Errorf("%s got %v", n.name, n.calls)
		}
		for _, a := range n.auth {
			if a == "" {
				t.Errorf("%s: an unsigned write", n.name)
			}
		}
	}
	if got := strings.Join(m.order, ","); got != "node-a,node-b,node-c" {
		t.Errorf("activation order %s; the entry node must be last", got)
	}
}

func TestRolloutEntryNodeLast(t *testing.T) {
	m := newFakeMesh(t, "node-b", "node-c", "node-a")
	m.entry = m.node("node-a") // alphabetically first, but it is the one we talk to
	if code, out, errOut := run(t, m, Phrase+"\n", "--to", "v2.6.0"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if got := strings.Join(m.order, ","); got != "node-b,node-c,node-a" {
		t.Errorf("activation order %s", got)
	}
}

func TestRolloutNeedsThePhrase(t *testing.T) {
	m := newFakeMesh(t, "node-a", "node-b")
	for _, c := range []struct {
		stdin string
		args  []string
	}{
		{"yes\n", nil},
		{"", nil},
		{Phrase + "\n", []string{"--confirm", "yes"}},
	} {
		if code, _, _ := run(t, m, c.stdin, c.args...); code != 1 {
			t.Errorf("stdin %q args %v: exit %d", c.stdin, c.args, code)
		}
	}
	for _, n := range m.nodes {
		if len(n.calls) != 0 {
			t.Errorf("%s was written to without the phrase: %v", n.name, n.calls)
		}
	}
	if code, _, _ := run(t, m, "", "--confirm", Phrase); code != 0 {
		t.Errorf("--confirm with the phrase: exit %d", code)
	}
}

func TestRolloutStagesEverywhereFirst(t *testing.T) {
	m := newFakeMesh(t, "node-a", "node-b", "node-c")
	m.node("node-b").stage = http.StatusConflict
	code, _, errOut := run(t, m, Phrase+"\n")
	if code != 1 || !strings.Contains(errOut, "node-b") || !strings.Contains(errOut, "stage refused") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if len(m.order) != 0 {
		t.Errorf("activated %v although a stage failed", m.order)
	}
}

func TestRolloutStopsWhenAHostRollsBack(t *testing.T) {
	m := newFakeMesh(t, "node-a", "node-b", "node-c")
	m.node("node-a").behave = rollsBack
	m.node("node-a").st.LastGood = "v2.5.0"
	code, _, errOut := run(t, m, Phrase+"\n")
	if code != 1 || !strings.Contains(errOut, "node-a") || !strings.Contains(errOut, "rolled back") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if got := strings.Join(m.order, ","); got != "node-a" {
		t.Errorf("activations after a rollback: %s", got)
	}
}

func TestRolloutGivesUpOnAHostThatNeverReturns(t *testing.T) {
	m := newFakeMesh(t, "node-a", "node-b")
	a := m.node("node-a")
	a.behave = func(n *fakeNode, polls int) {
		if n.st.Pending != nil && polls >= 1 {
			n.gone = true // stops answering
		}
	}
	start := time.Now()
	code, _, errOut := run(t, m, Phrase+"\n")
	if code != 1 || !strings.Contains(errOut, "node-a") || time.Since(start) > 10*time.Second {
		t.Fatalf("exit %d after %v: %s", code, time.Since(start), errOut)
	}
}

func TestRolloutPlanSkips(t *testing.T) {
	m := newFakeMesh(t, "node-a", "node-b", "node-c", "node-d", "node-e", "node-f")
	m.node("node-a").st.Enabled = false
	m.node("node-b").st.Running = "v2.6.0"
	m.node("node-c").st.Running = "v2.7.0"
	m.node("node-d").gone = true
	m.node("node-e").dead = true
	code, out, errOut := run(t, m, Phrase+"\n")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"update.enabled is off", "already on v2.6.0", "newer than v2.6.0", "no /v1/update", "not alive"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
	if got := strings.Join(m.order, ","); got != "node-f" {
		t.Errorf("activated %s, want only node-f", got)
	}

	m2 := newFakeMesh(t, "node-a", "node-b", "node-c")
	if code, out, _ := run(t, m2, Phrase+"\n", "--hosts", "node-b"); code != 0 || !strings.Contains(out, "not selected") || strings.Join(m2.order, ",") != "node-b" {
		t.Errorf("--hosts: %d %v\n%s", code, m2.order, out)
	}
	m3 := newFakeMesh(t, "node-a", "node-b")
	a3 := m3.node("node-a")
	a3.st.Running, a3.st.Current, a3.st.LastGood = "v2.7.0", "v2.7.0", "v2.7.0" // on a staged release, so a downgrade sticks
	if code, _, _ := run(t, m3, Phrase+"\n", "--allow-downgrade"); code != 0 || strings.Join(m3.order, ",") != "node-a,node-b" {
		t.Errorf("--allow-downgrade: %d %v", code, m3.order)
	}
}

func TestRolloutRefusesAnUnsignedLatest(t *testing.T) {
	m := newFakeMesh(t, "node-a")
	m.ghSig = false
	code, _, errOut := run(t, m, Phrase+"\n")
	if code != 1 || !strings.Contains(errOut, "not signed") || len(m.node("node-a").calls) != 0 {
		t.Fatalf("exit %d: %s, calls %v", code, errOut, m.node("node-a").calls)
	}
	if code, _, _ := run(t, m, Phrase+"\n", "--to", "latest"); code != 2 {
		t.Errorf("--to latest: exit %d", code)
	}
}

// restarts is the path every real host takes: the old process answers once,
// then nothing answers while it restarts, then the new process is pending
// with no deadline yet, then with one, then confirmed.
func restarts(n *fakeNode, polls int) {
	if n.st.Pending == nil && n.st.LastGood != n.st.Current {
		return
	}
	to := n.st.Current
	n.drop = false
	switch {
	case polls == 1: // the old process
	case polls <= 3:
		n.drop = true
	case polls == 4:
		n.st.Running = to
	case polls == 5:
		n.st.Pending.Deadline = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	default:
		n.st.LastGood, n.st.Pending = to, nil
	}
}

func TestRolloutThroughARealRestart(t *testing.T) {
	m := newFakeMesh(t, "node-a", "node-b")
	for _, n := range m.nodes {
		n.behave = restarts
	}
	if code, out, errOut := run(t, m, Phrase+"\n"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if got := strings.Join(m.order, ","); got != "node-a,node-b" {
		t.Errorf("order %s", got)
	}
}

// In an open mesh a node accepts update writes only from its own machine,
// and the member list gives only advertised addresses, which are never
// loopback. So only the node the CLI talks to can be updated, through the
// address the CLI already uses for it.
func TestOpenMeshUpdatesOnlyThisMachine(t *testing.T) {
	m := newFakeMesh(t, "node-a", "node-b", "node-c")
	m.open = true
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(m.entry.srv.URL, "http://"))
	m.entryAdvertise = "127.0.0.2:" + port // nothing listens there
	code, out, errOut := run(t, m, Phrase+"\n")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if got := strings.Join(m.order, ","); got != "node-c" {
		t.Errorf("activated %s, want only this machine's node", got)
	}
	if !strings.Contains(out, "open mesh") {
		t.Errorf("the plan does not say why the others are skipped:\n%s", out)
	}
	if code, _, errOut := run(t, m, "", "rollback", "--host", "node-a"); code != 1 || !strings.Contains(errOut, "open mesh") {
		t.Errorf("rollback of another machine in an open mesh: %d %s", code, errOut)
	}
	if code, _, errOut := run(t, m, "", "rollback", "--host", "node-c"); code != 0 {
		t.Errorf("rollback of this machine: %d %s", code, errOut)
	}
}

// A secured mesh without the secret loaded would refuse every write; say so
// before asking for the phrase, not after.
func TestSecuredMeshNeedsTheSecretFirst(t *testing.T) {
	m := newFakeMesh(t, "node-a", "node-b")
	m.noSec = true
	code, out, errOut := run(t, m, Phrase+"\n")
	if code != 1 || !strings.Contains(errOut, "secret") || strings.Contains(out, "Type") {
		t.Fatalf("exit %d\nstdout %s\nstderr %s", code, out, errOut)
	}
	// The hint must work on a wizard install, whose mesh.env is root's.
	if !strings.Contains(errOut, "sudo sh -c") {
		t.Errorf("the hint does not load a root-only mesh.env: %s", errOut)
	}
	for _, n := range m.nodes {
		if len(n.calls) != 0 {
			t.Errorf("%s was written to: %v", n.name, n.calls)
		}
	}
}

func TestUnknownHostsAreRefused(t *testing.T) {
	m := newFakeMesh(t, "node-a", "node-b")
	code, _, errOut := run(t, m, Phrase+"\n", "--hosts", "node-a,node-zz")
	if code != 2 || !strings.Contains(errOut, "node-zz") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if len(m.order) != 0 || len(m.node("node-a").calls) != 0 {
		t.Error("a typo in --hosts still updated the rest")
	}
}

// Below a host's installed binary, a downgrade is undone at restart by the
// newer-floor rule: skip it instead of restarting the host for nothing.
func TestDowngradeBelowTheInstalledBinaryIsSkipped(t *testing.T) {
	m := newFakeMesh(t, "node-a", "node-b")
	m.node("node-a").st.Running = "v2.7.0" // the floor itself
	b := m.node("node-b")
	b.st.Running, b.st.Current, b.st.LastGood = "v2.7.0", "v2.7.0", "v2.7.0" // a staged release
	code, out, errOut := run(t, m, Phrase+"\n", "--allow-downgrade")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "installed binary is newer") {
		t.Errorf("plan:\n%s", out)
	}
	if got := strings.Join(m.order, ","); got != "node-b" {
		t.Errorf("activated %s, want node-b only", got)
	}
}

// The phrase must be true of what is on screen: with --hosts picking one
// machine, "update all nodes" was not (found rolling one host of seven).
func TestPhraseMatchesAPartialRollout(t *testing.T) {
	if strings.Contains(Phrase, "ALL") || !strings.Contains(Phrase, "ABOVE") {
		t.Errorf("Phrase %q must name the planned nodes, not all of them", Phrase)
	}
}

// On a Mac installed by viiwork init the mesh secret lives only in the node's
// LaunchAgent: the rollout CLI reads it from there, as join-code does, so a
// Mac can run `viiwork update` with nothing exported.
func TestSecretFromTheLaunchAgent(t *testing.T) {
	secret := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	plist := "<plist><dict><key>EnvironmentVariables</key><dict>" +
		"<key>VIIWORK_MESH_SECRET</key><string>" + secret + "</string></dict></dict></plist>"
	env := withDefaults(Env{
		Stdout: io.Discard, Stderr: io.Discard,
		LookupEnv: func(string) (string, bool) { return "", false },
		ReadFile: func(p string) ([]byte, error) {
			if p == "/Users/u/Library/LaunchAgents/fi.viiwork.node.plist" {
				return []byte(plist), nil
			}
			return nil, os.ErrNotExist
		},
		Plist: "/Users/u/Library/LaunchAgents/fi.viiwork.node.plist",
	})
	c, code := newCLI(env, "127.0.0.1:1", "", "")
	if c == nil || code != 0 || c.signer == nil {
		t.Fatalf("no signer from the plist: code %d", code)
	}
}

// A release that breaks mesh membership still loads its models, so the host
// confirms itself. The rollout must also see the host back in the mesh
// before moving on, or it would split the fleet one host at a time.
func TestRolloutStopsWhenAHostLeavesTheMesh(t *testing.T) {
	m := newFakeMesh(t, "node-a", "node-b", "node-c")
	m.node("node-a").splits = true
	code, _, errOut := run(t, m, "", "--to", "v2.6.0", "--confirm", Phrase)
	if code == 0 || !strings.Contains(errOut, "mesh") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	if strings.Contains(strings.Join(m.order, ","), "node-b") {
		t.Errorf("the next host was activated after one left the mesh: %v", m.order)
	}
}

func TestBuildWaves(t *testing.T) {
	for _, c := range []struct {
		name     string
		targets  []string
		entry    string
		parallel int
		serves   map[string][]string
		want     string // waves joined by " | ", hosts by ","
	}{
		{"one at a time is today's order", []string{"a", "b", "c"}, "c", 1, nil, "a | b | c"},
		{"waves of two, entry alone last", []string{"a", "b", "c", "d", "e"}, "e", 2, nil, "a,b | c,d | e"},
		{"entry alone even with room", []string{"a", "c", "b"}, "c", 8, nil, "a,b | c"},
		{"only the entry", []string{"c"}, "c", 4, nil, "c"},
		{"entry not in the rollout", []string{"a", "b", "c"}, "z", 2, nil, "a,b | c"},
		{"a model's two hosts never share a wave", []string{"a", "b", "c", "d"}, "d", 3,
			map[string][]string{"a": {"m"}, "b": {"m"}}, "a | b,c | d"},
		{"a host outside the rollout keeps the model up", []string{"a", "b", "c", "d"}, "d", 3,
			map[string][]string{"a": {"m"}, "b": {"m"}, "x": {"m"}}, "a,b,c | d"},
		{"a single-host model constrains nothing", []string{"a", "b", "c"}, "c", 2,
			map[string][]string{"a": {"m"}}, "a,b | c"},
		{"three hosts of one model, waves of three", []string{"a", "b", "c", "d", "e"}, "e", 3,
			map[string][]string{"a": {"m"}, "b": {"m"}, "c": {"m"}}, "a,b | c,d | e"},
		{"two models", []string{"a", "b", "c", "d"}, "z", 4,
			map[string][]string{"a": {"m", "n"}, "b": {"m"}, "c": {"n"}, "x": {"m"}}, "a,b | c,d"},
		{"the entry's models count too", []string{"a", "b", "c"}, "c", 2,
			map[string][]string{"a": {"m"}, "b": {"m"}, "c": {"m"}}, "a,b | c"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for _, w := range buildWaves(c.targets, c.entry, c.parallel, c.serves) {
				got = append(got, strings.Join(w, ","))
			}
			if g := strings.Join(got, " | "); g != c.want {
				t.Errorf("got %q, want %q", g, c.want)
			}
		})
	}
}

// Staging changes nothing a host runs, so every host stages at once.
func TestRolloutStagesInParallel(t *testing.T) {
	m := newFakeMesh(t, "node-a", "node-b", "node-c", "node-d")
	for _, n := range m.nodes {
		n.stageTook = 300 * time.Millisecond
	}
	start := time.Now()
	code, out, errOut := run(t, m, Phrase+"\n")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	// Serially the stages alone would take 1.2 s.
	if took := time.Since(start); took > 900*time.Millisecond {
		t.Errorf("rollout took %v; the stages did not overlap", took)
	}
	for _, n := range m.nodes {
		if !strings.Contains(out, n.name+": staged v2.6.0\n") {
			t.Errorf("no staged line for %s:\n%s", n.name, out)
		}
	}
}

func TestRolloutStageFailuresAreSummarised(t *testing.T) {
	m := newFakeMesh(t, "node-a", "node-b", "node-c")
	m.node("node-a").stage = http.StatusConflict
	m.node("node-c").stage = http.StatusConflict
	code, _, errOut := run(t, m, Phrase+"\n", "--parallel", "3")
	if code != 1 || !strings.Contains(errOut, "staging failed on node-a, node-c: nothing was activated") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if len(m.order) != 0 {
		t.Errorf("activated %v although a stage failed", m.order)
	}
}

func TestRolloutParallelRejectsBelowOne(t *testing.T) {
	m := newFakeMesh(t, "node-a")
	for _, p := range []string{"0", "-1"} {
		if code, _, errOut := run(t, m, Phrase+"\n", "--parallel", p); code != 2 || !strings.Contains(errOut, "--parallel") {
			t.Errorf("--parallel %s: exit %d %s", p, code, errOut)
		}
	}
	if len(m.node("node-a").calls) != 0 {
		t.Error("written to with a bad --parallel")
	}
}

// pendsUntil stays pending, with no deadline, until the mesh has activated
// want hosts, then confirms. Waiting on one host at a time would never see
// the second activation and would give up after ComeBack.
func pendsUntil(want int) func(*fakeNode, int) {
	return func(n *fakeNode, polls int) {
		if n.st.Pending == nil {
			return
		}
		n.mesh.mu.Lock()
		activated := len(n.mesh.order)
		n.mesh.mu.Unlock()
		n.st.Running = n.st.Pending.Version
		if activated >= want {
			n.st.LastGood, n.st.Pending = n.st.Running, nil
		}
	}
}

func TestRolloutInWaves(t *testing.T) {
	m := newFakeMesh(t, "node-a", "node-b", "node-c", "node-d", "node-e") // node-e is the entry
	m.node("node-a").behave = pendsUntil(2)
	m.node("node-b").behave = pendsUntil(2)
	code, out, errOut := run(t, m, Phrase+"\n", "--parallel", "2")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if got := strings.Join(m.order, ","); got != "node-a,node-b,node-c,node-d,node-e" {
		t.Errorf("activation order %s", got)
	}
	for _, want := range []string{"wave 1: node-a, node-b", "wave 2: node-c, node-d", "wave 3: node-e", "wave 1/3", "rollout of v2.6.0 complete"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// The plan, with its waves, comes before the phrase.
	if i, j := strings.Index(out, "wave 3: node-e"), strings.Index(out, "Type "); i < 0 || j < 0 || i > j {
		t.Errorf("the waves are not shown before the phrase:\n%s", out)
	}
}

// The capacity guard reads the models each member's status lists, counting
// members outside the rollout.
func TestRolloutWavesKeepEveryModelUp(t *testing.T) {
	m := newFakeMesh(t, "node-a", "node-b", "node-c", "node-d")
	m.node("node-a").models = []string{"qwen"}
	m.node("node-b").models = []string{"qwen"}
	code, out, errOut := run(t, m, Phrase+"\n", "--parallel", "3")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "wave 1: node-a\n") || !strings.Contains(out, "wave 2: node-b, node-c\n") {
		t.Errorf("node-a and node-b share a wave:\n%s", out)
	}
	// With a third host serving it, outside the rollout, they may.
	m2 := newFakeMesh(t, "node-a", "node-b", "node-c", "node-d", "node-x")
	m2.entry = m2.node("node-d")
	for _, n := range []string{"node-a", "node-b", "node-x"} {
		m2.node(n).models = []string{"qwen"}
	}
	m2.node("node-x").st.Enabled = false
	if code, out, errOut := run(t, m2, Phrase+"\n", "--parallel", "3"); code != 0 || !strings.Contains(out, "wave 1: node-a, node-b, node-c\n") {
		t.Errorf("exit %d\n%s\n%s", code, out, errOut)
	}
	// A third host that has the model parked (viiwork down) does not keep it up.
	m3 := newFakeMesh(t, "node-a", "node-b", "node-c", "node-d", "node-x")
	m3.entry = m3.node("node-d")
	m3.node("node-a").models = []string{"qwen"}
	m3.node("node-b").models = []string{"qwen"}
	m3.node("node-x").parked = []string{"qwen"}
	m3.node("node-x").st.Enabled = false
	if code, out, errOut := run(t, m3, Phrase+"\n", "--parallel", "3"); code != 0 || strings.Contains(out, "wave 1: node-a, node-b") {
		t.Errorf("a parked host counted as serving: exit %d\n%s\n%s", code, out, errOut)
	}
}

func TestRolloutStopsAfterTheFailedWave(t *testing.T) {
	m := newFakeMesh(t, "node-a", "node-b", "node-c", "node-d", "node-e")
	a := m.node("node-a")
	a.behave, a.st.LastGood = rollsBack, "v2.5.0"
	code, out, errOut := run(t, m, Phrase+"\n", "--parallel", "2")
	if code != 1 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	// The whole wave is waited for and reported; no later wave starts.
	if got := strings.Join(m.order, ","); got != "node-a,node-b" {
		t.Errorf("activations %s", got)
	}
	for _, want := range []string{"rollout stopped after wave 1/3", "node-a: it rolled back", "node-b: on v2.6.0, confirmed", "later waves untouched"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}
}

// An activate refused mid-wave leaves the rest of the wave alone, but the
// hosts already restarting are still followed to the end.
func TestRolloutActivateRefusedMidWave(t *testing.T) {
	m := newFakeMesh(t, "node-a", "node-b", "node-c", "node-d")
	m.node("node-b").activate = http.StatusConflict
	code, out, errOut := run(t, m, Phrase+"\n", "--parallel", "3")
	if code != 1 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if got := strings.Join(m.order, ","); got != "node-a" {
		t.Errorf("activations %s", got)
	}
	for _, want := range []string{"node-a: on v2.6.0, confirmed", "node-b: activate failed", "activate refused", "node-c: not activated"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}
}

// A host whose model is still loading refuses the activate with a 409 that
// ends by itself: the rollout asks again instead of stopping with the fleet
// on two versions.
func TestRolloutWaitsForALoadingModel(t *testing.T) {
	m := newFakeMesh(t, "node-a", "node-b", "node-c")
	m.node("node-b").loading = 2
	code, out, errOut := run(t, m, Phrase+"\n")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if got := strings.Join(m.order, ","); got != "node-a,node-b,node-c" {
		t.Errorf("activations %s", got)
	}
	if !strings.Contains(out, "node-b: model m is still loading") || !strings.Contains(out, "waiting up to") {
		t.Errorf("the wait was not said:\n%s", out)
	}
}

// A host another rollout activated in the meantime is done, not a failure.
func TestRolloutCountsAnAlreadyRunningHostAsDone(t *testing.T) {
	m := newFakeMesh(t, "node-a", "node-b", "node-c")
	m.node("node-a").already = true
	code, out, errOut := run(t, m, Phrase+"\n")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if got := strings.Join(m.order, ","); got != "node-b,node-c" {
		t.Errorf("activations %s", got)
	}
	if !strings.Contains(out, "node-a: already running v2.6.0") {
		t.Errorf("stdout:\n%s", out)
	}
}
