package route

import (
	"fmt"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

// BenchmarkPick is the per-request routing cost: 3 local models of 4 backends
// and 10 peer reports of 3 models each, Pick then Release.
func BenchmarkPick(b *testing.B) {
	local := newFakeLocal()
	for m := 0; m < 3; m++ {
		name := fmt.Sprintf("m%d", m)
		for i := 0; i < 4; i++ {
			local.add(name, newFakeBackend(fmt.Sprintf("%s/%d", name, i), 2))
		}
	}
	reports := &fakeReports{}
	now := time.Now()
	for n := 0; n < 10; n++ {
		var models []meshapi.ModelCapacity
		for m := 0; m < 3; m++ {
			models = append(models, meshapi.ModelCapacity{Name: fmt.Sprintf("m%d", m), Slots: 4, Busy: 1, HealthyBackends: 2})
		}
		reports.reports = append(reports.reports, report(fmt.Sprintf("peer%d", n), now, time.Millisecond, models...))
	}
	r := New(Config{Self: "self", Local: local, Remote: reports, StaleAfter: time.Hour, QueueMax: 64, QueueTimeout: time.Second})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l, err := r.Pick(Request{Model: "m1"})
		if err != nil {
			b.Fatal(err)
		}
		l.Release()
	}
}

// BenchmarkDispatchManyWaiters is one Wake over a queue that cannot be served:
// 256 unpinned and 8 pinned waiters for a model whose every slot, local and on
// 10 peers, is busy. Wake runs on every lease release and capacity report, so
// its cost is paid under the router mutex many times a second.
func BenchmarkDispatchManyWaiters(b *testing.B) {
	local := newFakeLocal()
	busy := newFakeBackend("m/0", 1)
	busy.Acquire()
	local.add("m", busy)
	reports := &fakeReports{}
	now := time.Now()
	for n := 0; n < 10; n++ {
		reports.reports = append(reports.reports, report(fmt.Sprintf("peer%d", n), now, time.Millisecond,
			meshapi.ModelCapacity{Name: "m", Slots: 4, Busy: 4, HealthyBackends: 2}))
	}
	r := New(Config{Self: "self", Local: local, Remote: reports, StaleAfter: time.Hour, QueueMax: 1024, QueueTimeout: time.Second})
	for i := 0; i < 264; i++ {
		req := Request{Model: "m"}
		if i%33 == 32 {
			req.Host = fmt.Sprintf("peer%d", i%10)
		}
		r.queues["m"] = append(r.queues["m"], &waiter{req: req, enqueued: now, result: make(chan waitResult, 1)})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Wake()
	}
	if got := r.QueueLen("m"); got != 264 {
		b.Fatalf("queue = %d, want every waiter still queued", got)
	}
}
