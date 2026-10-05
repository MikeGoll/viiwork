//go:build integration

package node

// Performance routing over a real three-node mesh: each node measures its own
// entry traffic, publishes the score, and an entry node then sends most
// requests to the fast node. Degradation within the window is covered by
// internal/perf's unit tests with a fake clock; here it would take minutes.

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/mesh/meshtest"
	"github.com/janit/viiwork/v2/meshapi"
)

func streamOnce(t *testing.T, tn *testNode) string {
	t.Helper()
	return streamTo(t, tn, "")
}

// streamTo is streamOnce with an optional ?host= pin, used to force a warm-up
// request to run on the node it is sent to (see the comment in
// TestPerformanceRoutingPrefersTheFastNode) without going through the scored
// choice at all.
func streamTo(t *testing.T, tn *testNode, host string) string {
	t.Helper()
	pad := strings.Repeat("x", 8000) // EstK = 2, matching FAKE_PROMPT_TOKENS
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"` + pad + `"}]}`
	url := tn.url("/v1/chat/completions")
	if host != "" {
		url += "?host=" + host
	}
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := new(strings.Builder)
	_, _ = io.Copy(buf, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %s: %s", resp.Status, buf)
	}
	// The fake sends usage whether or not it was asked, like an engine with
	// Unasked true, so whether the client sees it here depends on Task 0's
	// answer. Stripping is pinned byte for byte in internal/proxy instead.
	return resp.Header.Get(meshapi.HeaderNode)
}

func TestPerformanceRoutingPrefersTheFastNode(t *testing.T) {
	net := meshtest.NewNetwork()
	slow := []fakeModel{{name: "m", parallel: 4, prefillPer1k: "100ms", promptTokens: "2000"}}
	fast := []fakeModel{{name: "m", parallel: 4, prefillPer1k: "5ms", promptTokens: "2000"}}
	a := startNode(t, net, "A", slow, "", seeded(net, "A"))
	b := startNode(t, net, "B", fast, "", seeded(net, "B", a))
	c := startNode(t, net, "C", slow, "", seeded(net, "C", a, b))

	// Wait for each node's own backend to reach healthy before sending it
	// anything: Ready() only means the API and mesh are up, and a node whose
	// local backend is still starting would forward the warm-up request to
	// whichever peer already reports the model, tripping the check below for
	// a reason that has nothing to do with routing.
	for _, tn := range []*testNode{a, b, c} {
		waitForModels(t, tn, "m")
	}

	// Each node measures its own entry traffic first. streamOnce's unpinned
	// request stays local only until ANY node anywhere has a published score:
	// one local sample of >= 512 uncached tokens already clears internal/perf's
	// BigTokens gate, so as soon as the first of these three warm-up requests
	// completes, the scored choice lights up mesh-wide for this model and
	// starts weighing every fresh candidate for every node's traffic,
	// including a node's own still-unscored self against an already-scored
	// peer. Concretely: once B's warm-up establishes its real ~10 ms score,
	// C's own unpinned warm-up requests lose to B almost every time, so C can
	// spend many requests without ever landing a genuine local sample of its
	// own. A ?host= pin bypasses the scored choice entirely (it is compared,
	// never weighed) and goes straight to the deterministic local-first path,
	// so it is used here to get each node a handful of real local samples
	// without racing the very feature under test.
	for _, tn := range []*testNode{a, b, c} {
		for i := 0; i < 3; i++ {
			if got := streamTo(t, tn, tn.Node.name); got != tn.Node.name {
				t.Fatalf("warm-up on %s pinned to itself ran on %s", tn.Node.name, got)
			}
		}
	}
	time.Sleep(time.Second) // several capacity polls (250 ms) carry the scores

	counts := map[string]int{}
	for i := 0; i < 40; i++ {
		counts[streamOnce(t, a)]++
	}
	t.Logf("served by %v", counts)
	if counts["B"] < 30 {
		t.Fatalf("served by %v; B predicts ~10 ms against ~200 ms elsewhere and must take most requests", counts)
	}
}
