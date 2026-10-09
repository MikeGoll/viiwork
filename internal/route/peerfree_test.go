package route

import (
	"testing"
	"time"

	"github.com/janit/viiwork/v2/mesh/capacity"
)

// peerFreeLocked is the one definition of a peer's eligibility and free
// slots, shared by the most-free pick and the scored pick so that a change to
// either rule (freshness, refusal marks, reservations) reaches both.
func TestPeerFreeLocked(t *testing.T) {
	now := t0.Add(100 * time.Millisecond)
	fresh := report("P", t0, time.Millisecond, capM(4, 1, 1))
	// Requested at t0-100ms, received at t0: its AsOf is the request.
	requested := fresh
	requested.Sent = t0.Add(-100 * time.Millisecond)
	cases := []struct {
		name       string
		rep        capacity.Report
		req        Request
		refusedAt  time.Time // zero = no refusal mark
		reserved   int       // forwards started after the report
		wantServes bool
		wantFree   int
		wantMark   bool // the refusal mark survives
	}{
		{"fresh with room", fresh, Request{Model: "m"}, time.Time{}, 0, true, 3, false},
		{"self", report("self", t0, 0, capM(4, 0, 1)), Request{Model: "m"}, time.Time{}, 0, false, 0, false},
		{"stale", report("P", t0.Add(-2*time.Second), 0, capM(4, 0, 1)), Request{Model: "m"}, time.Time{}, 0, false, 0, false},
		{"other model", fresh, Request{Model: "n"}, time.Time{}, 0, false, 0, false},
		{"no healthy backend", report("P", t0, 0, capM(4, 0, 0)), Request{Model: "m"}, time.Time{}, 0, false, 0, false},
		{"excluded still serves, no room", fresh, Request{Model: "m", Exclude: map[string]bool{"peer:P": true}}, time.Time{}, 0, true, 0, false},
		{"other pin still serves, no room", fresh, Request{Model: "m", Host: "Q"}, time.Time{}, 0, true, 0, false},
		{"refused after the report", fresh, Request{Model: "m"}, t0.Add(50 * time.Millisecond), 0, true, 0, true},
		{"report newer than the refusal clears it", fresh, Request{Model: "m"}, t0.Add(-50 * time.Millisecond), 0, true, 3, false},
		{"excluded keeps the mark", fresh, Request{Model: "m", Exclude: map[string]bool{"peer:P": true}}, t0.Add(-50 * time.Millisecond), 0, true, 0, true},
		{"reservations count", fresh, Request{Model: "m"}, time.Time{}, 2, true, 1, false},
		// AsOf (main 94f2825): the mark and reservations are judged against
		// when the report was requested, so both picks share the rule.
		{"received after the refusal but requested before keeps the mark", requested, Request{Model: "m"}, t0.Add(-50 * time.Millisecond), 0, true, 0, true},
		{"requested after the refusal clears it", requested, Request{Model: "m"}, t0.Add(-150 * time.Millisecond), 0, true, 3, false},
	}
	for _, tc := range cases {
		r := New(Config{Self: "self", Local: newFakeLocal(), Remote: &fakeReports{}, StaleAfter: time.Second, QueueMax: 1, QueueTimeout: time.Second,
			Now: func() time.Time { return now }})
		key := resKey{"P", tc.req.Model}
		if !tc.refusedAt.IsZero() {
			r.refused[key] = tc.refusedAt
		}
		for i := 0; i < tc.reserved; i++ {
			r.peerLeaseLocked(tc.rep, tc.req.Model, now)
		}
		mc, free, serves := r.peerFreeLocked(tc.req, tc.rep, now)
		if serves != tc.wantServes || free != tc.wantFree {
			t.Errorf("%s: serves %v free %d, want %v %d", tc.name, serves, free, tc.wantServes, tc.wantFree)
		}
		if serves && mc.Name != tc.req.Model {
			t.Errorf("%s: capacity for %q", tc.name, mc.Name)
		}
		if _, mark := r.refused[key]; mark != tc.wantMark {
			t.Errorf("%s: refusal mark present = %v, want %v", tc.name, mark, tc.wantMark)
		}
	}
}
