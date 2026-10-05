// Package plan turns a host's GPUs and the models chosen to serve into a
// layout: which cards each model gets, how many per backend, the context per
// slot and the slots. Pure: no I/O, so every fit case is a table test. The
// estimate is deliberately conservative — f16 KV cache, a fixed per-GPU
// overhead — and a model whose header does not say enough is planned at the
// smallest context and marked uncertain.
package plan

import (
	"fmt"
	"sort"

	"github.com/janit/viiwork/v2/internal/gguf"
)

// ContextSteps are the per-slot contexts offered, smallest first.
var ContextSteps = []int{8192, 16384, 32768, 49152, 65536}

// OverheadMB is reserved on every card of a backend for compute buffers.
const OverheadMB = 1024

// defaultParallel is the slots a backend gets.
const defaultParallel = 2

type GPU struct {
	Index  int
	VRAMMB int64
}

type Model struct {
	Name string
	Info gguf.Info
}

type Placement struct {
	Name       string
	GPUs       []int // every card the model owns, backend groups in order
	PerBackend int   // gpus_per_backend
	Context    int   // per slot
	Parallel   int
	NeedMB     int64 // one backend's estimate at Context
	Uncertain  bool
}

type Refused struct{ Name, Reason string }

type Plan struct {
	Placed  []Placement
	Refused []Refused
}

const mib = 1 << 20

func weightsMB(m Model) int64 { return (m.Info.Size + mib - 1) / mib }

// steps is the contexts m may be given, largest first, capped at its trained
// context. A model trained on less than the smallest step gets exactly that.
func steps(m Model) []int {
	trained := m.Info.ContextLength
	var out []int
	for i := len(ContextSteps) - 1; i >= 0; i-- {
		if trained <= 0 || ContextSteps[i] <= trained {
			out = append(out, ContextSteps[i])
		}
	}
	if len(out) == 0 {
		out = []int{trained}
	}
	return out
}

// needMB is one backend of m across cards cards at ctx per slot.
func needMB(m Model, ctx, cards int) (int64, bool) {
	kv, ok := m.Info.KVBytesPerToken()
	if !ok {
		kv = 0
	}
	return weightsMB(m) + (kv*int64(ctx)*defaultParallel+mib-1)/mib + int64(cards)*OverheadMB, ok
}

// capacityMB is what a group of cards holds for one backend. llama.cpp splits
// a backend's weights evenly across its cards (tensor-split 1,1,…), so the
// smallest card bounds every share: the group holds its size times that card.
func capacityMB(cards []GPU) int64 {
	if len(cards) == 0 {
		return 0
	}
	least := cards[0].VRAMMB
	for _, c := range cards[1:] {
		least = min(least, c.VRAMMB)
	}
	return int64(len(cards)) * least
}

