package route

import (
	"log"
	"math/rand/v2"
	"time"

	"github.com/janit/viiwork/v2/internal/logging"
	"github.com/janit/viiwork/v2/internal/supervisor"
	"github.com/janit/viiwork/v2/mesh/capacity"
)

// The scored choice (performance-routing spec §3). Among hosts with a free
// slot, this node included, pick at random with weight 1/pred^3, where
// pred = overhead + rate*EstK + RTT from each host's own published score. A
// host with no score is learning: it is priced at the fleet median and gets a
// 5% trickle. With no score anywhere the caller routes exactly as before.
const (
	// trickleShare is the probability of sending a request to a learning
	// host, so a newcomer earns a score.
	trickleShare = 0.05
	// maxScored bounds the candidate list so it lives on the stack.
	maxScored = 32
)

type scoredCand struct {
	local    bool
	rep      capacity.Report
	ov, rate float64
	scored   bool
	rttMs    float64
}

// pickScoredLocked returns handled=false when no host serving the model has
// a score, so the caller falls through to local-first / most-free.
func (r *Router) pickScoredLocked(req Request, backends []LocalBackend, localEligible bool, reports []capacity.Report) (*Lease, bool) {
	now := r.c.Now()
	var buf [maxScored]scoredCand
	cands := buf[:0]
	var ovs, rates [maxScored]float64
	pool := 0

	if r.c.LocalScore != nil {
		if ov, rate, ok := r.c.LocalScore(req.Model); ok {
			ovs[pool], rates[pool] = float64(ov), float64(rate)
			pool++
		}
	}
	if localEligible && localHasFree(req, backends) {
		c := scoredCand{local: true}
		if pool > 0 {
			c.ov, c.rate, c.scored = ovs[0], rates[0], true
		}
		cands = append(cands, c)
	}
	for _, rep := range reports {
		mc, free, serves := r.peerFreeLocked(req, rep, now)
		if !serves {
			continue
		}
		scored := mc.PrefillMsPer1k > 0
		if scored && pool < maxScored {
			ovs[pool], rates[pool] = float64(mc.TTFTOverheadMs), float64(mc.PrefillMsPer1k)
			pool++
		}
		if free <= 0 || len(cands) == maxScored {
			continue
		}
		cands = append(cands, scoredCand{rep: rep, ov: float64(mc.TTFTOverheadMs), rate: float64(mc.PrefillMsPer1k),
			scored: scored, rttMs: float64(rep.RTT) / float64(time.Millisecond)})
	}
	if pool == 0 || len(cands) == 0 {
		return nil, false
	}

	medOv, medRate := median(ovs[:pool]), median(rates[:pool])
	var learning [maxScored]int
	nl := 0
	for i := range cands {
		if !cands[i].scored {
			learning[nl] = i
			nl++
		}
	}
	pick, why := -1, "weighted"
	if nl > 0 && r.rand() < trickleShare {
		pick, why = learning[min(int(r.rand()*float64(nl)), nl-1)], "trickle"
	} else {
		var w [maxScored]float64
		sum := 0.0
		for i, c := range cands {
			ov, rate := c.ov, c.rate
			if !c.scored {
				ov, rate = medOv, medRate
			}
			pred := max(ov+rate*req.EstK+c.rttMs, 1)
			w[i] = 1 / (pred * pred * pred)
			sum += w[i]
		}
		x := r.rand() * sum
		pick = len(cands) - 1
		for i := range cands {
			if x < w[i] {
				pick = i
				break
			}
			x -= w[i]
		}
	}
	c := cands[pick]
	if logging.DebugEnabled() {
		logScoredPick(req, cands, pick, why, medOv, medRate)
	}
	if c.local {
		return r.pickLocalLocked(req, backends), true
	}
	return r.peerLeaseLocked(c.rep, req.Model, now), true
}

func (r *Router) rand() float64 {
	if r.c.Rand != nil {
		return r.c.Rand()
	}
	return rand.Float64()
}

func localHasFree(req Request, backends []LocalBackend) bool {
	for _, b := range backends {
		if b.State() == supervisor.StateHealthy && !req.Exclude["local:"+b.ID()] && b.Slots()-b.InFlight() > 0 {
			return true
		}
	}
	return false
}

// median sorts xs in place (at most maxScored values).
func median(xs []float64) float64 {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
	return xs[len(xs)/2]
}

func logScoredPick(req Request, cands []scoredCand, pick int, why string, medOv, medRate float64) {
	for i, c := range cands {
		name := c.rep.Node
		if c.local {
			name = "local"
		}
		ov, rate := c.ov, c.rate
		if !c.scored {
			ov, rate = medOv, medRate
		}
		mark := " "
		if i == pick {
			mark = "*"
		}
		log.Printf("[debug] route %s est %.1fk: %s %s pred %.0f ms (scored %v) [%s]", req.Model, req.EstK, mark, name, ov+rate*req.EstK+c.rttMs, c.scored, why)
	}
}
