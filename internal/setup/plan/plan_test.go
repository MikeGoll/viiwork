package plan

import (
	"slices"
	"testing"

	"github.com/janit/viiwork/v2/internal/gguf"
)

const gib = 1 << 30

// model is a GGUF of weightsGiB with a llama-7B-like KV cache: 32 layers,
// 8 KV heads of 128 — 128 KiB per token.
func model(name string, weightsGiB float64, trained int) Model {
	return Model{Name: name, Info: gguf.Info{Arch: "llama", Layers: 32, HeadCountKV: 8, KVHeads: 32 * 8, HeadDim: 128,
		ContextLength: trained, Size: int64(weightsGiB * gib)}}
}

func cards(vramGiB ...int) []GPU {
	var g []GPU
	for i, v := range vramGiB {
		g = append(g, GPU{Index: i, VRAMMB: int64(v) * 1024})
	}
	return g
}

func find(p Plan, name string) (Placement, bool) {
	for _, pl := range p.Placed {
		if pl.Name == name {
			return pl, true
		}
	}
	return Placement{}, false
}

func TestDiscreteOneCardModelGetsReplicas(t *testing.T) {
	p := Discrete(cards(16, 16, 16, 16), []Model{model("small", 5, 131072)})
	pl, ok := find(p, "small")
	if !ok || pl.PerBackend != 1 || !slices.Equal(pl.GPUs, []int{0, 1, 2, 3}) {
		t.Fatalf("placement %+v", pl)
	}
	// 16 GiB − 1 GiB overhead − 5 GiB weights = 10 GiB for 2 slots of KV at
	// 128 KiB/token: 40960 tokens per slot fits 32768, not 49152.
	if pl.Context != 32768 || pl.Parallel != 2 {
		t.Errorf("context %d parallel %d", pl.Context, pl.Parallel)
	}
}

func TestDiscreteSplitsABigModel(t *testing.T) {
	p := Discrete(cards(16, 16, 16, 16), []Model{model("big", 20, 32768)})
	pl, ok := find(p, "big")
	if !ok || pl.PerBackend != 2 || !slices.Equal(pl.GPUs, []int{0, 1, 2, 3}) {
		t.Fatalf("placement %+v, refused %+v", pl, p.Refused)
	}
	if pl.Context > 32768 {
		t.Errorf("context %d above the trained 32768", pl.Context)
	}
}

func TestDiscreteSeveralModels(t *testing.T) {
	p := Discrete(cards(16, 16, 16), []Model{model("a", 5, 65536), model("b", 20, 65536), model("c", 3, 65536)})
	used := map[int]string{}
	for _, pl := range p.Placed {
		for _, g := range pl.GPUs {
			if prev, dup := used[g]; dup {
				t.Fatalf("card %d given to %s and %s", g, prev, pl.Name)
			}
			used[g] = pl.Name
		}
	}
	// b (largest) needs two cards; a and c compete for the one left: the
	// larger of them is placed and the other refused with a reason.
	if b, _ := find(p, "b"); b.PerBackend != 2 {
		t.Errorf("b = %+v", b)
	}
	if _, ok := find(p, "a"); !ok {
		t.Errorf("a not placed: %+v", p)
	}
	if len(p.Refused) != 1 || p.Refused[0].Name != "c" || p.Refused[0].Reason == "" {
		t.Errorf("refused = %+v", p.Refused)
	}
}

// llama.cpp splits a backend's weights evenly across its cards (tensor-split
// 1,1 unless split_weights says otherwise), so a group is only as good as its
// smallest card times its size — not the sum of what the cards hold.
func TestDiscreteMixedPairUsesTheSmallestCard(t *testing.T) {
	p := Discrete([]GPU{{Index: 0, VRAMMB: 24 * 1024}, {Index: 1, VRAMMB: 8 * 1024}}, []Model{model("big", 26, 32768)})
	if pl, ok := find(p, "big"); ok {
		t.Fatalf("split 26 GiB evenly over a 24 and an 8 GiB card: %+v", pl)
	}
	// Two 16 GiB cards hold what the 24+8 pair cannot.
	p = Discrete(cards(16, 16), []Model{model("big", 26, 32768)})
	if _, ok := find(p, "big"); !ok {
		t.Fatalf("a matched pair was refused: %+v", p)
	}
}