// Discrete plans a host of separate cards. Models are placed largest first,
// each on the fewest cards that hold one backend at the smallest context
// (capacityMB: an even split, so the group's smallest card counts), preferring the largest free cards; a card belongs to one
// model. Then each model's context is raised as far as its cards allow, and
// the cards left over become extra replicas, handed out a whole group at a
// time in the same order.
func Discrete(gpus []GPU, models []Model) Plan {
	free := append([]GPU(nil), gpus...)
	sort.SliceStable(free, func(i, j int) bool { return free[i].VRAMMB > free[j].VRAMMB })
	order := append([]Model(nil), models...)
	sort.SliceStable(order, func(i, j int) bool { return weightsMB(order[i]) > weightsMB(order[j]) })

	var p Plan
	type group struct {
		m     Model
		cards []GPU
	}
	var placed []group
	for _, m := range order {
		smallest := steps(m)[len(steps(m))-1]
		var chosen []GPU
		for k := 1; k <= len(free) && chosen == nil; k++ {
			need, _ := needMB(m, smallest, k)
			if capacityMB(free[:k]) >= need {
				chosen = append([]GPU(nil), free[:k]...)
			}
		}
		if chosen == nil {
			need, _ := needMB(m, smallest, 1)
			var total int64
			for _, g := range free {
				total += g.VRAMMB
			}
			p.Refused = append(p.Refused, Refused{m.Name, fmt.Sprintf("needs about %d MiB at %d tokens per slot; %d MiB of cards are left", need, smallest, total)})
			continue
		}
		free = free[len(chosen):]
		placed = append(placed, group{m, chosen})
	}
	// Extra replicas: whole groups of the same size, while cards remain.
	extra := map[string][][]GPU{}
	for progress := true; progress; {
		progress = false
		for _, g := range placed {
			k := len(g.cards)
			if len(free) < k {
				continue
			}
			// A replica must fit like the first group did.
			need, _ := needMB(g.m, steps(g.m)[len(steps(g.m))-1], k)
			if capacityMB(free[:k]) < need {
				continue
			}
			extra[g.m.Name] = append(extra[g.m.Name], free[:k])
			free = free[k:]
			progress = true
		}
	}
	for _, g := range placed {
		groups := append([][]GPU{g.cards}, extra[g.m.Name]...)
		// The context every group can hold, set by the tightest group.
		ctx, uncertain := 0, false
		for _, c := range steps(g.m) {
			fits := true
			for _, grp := range groups {
				need, ok := needMB(g.m, c, len(grp))
				uncertain = !ok
				if need > capacityMB(grp) {
					fits = false
				}
			}
			if fits {
				ctx = c
				break
			}
		}
		if uncertain {
			ctx = steps(g.m)[len(steps(g.m))-1]
		}
		var idx []int
		for _, grp := range groups {
			for _, c := range grp {
				idx = append(idx, c.Index)
			}
		}
		need, _ := needMB(g.m, ctx, len(g.cards))
		p.Placed = append(p.Placed, Placement{Name: g.m.Name, GPUs: idx, PerBackend: len(g.cards),
			Context: ctx, Parallel: defaultParallel, NeedMB: need, Uncertain: uncertain})
	}
	return p
}

// Unified plans an Apple Silicon host: one GPU whose memory is shared by every
// model and by everything else on the machine. The models' total must stay
// under the budget minus headroom (the larger of 4 GiB and 15%). Models are
// admitted largest first while they fit at their smallest context — one that
// cannot is refused with the reason, and the next is still tried — then each
// admitted model's context is raised, largest model first, as far as the
// budget allows.
func Unified(budgetMB int64, models []Model) Plan {
	headroom := max(int64(4*1024), budgetMB*15/100)
	avail := budgetMB - headroom
	order := append([]Model(nil), models...)
	sort.SliceStable(order, func(i, j int) bool { return weightsMB(order[i]) > weightsMB(order[j]) })

	var p Plan
	var admitted []Model
	var used int64
	for _, m := range order {
		smallest := steps(m)[len(steps(m))-1]
		need, _ := needMB(m, smallest, 1)
		if used+need > avail {
			p.Refused = append(p.Refused, Refused{m.Name, fmt.Sprintf(
				"needs about %d MiB even at %d tokens per slot; %d MiB of the %d MiB budget are left after headroom",
				need, smallest, avail-used, budgetMB)})
			continue
		}
		admitted = append(admitted, m)
		used += need
	}
	ctx := make([]int, len(admitted))
	for i, m := range admitted {
		ctx[i] = steps(m)[len(steps(m))-1]
	}
	for i, m := range admitted {
		base, _ := needMB(m, ctx[i], 1)
		for _, c := range steps(m) { // largest first
			need, ok := needMB(m, c, 1)
			if !ok {
				break // no KV estimate: stay at the smallest context
			}
			if used-base+need <= avail {
				used += need - base
				ctx[i] = c
				break
			}
		}
	}
	for i, m := range admitted {
		need, ok := needMB(m, ctx[i], 1)
		p.Placed = append(p.Placed, Placement{Name: m.Name, GPUs: []int{0}, PerBackend: 1,
			Context: ctx[i], Parallel: defaultParallel, NeedMB: need, Uncertain: !ok})
	}
	return p
}
