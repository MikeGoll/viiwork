package activity

import (
	"fmt"
	"testing"
)

// Recent is oldest first and holds the last maxEvents events, whether the
// ring is filling, exactly full, or has wrapped any number of times.
func TestEventsRingOrder(t *testing.T) {
	const size = 5
	for _, total := range []int{0, 1, size - 1, size, size + 1, 2*size - 1, 2 * size, 3*size + 2} {
		t.Run(fmt.Sprint(total), func(t *testing.T) {
			l := NewLogWithHistory(10, size)
			for i := 0; i < total; i++ {
				l.EmitRequest(int64(i+1), 0, "e%d", i)
			}
			got := l.Recent()
			first := max(0, total-size)
			if len(got) != total-first {
				t.Fatalf("len = %d, want %d", len(got), total-first)
			}
			for j, ev := range got {
				if want := fmt.Sprintf("e%d", first+j); ev.Message != want {
					t.Errorf("Recent()[%d] = %q, want %q", j, ev.Message, want)
				}
			}
			// Recent is a copy: changing it leaves the ring alone.
			if len(got) > 0 {
				got[0].Message = "changed"
				if l.Recent()[0].Message == "changed" {
					t.Error("Recent returned the ring itself")
				}
			}
		})
	}
}

// BenchmarkEmitFullRing is an emit into a ring already at capacity. The ring
// must not be reallocated or copied per event: allocations stay constant
// (the JSON encoding and the formatted message), whatever the ring size.
func BenchmarkEmitFullRing(b *testing.B) {
	for _, size := range []int{200, 2000, 20000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			l := NewLogWithHistory(10, size)
			for i := 0; i < size; i++ {
				l.EmitRequest(int64(i+1), 0, "fill %d", i)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				l.EmitRequest(int64(i+1), 0, "bench %d", i)
			}
		})
	}
}
