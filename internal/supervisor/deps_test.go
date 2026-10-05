package supervisor

import (
	"testing"
	"time"
)

// The fetch timings are the operator-visible cadence of a viiwork-parrot
// download: every test overrides them, so without this nothing would notice a
// default going missing or a zero field no longer being filled.
func TestFetchTimingDefaults(t *testing.T) {
	def := DefaultTiming()
	if def.FetchPoll != 5*time.Second || def.FetchRetryMax != 60*time.Second || def.FetchLogEvery != 60*time.Second {
		t.Errorf("DefaultTiming fetch values = %v/%v/%v, want 5s/60s/60s", def.FetchPoll, def.FetchRetryMax, def.FetchLogEvery)
	}

	// A Deps with no Timing at all must come out on the defaults: this is the
	// path node.Options.SupervisorTiming takes when a deployment sets nothing.
	got := Deps{}.withDefaults().Timing
	if got.FetchPoll != def.FetchPoll || got.FetchRetryMax != def.FetchRetryMax || got.FetchLogEvery != def.FetchLogEvery {
		t.Errorf("withDefaults fetch timing = %v/%v/%v, want the defaults", got.FetchPoll, got.FetchRetryMax, got.FetchLogEvery)
	}

	// A partially set Timing keeps what the caller set and fills the rest.
	part := Deps{Timing: Timing{FetchPoll: 250 * time.Millisecond}}.withDefaults().Timing
	if part.FetchPoll != 250*time.Millisecond {
		t.Errorf("FetchPoll = %v, want the caller's 250ms", part.FetchPoll)
	}
	if part.FetchRetryMax != def.FetchRetryMax || part.FetchLogEvery != def.FetchLogEvery {
		t.Errorf("unset fields = %v/%v, want the defaults", part.FetchRetryMax, part.FetchLogEvery)
	}
}
