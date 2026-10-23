//go:build integration

package node

// kv_share over a real two-node mesh with the fake Strata: a session's turn
// on its home node is replicated to the member it would spill to, and a turn
// that runs there imports the replica before the engine sees it.

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/route"
	"github.com/janit/viiwork/v2/mesh/meshtest"
	"github.com/janit/viiwork/v2/meshapi"
)

// sessionFavouring is a session id whose rendezvous ranks home first.
func sessionFavouring(t *testing.T, home string, names ...string) (string, uint64) {
	t.Helper()
	for i := 0; i < 1000; i++ {
		s := "sess-" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('a'+i/26%26))
		key := fnv64(s)
		if route.SessionRank(key, names)[0] == home {
			return s, key
		}
	}
	t.Fatalf("no session favours %s", home)
	return "", 0
}

// fnv64 is the proxy's sessionKey hash.
func fnv64(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

func sessionTurn(t *testing.T, tn *testNode, model, session, host string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, tn.url(meshapi.PathChatCompletions)+"?host="+host,
		strings.NewReader(`{"model":"`+model+`","stream":true,"messages":[{"role":"user","content":"go on"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-Affinity", session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("turn on %s: %d %s", host, resp.StatusCode, body)
	}
	return resp.Header.Get(meshapi.HeaderNode)
}

func kvStats(t *testing.T, tn *testNode, model string) meshapi.KVShareStats {
	m, ok := modelOf(tn.status(t), model)
	if !ok || m.KVShare == nil {
		return meshapi.KVShareStats{}
	}
	return *m.KVShare
}

func readLog(path string) []string {
	b, _ := os.ReadFile(path)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func startStrataPair(t *testing.T, bEnv map[string]string) (a, b *testNode, logA, logB string) {
	net := meshtest.NewNetwork()
	dirA, dirB := t.TempDir(), t.TempDir()
	logA, logB = filepath.Join(dirA, "log"), filepath.Join(dirB, "log")
	a = startNodeYAML(t, net, "A", []string{strataModel(t, dirA, "q", 2000, logA, nil)}, "", seeded(net, "A"))
	b = startNodeYAML(t, net, "B", []string{strataModel(t, dirB, "q", 2000, logB, bEnv)}, "", seeded(net, "B", a))
	until(t, 10*time.Second, "A and B joined", allAlive(a, b))
	waitForModels(t, a, "q")
	waitForModels(t, b, "q")
	until(t, 10*time.Second, "each sees the other's group", func() bool {
		for _, tn := range []*testNode{a, b} {
			found := false
			for _, rep := range tn.capPoller.Reports() {
				for _, mc := range rep.Models {
					found = found || (rep.Node != tn.name && mc.KVShareGroup == "g" && mc.HealthyBackends > 0)
				}
			}
			if !found {
				return false
			}
		}
		return true
	})
	return a, b, logA, logB
}

func TestKVShareSpillResumesFromTheReplica(t *testing.T) {
	a, b, logA, logB := startStrataPair(t, nil)
	sess, _ := sessionFavouring(t, "A", "A", "B")

	if got := sessionTurn(t, a, "q", sess, "A"); got != "A" {
		t.Fatalf("the home turn ran on %s", got)
	}
	until(t, 15*time.Second, "A's turn reaches B", func() bool {
		return kvStats(t, b, "q").Puts == 1 && kvStats(t, a, "q").Pushes == 1
	})
	if s := kvStats(t, a, "q"); s.BytesSent == 0 || s.Group != "g" {
		t.Fatalf("A's stats %+v", s)
	}

	// the session spills to B, entering through A: B imports, then serves
	if got := sessionTurn(t, a, "q", sess, "B"); got != "B" {
		t.Fatalf("the spilled turn ran on %s", got)
	}
	lines := readLog(logB)
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "import ") || lines[1] != "chat "+sess {
		t.Fatalf("B's engine saw %q; want the import before the turn", lines)
	}
	if s := kvStats(t, b, "q"); s.Imports != 1 || s.ImportFails != 0 {
		t.Fatalf("B's stats %+v", s)
	}
	var capB meshapi.CapacityResponse
	getJSON(t, b.url(meshapi.PathCapacity), &capB)
	if len(capB.Models) != 1 || capB.Models[0].KVShareImportMsPer1k < 1 {
		t.Fatalf("B publishes no import cost after an import: %+v", capB.Models)
	}
	// B ran the newest turn, so it sends it back to the session's home
	until(t, 15*time.Second, "B's turn reaches A", func() bool { return kvStats(t, a, "q").Puts == 1 })
	if l := readLog(logA); l[0] != "chat "+sess || !strings.HasPrefix(l[1], "save ") {
		t.Fatalf("A's engine saw %q", l)
	}

	// back home: A imports B's newer state, and its save is a delta against
	// A's own first save, which B still holds
	if got := sessionTurn(t, a, "q", sess, "A"); got != "A" {
		t.Fatalf("the turn back home ran on %s", got)
	}
	until(t, 15*time.Second, "A's delta reaches B", func() bool { return kvStats(t, b, "q").Puts == 2 })
	l := readLog(logA)
	if len(l) < 5 || !strings.HasPrefix(l[2], "import ") || l[3] != "chat "+sess || !strings.HasPrefix(l[4], "save-delta "+sess) {
		t.Fatalf("A's engine saw %q; want import, the turn, then a delta save", l)
	}
}

func TestKVShareFailedImportServesCold(t *testing.T) {
	a, b, _, logB := startStrataPair(t, map[string]string{"FAKE_IMPORT_FAIL": "1"})
	sess, _ := sessionFavouring(t, "A", "A", "B")
	sessionTurn(t, a, "q", sess, "A")
	until(t, 15*time.Second, "A's turn reaches B", func() bool { return kvStats(t, b, "q").Puts == 1 })
	if got := sessionTurn(t, a, "q", sess, "B"); got != "B" {
		t.Fatalf("the spilled turn ran on %s", got)
	}
	lines := readLog(logB)
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "import-refused ") || lines[1] != "chat "+sess {
		t.Fatalf("B's engine saw %q", lines)
	}
	if s := kvStats(t, b, "q"); s.ImportFails != 1 || s.Imports != 0 {
		t.Fatalf("B's stats %+v", s)
	}
}

func TestKVShareRefusesNonMembers(t *testing.T) {
	a, _, _, _ := startStrataPair(t, nil)
	for _, tc := range []struct {
		name   string
		method string
		path   string
		header map[string]string
	}{
		{"manifest, no claim", http.MethodGet, meshapi.PathKVShareManifest + "?model=q&key=0000000000000001", nil},
		{"put, no claim", http.MethodPut, meshapi.PathKVSharePut + "?model=q&key=0000000000000001&size=10&digest=" + strings.Repeat("00", 32), nil},
		// open mesh: the claim must come from that member's address, and no
		// member is this node itself
		{"put claiming to be A itself", http.MethodPut, meshapi.PathKVSharePut + "?model=q&key=0000000000000001&size=10&digest=" + strings.Repeat("00", 32), map[string]string{meshapi.HeaderForwarded: "A"}},
		{"claiming an unknown member", http.MethodGet, meshapi.PathKVShareManifest + "?model=q&key=0000000000000001", map[string]string{meshapi.HeaderForwarded: "Z"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(tc.method, a.url(tc.path), strings.NewReader("x"))
			for k, v := range tc.header {
				req.Header.Set(k, v)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status %d, want 403", resp.StatusCode)
			}
		})
	}
}
