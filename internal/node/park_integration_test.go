//go:build integration

package node

import (
	"testing"
	"time"

	"github.com/janit/viiwork/v2/mesh/meshtest"
	"github.com/janit/viiwork/v2/meshapi"
)

// Two nodes serve m. Parked on A, a request entering A runs on B; unparked,
// A serves it again once healthy. A stays in the mesh throughout.
func TestParkRoutesElsewhere(t *testing.T) {
	net := meshtest.NewNetwork()
	a := startNode(t, net, "A", []fakeModel{{name: "m"}}, "", seeded(net, "A"))
	b := startNode(t, net, "B", []fakeModel{{name: "m"}}, "", seeded(net, "B", a))
	until(t, 10*time.Second, "A and B joined", allAlive(a, b))
	until(t, 10*time.Second, "m healthy on A and B", func() bool {
		return healthyModel(a.status(t), "m") && healthyModel(b.status(t), "m")
	})
	aPIDs := pidsOf(a.status(t), "m")

	if code, _, body := postPark(t, a, meshapi.PathModelsDown, `{"models":["m"]}`); code != 200 {
		t.Fatalf("down on A: %d %s", code, body)
	}
	until(t, 10*time.Second, "A's engine gone", func() bool { return allGone(aPIDs) })
	for i := 0; i < 5; i++ {
		r := chat(t, a, "m")
		if r.status != 200 || r.header.Get(meshapi.HeaderNode) != "B" {
			t.Fatalf("request %d entering A with m parked there: %d ran on %q %s", i, r.status, r.header.Get(meshapi.HeaderNode), r.body)
		}
	}
	if !allAlive(a, b)() {
		t.Error("parking took A out of the mesh")
	}
	// B sees A list m with no slots, and keeps routing to itself.
	until(t, 5*time.Second, "B sees A's m with no slots", func() bool {
		for _, rep := range b.capPoller.Reports() {
			if rep.Node == "A" {
				mc, ok := rep.Model("m")
				return ok && mc.Slots == 0 && mc.HealthyBackends == 0
			}
		}
		return false
	})
	if r := chat(t, b, "m"); r.status != 200 || r.header.Get(meshapi.HeaderNode) != "B" {
		t.Errorf("entering B: %d on %q", r.status, r.header.Get(meshapi.HeaderNode))
	}

	if code, _, body := postPark(t, a, meshapi.PathModelsUp, `{"models":["m"]}`); code != 200 {
		t.Fatalf("up on A: %d %s", code, body)
	}
	until(t, 10*time.Second, "m healthy on A again", func() bool { return healthyModel(a.status(t), "m") })
	// With B parked now, only A can serve: a request entering B runs on A.
	if code, _, body := postPark(t, b, meshapi.PathModelsDown, `{}`); code != 200 {
		t.Fatalf("down on B: %d %s", code, body)
	}
	until(t, 10*time.Second, "a request entering B runs on A", func() bool {
		r := chat(t, b, "m")
		return r.status == 200 && r.header.Get(meshapi.HeaderNode) == "A"
	})
	if r := chat(t, a, "m"); r.status != 200 || r.header.Get(meshapi.HeaderNode) != "A" {
		t.Errorf("entering A after up: %d on %q", r.status, r.header.Get(meshapi.HeaderNode))
	}
}
