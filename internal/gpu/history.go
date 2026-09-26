package gpu

import (
	"sort"
	"sync"
	"time"
)

type RingBuffer struct {
	samples []GPUSample
	head    int
	count   int
	maxSize int
}

func newRingBuffer(maxSize int) *RingBuffer {
	return &RingBuffer{samples: make([]GPUSample, maxSize), maxSize: maxSize}
}

func (rb *RingBuffer) add(s GPUSample) {
	rb.samples[rb.head] = s
	rb.head = (rb.head + 1) % rb.maxSize
	if rb.count < rb.maxSize {
		rb.count++
	}
}

func (rb *RingBuffer) slice() []GPUSample {
	if rb.count == 0 {
		return nil
	}
	out := make([]GPUSample, rb.count)
	if rb.count < rb.maxSize {
		copy(out, rb.samples[:rb.count])
	} else {
		start := rb.head
		n := copy(out, rb.samples[start:])
		copy(out[n:], rb.samples[:start])
	}
	return out
}

type History struct {
	mu      sync.RWMutex
	buffers map[int]*RingBuffer
	maxSize int
	now     func() time.Time // injectable for tests; nil is time.Now
}

func NewHistory(maxSize int) *History {
	return &History{buffers: make(map[int]*RingBuffer), maxSize: maxSize}
}

// GPUIDs returns every GPU the collector has seen, sorted. This is every card
// rocm-smi reports, not just the ones this instance was configured with, which
// is what energy attribution needs: the marginal-power denominator has to cover
// the whole host or a co-tenant instance's load is charged to this one's GPUs.
func (h *History) GPUIDs() []int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	ids := make([]int, 0, len(h.buffers))
	for id := range h.buffers {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

func (h *History) Record(s GPUSample) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rb, ok := h.buffers[s.GPUID]
	if !ok {
		rb = newRingBuffer(h.maxSize)
		h.buffers[s.GPUID] = rb
	}
	rb.add(s)
}

func (h *History) Samples(gpuID int) []GPUSample {
	h.mu.RLock()
	defer h.mu.RUnlock()
	rb, ok := h.buffers[gpuID]
	if !ok {
		return nil
	}
	return rb.slice()
}

func (h *History) AllGPUSamples() map[int][]GPUSample {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(map[int][]GPUSample, len(h.buffers))
	for id, rb := range h.buffers {
		out[id] = rb.slice()
	}
	return out
}

// Staleness of the newest sample. A collector whose tool stops answering
// records nothing, so without an age check the last sample would stand as the
// card's current state — and its wattage be recorded as measured — for as long
// as the outage lasted. A sample is current for staleIntervals recording
// periods, where the period is the smallest gap among a card's newest few
// samples (so one long outage does not stretch it), floored at minPeriod and
// defaultPeriod until two samples exist.
const (
	staleIntervals = 3
	periodWindow   = 4 // newest samples consulted for the period
	minPeriod      = 5 * time.Second
	defaultPeriod  = 10 * time.Second
)

func (h *History) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

// at returns the i-th newest sample (0 is the newest). Callers ensure i < count.
func (rb *RingBuffer) at(i int) GPUSample {
	return rb.samples[((rb.head-1-i)%rb.maxSize+rb.maxSize)%rb.maxSize]
}

// period is the recording period implied by the newest samples.
func (rb *RingBuffer) period() time.Duration {
	best := int64(0)
	for i := 0; i+1 < rb.count && i+1 < periodWindow; i++ {
		gap := rb.at(i).Timestamp - rb.at(i+1).Timestamp
		if gap > 0 && (best == 0 || gap < best) {
			best = gap
		}
	}
	p := defaultPeriod
	if best > 0 {
		p = time.Duration(best) * time.Second
	}
	if p < minPeriod {
		p = minPeriod
	}
	return p
}

// Latest returns the newest sample for each GPU, ordered by GPU ID, leaving out
// any card whose newest sample is stale: absent, rather than an old value
// presented as current.
//
// A status poll needs only the current value; AllGPUSamples copies the whole
// hour-long ring buffer for every GPU, which is far too much to put on a
// per-poll mesh payload.
func (h *History) Latest() []GPUSample {
	now := h.clock().Unix()
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]GPUSample, 0, len(h.buffers))
	for _, rb := range h.buffers {
		if rb.count == 0 {
			continue
		}
		newest := rb.at(0)
		if time.Duration(now-newest.Timestamp)*time.Second > staleIntervals*rb.period() {
			continue
		}
		out = append(out, newest)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GPUID < out[j].GPUID })
	return out
}
