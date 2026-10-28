package energy

import (
	"reflect"
	"testing"
	"time"
)

// clientRecorder is a recorder over two cards of "qwen" at 120 W and 180 W:
// 300 W, so a fully covered minute is 5 Wh. attribute nil is the chassis
// model, with node power at 500 W.
func clientRecorder(t *testing.T, attribute AttributeFunc, uses ClientUseFunc) (*Recorder, *Store) {
	t.Helper()
	s := clientStoreIn(t, t.TempDir(), 8)
	readings := []GPUReading{{GPUID: 0, Watts: 120, Model: "qwen"}, {GPUID: 1, Watts: 180, Model: "qwen"}}
	nodeW := 300.0
	if attribute == nil {
		nodeW = 500
	}
	rec := NewRecorderWithAttribution(s, 30*time.Second,
		func() (float64, bool) { return nodeW, true },
		func() []GPUReading { return readings },
		attribute, quiet())
	if uses != nil {
		rec.SetClientUse(uses)
	}
	return rec, s
}

// sampleMinutes samples n whole minutes from minute0 and flushes the last.
func sampleMinutes(rec *Recorder, n int) {
	base := time.Unix(minute0, 0)
	for i := 0; i < n; i++ {
		m := base.Add(time.Duration(i) * time.Minute)
		rec.Sample(m)
		rec.Sample(m.Add(30 * time.Second))
	}
	rec.Flush(base.Add(time.Duration(n) * time.Minute))
}

func TestRecorderSplitsAMinuteBetweenClients(t *testing.T) {
	var asked []int64
	rec, s := clientRecorder(t, Direct, func(bucketStart int64) []ClientUse {
		asked = append(asked, bucketStart)
		return []ClientUse{{Client: "factory-a", Model: "qwen", BusyS: 45, Tokens: 900}, {Client: "batch-b", Model: "qwen", BusyS: 15, Tokens: 100}}
	})
	sampleMinutes(rec, 1)
	if !reflect.DeepEqual(asked, []int64{minute0}) {
		t.Fatalf("the recorder asked for %v, want the minute's start once", asked)
	}
	got := map[string]ClientRecord{}
	for _, r := range minuteRecords(s, minute0) {
		got[s.ClientName(r.ClientIdx)] = r
	}
	if len(got) != 2 || !near(float64(got["factory-a"].Wh), 3.75) || !near(float64(got["batch-b"].Wh), 1.25) {
		t.Errorf("got %+v, want 3.75 Wh and 1.25 Wh", got)
	}
	if s.ModelName(got["factory-a"].ModelIdx) != "qwen" || got["factory-a"].Tokens != 900 {
		t.Errorf("factory-a: %+v", got["factory-a"])
	}
}

func TestRecorderWithoutClientsWritesNoClientFile(t *testing.T) {
	for name, uses := range map[string]ClientUseFunc{
		"no function":        nil,
		"nobody named, ever": func(int64) []ClientUse { return nil },
	} {
		rec, s := clientRecorder(t, Direct, uses)
		sampleMinutes(rec, 2)
		if s.HasClients() {
			t.Errorf("%s: the store gained client files", name)
		}
		if len(s.ReadNode(TierMinute, time.Unix(minute0, 0), time.Unix(minute0+120, 0))) != 2 {
			t.Errorf("%s: the node minutes were not recorded", name)
		}
	}
}

// A minute with no request, on a store that has clients: the model's energy
// is recorded against nobody.
func TestIdleMinuteGoesToNobody(t *testing.T) {
	rec, s := clientRecorder(t, Direct, func(bucketStart int64) []ClientUse {
		if bucketStart == minute0 {
			return []ClientUse{{Client: "factory-a", Model: "qwen", BusyS: 60}}
		}
		return nil
	})
	sampleMinutes(rec, 2)
	idle := minuteRecords(s, minute0+60)
	if len(idle) != 1 || idle[0].ClientIdx != 0 || !near(float64(idle[0].Wh), 5) {
		t.Errorf("the idle minute: %+v, want 5 Wh for nobody", idle)
	}
	if c := theClient(t, s, "factory-a"); !near(c.Wh, 5) {
		t.Errorf("factory-a was charged for the idle minute: %+v", c)
	}
}

