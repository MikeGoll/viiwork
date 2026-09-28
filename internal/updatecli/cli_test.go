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
	name   string
	srv    *httptest.Server
	mesh   *fakeMesh
	mu     sync.Mutex
	st     meshapi.UpdateStatus
	gone   bool // /v1/update answers 404: an older node
	dead   bool // not alive in the cluster view
	stage  int  // status code for stage; 0 = 200
	drop   bool // the GET that set it, and every one while set, gets a closed connection
	behave func(n *fakeNode, polls int)
	splits bool // once on a new release, the entry node sees it dead: it lost the mesh
	polls  int
	calls  []string
	auth   []string // X-Viiwork-Auth of every write
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
			if n.stage != 0 {
				w.WriteHeader(n.stage)
				json.NewEncoder(w).Encode(meshapi.ErrorResponse{Error: meshapi.ErrorBody{Message: "stage refused", Type: "update_error"}})
				return
			}
			n.st.Staged = append(n.st.Staged, req.Version)
		case meshapi.PathUpdateActivate:
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
			w.WriteHeader(http.StatusAccepted)
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
		addr := strings.TrimPrefix(n.srv.URL, "http://")
		if n == m.entry && m.entryAdvertise != "" {
			addr = m.entryAdvertise
		}
		host, port, _ := net.SplitHostPort(addr)
		p, _ := strconv.Atoi(port)
		mem := meshapi.Member{Node: n.name, Addr: host, Role: meshapi.RoleNode, State: meshapi.MemberAlive,
			Status: &meshapi.NodeStatus{Node: n.name, Addr: host, APIPort: p, Ver: n.st.Running}}
		if n.dead || n.splits && n.st.Running != "v2.5.0" {
			mem.State, mem.Status = meshapi.MemberDead, nil
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
	code, out, errOut := run(t, m, "", "status")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"node-a", "v2.5.0", "llamacpp b10437", "node-b", "no /v1/update", "node-c"} {
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
