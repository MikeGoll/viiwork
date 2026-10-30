package node

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/mesh/meshtest"
	"github.com/janit/viiwork/v2/meshapi"
)

const nodeImageReq = `{"model":%q,"messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]}`

// One node, the real llamacpp engine, two models: one started with a
// projector and one without. The node says which reads images wherever it
// describes its models, sends an image to the one that does and refuses it
// for the one that does not.
func TestNodePublishesAndRoutesByImageInput(t *testing.T) {
	tn := startNode(t, meshtest.NewNetwork(), "n1", []fakeModel{
		{name: "eyes", args: []string{"--mmproj", "/models/mmproj.gguf"}},
		{name: "blind"},
	}, "", nil)
	waitForModels(t, tn, "eyes", "blind")

	// The answer is asked once the backend is healthy, so it can trail the
	// first slots by a moment.
	deadline := time.Now().Add(5 * time.Second)
	var cap map[string]meshapi.ModelCapacity
	for {
		cap = capacityOf(t, tn)
		if cap["eyes"].Images != nil && cap["blind"].Images != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no images answer within 5s: eyes %v blind %v", cap["eyes"].Images, cap["blind"].Images)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !*cap["eyes"].Images || *cap["blind"].Images {
		t.Fatalf("capacity images: eyes %v blind %v, want true and false", *cap["eyes"].Images, *cap["blind"].Images)
	}

	for _, m := range tn.status(t).Models {
		want := m.Name == "eyes"
		if m.Images == nil || *m.Images != want {
			t.Errorf("status %s: images = %v, want %v", m.Name, m.Images, want)
		}
	}

	var fleet meshapi.FleetCapacityResponse
	getJSON(t, tn.url(meshapi.PathFleetCapacity), &fleet)
	for _, m := range fleet.Models {
		want := m.Name == "eyes"
		if m.Images == nil || *m.Images != want {
			t.Errorf("fleet %s: images = %v, want %v", m.Name, m.Images, want)
		}
	}

	var catalog map[string]struct {
		Models map[string]struct {
			Modalities struct {
				Input []string `json:"input"`
			} `json:"modalities"`
		} `json:"models"`
	}
	getJSON(t, tn.url("/api.json"), &catalog)
	for _, p := range catalog {
		if in := strings.Join(p.Models["eyes"].Modalities.Input, ","); in != "text,image" {
			t.Errorf("catalogue eyes input = %q, want text,image", in)
		}
		if in := strings.Join(p.Models["blind"].Modalities.Input, ","); in != "text" {
			t.Errorf("catalogue blind input = %q, want text", in)
		}
	}

	post := func(model string) (int, string) {
		resp, err := http.Post(tn.url("/v1/chat/completions"), "application/json", strings.NewReader(strings.Replace(nodeImageReq, "%q", `"`+model+`"`, 1)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := post("eyes"); code != http.StatusOK {
		t.Errorf("image to eyes: %d %s", code, body)
	}
	code, body := post("blind")
	var e meshapi.ErrorResponse
	json.Unmarshal([]byte(body), &e)
	if code != http.StatusBadRequest || e.Error.Message != `model "blind" does not read images on any node` {
		t.Errorf("image to blind: %d %s", code, body)
	}
}
