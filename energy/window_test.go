package energy

import (
	"math/rand"
	"reflect"
	"sort"
	"testing"
	"time"
)

// refNode and refGPU are the range reads as they were before they learnt to
// read only the slots in range: scan every slot, filter by timestamp.
func refNode(t *testing.T, r *ring, from, to time.Time) []NodeRecord {
	buf, err := r.all()
	if err != nil {
		t.Fatal(err)
	}
	var out []NodeRecord
	for off := 0; off+NodeRecordSize <= len(buf); off += NodeRecordSize {
		rec := decodeNode(buf[off : off+NodeRecordSize])
		if rec.TS >= from.Unix() && rec.TS < to.Unix() {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS < out[j].TS })
	return out
}

func refGPU(t *testing.T, r *ring, from, to time.Time) []GPURecord {
	buf, err := r.all()
	if err != nil {
		t.Fatal(err)
	}
	var out []GPURecord
	for off := 0; off+GPURecordSize <= len(buf); off += GPURecordSize {
		rec := decodeGPU(buf[off : off+GPURecordSize])
		if rec.TS >= from.Unix() && rec.TS < to.Unix() {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TS != out[j].TS {
			return out[i].TS < out[j].TS
		}
		return out[i].GPUID < out[j].GPUID
	})
	return out
}

// Reading only the slots a window can touch must return exactly what a full
// scan returns, including across the wrap and with stale records from earlier
// laps still in the slots.
func TestWindowedReadsMatchFullScan(t *testing.T) {
	gpus := []int{0, 3, 7}
	s, err := Open(Config{Dir: t.TempDir(), GPUIDs: gpus, MinuteSlots: 50, HourSlots: 30, DaySlots: 10}, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	rng := rand.New(rand.NewSource(1))
	base := int64(1_800_000_000)
	rings := []struct {
		node, gpu *ring
		period    int64
	}{
		{s.nodeMinute, s.gpuMinute, 60},
		{s.nodeHour, s.gpuHour, 3600},
		{s.nodeDay, s.gpuDay, 86400},
	}
	for _, rg := range rings {
		lapBuckets := int64(rg.node.slots) * 4
		for range 400 {
			bucket := base/rg.period + rng.Int63n(lapBuckets)
			ts := bucket * rg.period
			node := NodeRecord{TS: ts, Watts: rng.Float32() * 1000, CoveredS: uint16(rng.Intn(60))}
			if err := rg.node.put(bucket, 0, node.encode); err != nil {
				t.Fatal(err)
			}
			for lane, id := range gpus {
				if rng.Intn(3) == 0 {
					continue
				}
				g := GPURecord{TS: ts, GPUID: uint16(id), AttrW: rng.Float32() * 300, RawW: rng.Float32() * 300, CoveredS: 60}
				if err := rg.gpu.put(bucket, lane, g.encode); err != nil {
					t.Fatal(err)
				}
			}
		}
		for i := range 300 {
			span := lapBuckets * rg.period
			lo := base + rng.Int63n(span) - rg.period
			hi := lo + rng.Int63n(int64(rg.node.slots)*rg.period*3/2)
			if i%7 == 0 {
				lo = lo / rg.period * rg.period // bucket-aligned edges too
				hi = hi / rg.period * rg.period
			}
			from, to := time.Unix(lo, 0), time.Unix(hi, 0)
			if got, want := s.readNodeRange(rg.node, from, to), refNode(t, rg.node, from, to); !reflect.DeepEqual(got, want) {
				t.Fatalf("period %d node [%d,%d): got %d records, want %d", rg.period, lo, hi, len(got), len(want))
			}
			if got, want := s.readGPURange(rg.gpu, from, to), refGPU(t, rg.gpu, from, to); !reflect.DeepEqual(got, want) {
				t.Fatalf("period %d gpu [%d,%d): got %d records, want %d", rg.period, lo, hi, len(got), len(want))
			}
		}
	}
}
