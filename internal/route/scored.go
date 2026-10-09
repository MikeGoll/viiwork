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
//
// Stickiness (v2.7.0-beta3): only hosts within stickyBand of the best
// prediction are eligible. A request with a session key goes to the band
// member with the highest rendezvous score, this node included (in beta3 and
// beta4 the session came after this node); else this node, if in the band;
// else the 1/pred^3 draw runs over the band alone.
const (
	// trickleShare is the probability of sending a request to a learning
	// host, so a newcomer earns a score.
	trickleShare = 0.05
	// maxScored bounds the candidate list so it lives on the stack.
	maxScored = 32
	// stickyBand is how much slower than the best prediction a host may be
	// and still count as equal (v2.7.0-beta3). Within the band a request
	// follows its session to one host, or stays local, so turns land on a
	// warm KV cache instead of being scattered across near-equal hosts.
	stickyBand = 1.25
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
		// A negative overhead is no measurement: a peer publishing one would
		// be predicted faster than any real host.
		scored := mc.PrefillMsPer1k > 0 && mc.TTFTOverheadMs >= 0
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
		var pred [maxScored]float64
		best := 0.0
		for i, c := range cands {
			ov, rate := c.ov, c.rate
			if !c.scored {
				ov, rate = medOv, medRate
			}
			pred[i] = max(ov+rate*req.EstK+c.rttMs, 1)
			if i == 0 || pred[i] < best {
				best = pred[i]
			}
		}
		limit := best * stickyBand
		// The band: hosts near enough the best that a warm KV cache is worth
		// more than the difference. The session's host first, this node
		// included, so its turns find their cache from any entry node; then
		// this node; else a weighted draw over the band only. Local-first
		// came before the session until 2026-10-03, and an entry node with a
		// free slot pulled back every session whose cache was elsewhere.
		if req.SessionKey != 0 {
			var top uint64
			for i, c := range cands {
				if pred[i] > limit {
					continue
				}
				name := c.rep.Node
				if c.local {
					name = r.c.Self
				}
				if h := rendezvous(req.SessionKey, name); pick < 0 || h > top {
					pick, top = i, h
				}
			}
			why = "session"
		} else if cands[0].local && pred[0] <= limit {
			pick, why = 0, "local-band"
		} else {
			var w [maxScored]float64
			sum := 0.0
			for i := range cands {
				if pred[i] <= limit {
					w[i] = 1 / (pred[i] * pred[i] * pred[i])
					sum += w[i]
				}
			}
			x := r.rand() * sum
			for i := range cands {
				if w[i] == 0 {
					continue
				}
				pick = i // the last band member catches rounding at the top
				if x < w[i] {
					break
				}
				x -= w[i]
			}
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

// rendezvous scores node for a session (highest random weight): the same key
// ranks the hosts the same way on every entry node, and a host leaving the
// band moves only its own sessions. FNV-1a over the name, seeded by the key,
// then the splitmix64 finaliser so nearby keys scatter.
func rendezvous(key uint64, node string) uint64 {
	h := uint64(14695981039346656037) ^ key
	for i := 0; i < len(node); i++ {
		h ^= uint64(node[i])
		h *= 1099511628211
	}
	h ^= h >> 30
	h *= 0xbf58476d1ce4e5b9
	h ^= h >> 27
	h *= 0x94d049bb133111eb
	h ^= h >> 31
	return h
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
