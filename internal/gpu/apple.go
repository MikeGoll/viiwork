package gpu

import (
	"bytes"
	"context"
	"log"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

// appleIoregArgs list the Apple Silicon GPU's registry entry. -c matches the
// subclass (AGXAcceleratorG15X on an M3), and none of it needs root.
var appleIoregArgs = []string{"-r", "-d", "1", "-w", "0", "-c", "AGXAccelerator"}

// AppleCollector samples the one GPU of an Apple Silicon Mac from ioreg's
// PerformanceStatistics: utilisation, and the unified memory the GPU holds
// against the machine's RAM. It records card 0 into the same History and
// live event as the other collectors.
//
// There is no power reading without root (powermetrics), so PowerAvailable is
// false and power, energy and cost read as unavailable, exactly as on a host
// without IPMI or GPU power.
type AppleCollector struct {
	history     *History
	broadcaster *Broadcaster
	run         Runner
	totalRAM    func() int64
	available   atomic.Bool
	logger      *log.Logger
}

var _ Collector = (*AppleCollector)(nil)

func NewAppleCollector(history *History, broadcaster *Broadcaster, run Runner, totalRAMBytes func() int64) *AppleCollector {
	c := &AppleCollector{
		history:     history,
		broadcaster: broadcaster,
		run:         run,
		totalRAM:    totalRAMBytes,
		logger:      log.New(os.Stdout, "[gpu] ", log.LstdFlags),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	out, err := run(ctx, "ioreg", appleIoregArgs...)
	if err != nil {
		c.logger.Printf("ioreg unavailable (GPU metrics disabled): %v", err)
		return c
	}
	if _, ok := parseAppleStats(out, c.totalRAM(), time.Now().Unix()); !ok {
		c.logger.Println("ioreg reports no GPU PerformanceStatistics (GPU metrics disabled)")
		return c
	}
	c.available.Store(true)
	c.logger.Println("ioreg available, GPU metrics enabled (per-GPU power: false)")
	c.Sample(context.Background())
	return c
}

func (c *AppleCollector) Sample(ctx context.Context) {
	if !c.available.Load() {
		return
	}
	out, err := c.run(ctx, "ioreg", appleIoregArgs...)
	if err != nil {
		c.logger.Printf("ioreg failed: %v", err)
		return
	}
	now := time.Now().Unix()
	s, ok := parseAppleStats(out, c.totalRAM(), now)
	if !ok {
		return
	}
	c.history.Record(s)
	c.broadcaster.Broadcast(streamEvent(now, []GPUSample{s}))
}

func (c *AppleCollector) Available() bool { return c.available.Load() }

func (c *AppleCollector) PowerAvailable() bool { return false }

// parseAppleStats reads card 0 from `ioreg -c AGXAccelerator`: "Device
// Utilization %" and "In use system memory" (bytes) from the first
// PerformanceStatistics dictionary, against totalRAM bytes. The keys are
// undocumented and can change between macOS releases, so a missing or
// unreadable one, or an unknown total, yields no sample rather than a zero.
func parseAppleStats(out []byte, totalRAM int64, now int64) (GPUSample, bool) {
	if totalRAM <= 0 {
		return GPUSample{}, false
	}
	stats := ioregDict(out, `"PerformanceStatistics"`)
	util, okUtil := stats["Device Utilization %"]
	inUse, okMem := stats["In use system memory"]
	if !okUtil || !okMem {
		return GPUSample{}, false
	}
	u, err1 := strconv.ParseFloat(util, 64)
	m, err2 := strconv.ParseInt(inUse, 10, 64)
	if err1 != nil || err2 != nil {
		return GPUSample{}, false
	}
	return GPUSample{
		GPUID:       0,
		Utilization: u,
		VRAMUsedMB:  float64(m / 1024 / 1024),
		VRAMTotalMB: float64(totalRAM / 1024 / 1024),
		Timestamp:   now,
	}, true
}

// ioregDict returns the top-level pairs of the first `<key> = {...}` in out,
// keyed by the unquoted name, with each value's raw text. Keys are compared
// whole, so "In use system memory (driver)" never answers for "In use system
// memory"; a nested dictionary or array value is kept as text and skipped
// over, so an unknown key cannot derail the rest.
func ioregDict(out []byte, key string) map[string]string {
	i := bytes.Index(out, []byte(key))
	if i < 0 {
		return nil
	}
	rest := out[i+len(key):]
	open := bytes.IndexByte(rest, '{')
	if open < 0 || len(bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(rest[:open]), []byte("=")))) != 0 {
		return nil
	}
	body := rest[open+1:]
	pairs := map[string]string{}
	depth, inQuote, start := 0, false, 0
	flush := func(end int) {
		k, v, ok := bytes.Cut(bytes.TrimSpace(body[start:end]), []byte("="))
		if !ok {
			return
		}
		name, err := strconv.Unquote(string(bytes.TrimSpace(k)))
		if err != nil {
			return
		}
		pairs[name] = string(bytes.TrimSpace(v))
	}
	for j, ch := range body {
		switch {
		case ch == '"':
			inQuote = !inQuote
		case inQuote:
		case ch == '{' || ch == '(':
			depth++
		case (ch == '}' || ch == ')') && depth > 0:
			depth--
		case ch == '}':
			flush(j)
			return pairs
		case ch == ',' && depth == 0:
			flush(j)
			start = j + 1
		}
	}
	return nil // unterminated
}