// Baseline is never shared out: the node and GPU records are the same with
// and without clients, under the chassis model.
func TestBaselineIsTheSameWithAndWithoutClients(t *testing.T) {
	plain, ps := clientRecorder(t, nil, nil)
	sampleMinutes(plain, 2)
	withClients, cs := clientRecorder(t, nil, func(int64) []ClientUse {
		return []ClientUse{{Client: "factory-a", Model: "qwen", BusyS: 60}}
	})
	sampleMinutes(withClients, 2)

	from, to := time.Unix(minute0, 0), time.Unix(minute0+120, 0)
	if !reflect.DeepEqual(ps.ReadNode(TierMinute, from, to), cs.ReadNode(TierMinute, from, to)) {
		t.Error("node records differ")
	}
	if !reflect.DeepEqual(ps.ReadGPU(TierMinute, from, to), cs.ReadGPU(TierMinute, from, to)) {
		t.Error("GPU records differ")
	}
	// And the client's share is the model's attributed energy, no more.
	var attributed float64
	for _, g := range cs.ReadGPU(TierMinute, from, to) {
		attributed += g.KWh() * 1000
	}
	if c := theClient(t, cs, "factory-a"); !near(c.Wh, attributed) {
		t.Errorf("factory-a has %v Wh, the model's cards %v Wh", c.Wh, attributed)
	}
}

// The spec's acceptance test, in miniature: one known client on one model for
// one hour gets the model's energy for that hour, within 2%.
func TestOneClientGetsTheModelsEnergy(t *testing.T) {
	rec, s := clientRecorder(t, Direct, func(int64) []ClientUse {
		return []ClientUse{{Client: "factory-a", Model: "qwen", BusyS: 60, Tokens: 1000}}
	})
	sampleMinutes(rec, 60)
	from, to := time.Unix(minute0, 0), time.Unix(minute0+3600, 0)
	model := s.ByModel(TierMinute, from, to)["qwen"] * 1000 // Wh
	c := theClient(t, s, "factory-a")
	if model < 299 || model > 301 {
		t.Fatalf("the model drew %v Wh in the hour, want 300", model)
	}
	if c.Wh < model*0.98 || c.Wh > model*1.02 {
		t.Errorf("factory-a: %v Wh, the model: %v Wh", c.Wh, model)
	}
	if c.BusyS != 3600 || c.Tokens != 60000 {
		t.Errorf("busy seconds and tokens: %+v", c)
	}
}

// Flush at shutdown finalises a minute that has not ended. It is split like
// any other and counted once: a second recorder writing the same minute (a
// restart inside it) leaves the totals where they were.
func TestFlushedPartialMinuteIsCountedOnce(t *testing.T) {
	var asked []int64
	use := func(bucketStart int64) []ClientUse {
		asked = append(asked, bucketStart)
		return []ClientUse{{Client: "factory-a", Model: "qwen", BusyS: 10, Tokens: 50}}
	}
	rec, s := clientRecorder(t, Direct, use)
	m := time.Unix(minute0, 0)
	rec.Sample(m)
	rec.Sample(m.Add(10 * time.Second))
	rec.Flush(m.Add(20 * time.Second))
	if !reflect.DeepEqual(asked, []int64{minute0}) {
		t.Fatalf("asked for %v, want the flushed minute once", asked)
	}
	recs := minuteRecords(s, minute0)
	if len(recs) != 1 || s.ClientName(recs[0].ClientIdx) != "factory-a" || recs[0].BusyS != 10 || recs[0].Wh <= 0 {
		t.Fatalf("the partial minute's client record: %+v", recs)
	}
	first := theClient(t, s, "factory-a")
	if first.Wh <= 0 || first.BusyS != 10 {
		t.Fatalf("totals after the flush: %+v", first)
	}

	// A restart inside the same minute: another recorder, the same store.
	again := NewRecorderWithAttribution(s, 30*time.Second,
		func() (float64, bool) { return 300, true },
		func() []GPUReading {
			return []GPUReading{{GPUID: 0, Watts: 120, Model: "qwen"}, {GPUID: 1, Watts: 180, Model: "qwen"}}
		}, Direct, quiet())
	again.SetClientUse(use)
	again.Sample(m.Add(30 * time.Second))
	again.Flush(m.Add(40 * time.Second))
	if len(asked) != 2 {
		t.Fatalf("the second recorder asked %d times in all, want 2", len(asked))
	}
	// The counters hold; the ring's own minute is rewritten (the node record
	// is whatever the second recorder's samples give), so w_1m and the windows
	// follow the ring and are not compared.
	if second := theClient(t, s, "factory-a"); second.Wh != first.Wh || second.BusyS != first.BusyS || second.Tokens != first.Tokens {
		t.Errorf("the minute was counted twice: %+v then %+v", first, second)
	}
}