func TestDiscreteHeterogeneousCards(t *testing.T) {
	// 18 GiB of weights fits one 24 GiB card, never the 16 GiB one.
	p := Discrete([]GPU{{Index: 0, VRAMMB: 16 * 1024}, {Index: 1, VRAMMB: 24 * 1024}}, []Model{model("m", 18, 32768)})
	pl, ok := find(p, "m")
	if !ok || !slices.Equal(pl.GPUs, []int{1}) || pl.PerBackend != 1 {
		t.Fatalf("placement %+v refused %+v", pl, p.Refused)
	}
}

func TestDiscreteTooBig(t *testing.T) {
	p := Discrete(cards(16, 16), []Model{model("huge", 70, 32768)})
	if len(p.Placed) != 0 || len(p.Refused) != 1 || p.Refused[0].Reason == "" {
		t.Fatalf("plan %+v", p)
	}
}

func TestSmallTrainedContextAndUncertainEstimate(t *testing.T) {
	p := Discrete(cards(24), []Model{model("short", 4, 4096)})
	if pl, _ := find(p, "short"); pl.Context != 4096 {
		t.Errorf("context %d; a model trained on 4096 gets 4096", pl.Context)
	}
	unknown := Model{Name: "odd", Info: gguf.Info{Arch: "odd", Size: 4 * gib}}
	p = Discrete(cards(24), []Model{unknown})
	pl, ok := find(p, "odd")
	if !ok || !pl.Uncertain || pl.Context != ContextSteps[0] {
		t.Fatalf("uncertain = %+v", pl)
	}
}

func TestUnifiedSharesOneGPU(t *testing.T) {
	// 28 GiB budget: headroom 4.2 GiB (15% > 4 GiB), leaving ~23.8 GiB.
	p := Unified(28*1024, []Model{model("a", 8, 65536), model("b", 5, 65536)})
	var total int64
	for _, pl := range p.Placed {
		if !slices.Equal(pl.GPUs, []int{0}) || pl.PerBackend != 1 {
			t.Errorf("%s: %+v", pl.Name, pl)
		}
		total += pl.NeedMB
	}
	if len(p.Placed) != 2 || total > 28*1024-28*1024*15/100 {
		t.Fatalf("plan %+v, total %d MiB", p, total)
	}
}

// Found on real models (a 36 GiB Mac): the largest model cannot fit at any
// context, but the next one does — it must be placed, not refused along with
// everything else because the loop dropped the smaller ones first.
func TestUnifiedPlacesWhatFits(t *testing.T) {
	p := Unified(27648, []Model{model("huge", 28, 65536), model("mid", 19, 65536), model("small", 3, 65536)})
	if _, ok := find(p, "huge"); ok {
		t.Error("placed a model that cannot fit")
	}
	if _, ok := find(p, "mid"); !ok {
		t.Fatalf("mid fits alone and was refused: %+v", p)
	}
	var total int64
	for _, pl := range p.Placed {
		total += pl.NeedMB
	}
	if avail := int64(27648 - 27648*15/100); total > avail {
		t.Errorf("placed %d MiB into %d MiB", total, avail)
	}
	for _, r := range p.Refused {
		if r.Reason == "" {
			t.Errorf("%s refused without a reason", r.Name)
		}
	}
}

func TestUnifiedStepsDownThenRefuses(t *testing.T) {
	p := Unified(16*1024, []Model{model("a", 6, 65536), model("b", 5, 65536)})
	for _, pl := range p.Placed {
		if pl.Context >= 65536 {
			t.Errorf("%s kept 64K context in a 16 GiB budget", pl.Name)
		}
	}
	p = Unified(12*1024, []Model{model("a", 6, 65536), model("b", 5, 65536)})
	if len(p.Refused) == 0 || p.Refused[0].Reason == "" {
		t.Fatalf("an over-full Mac plan refused nothing: %+v", p)
	}
}
