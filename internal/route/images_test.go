package route

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/supervisor"
	"github.com/janit/viiwork/v2/meshapi"
)

var (
	imgYesP = func() *bool { b := true; return &b }()
	imgNoP  = func() *bool { b := false; return &b }()
)

// imgCap is a peer's row for model "m" with its answer about images.
func imgCap(slots, busy int, images *bool) meshapi.ModelCapacity {
	m := capM(slots, busy, 1)
	m.Images = images
	return m
}

// imgFixture is a fixture whose own answer about images can be set: nil is
// "cannot say".
type imgFixture struct {
	*fixture
	local *bool
}

func newImgFixture(performance bool) *imgFixture {
	f := &imgFixture{fixture: &fixture{local: newFakeLocal(), reports: &fakeReports{}, clock: &clock{now: t0}}}
	f.router = New(Config{
		Self: "self", Local: f.fixture.local, Remote: f.reports, StaleAfter: 3 * time.Second,
		QueueMax: 4, QueueTimeout: time.Second, Now: f.clock.Now, Performance: performance,
		LocalImages: func(string) (bool, bool) {
			if f.local == nil {
				return false, false
			}
			return *f.local, true
		},
	})
	return f
}

func target(t *testing.T, l *Lease) string {
	t.Helper()
	defer l.Release()
	if l.Target().Local {
		return "self"
	}
	return l.Target().Node
}

func TestImageRequestGoesWhereImagesAreRead(t *testing.T) {
	img := Request{Images: true}

	t.Run("a peer that says yes before this node that says no", func(t *testing.T) {
		f := newImgFixture(false)
		f.local = imgNoP
		f.fixture.local.add("m", newFakeBackend("m/0", 4))
		f.reports.set(report("A", t0, time.Millisecond, imgCap(1, 0, imgYesP)), report("B", t0, time.Millisecond, imgCap(8, 0, imgNoP)))
		for range 3 {
			l := f.pick(t, img)
			if got := l.Target().Node; got != "A" {
				t.Fatalf("node = %q, want A", got)
			}
			l.Release()
		}
	})

	t.Run("this node when it says yes", func(t *testing.T) {
		f := newImgFixture(false)
		f.local = imgYesP
		f.fixture.local.add("m", newFakeBackend("m/0", 1))
		f.reports.set(report("A", t0, time.Millisecond, imgCap(8, 0, nil)))
		if got := target(t, f.pick(t, img)); got != "self" {
			t.Errorf("target = %q, want self", got)
		}
	})

	t.Run("a text request is not filtered", func(t *testing.T) {
		f := newImgFixture(false)
		f.local = imgNoP
		f.fixture.local.add("m", newFakeBackend("m/0", 1))
		f.reports.set(report("A", t0, time.Millisecond, imgCap(8, 0, imgYesP)))
		if got := target(t, f.pick(t, Request{})); got != "self" {
			t.Errorf("target = %q, want self: local first, as before", got)
		}
	})

	t.Run("a forward is not filtered", func(t *testing.T) {
		f := newImgFixture(false)
		f.local = imgNoP
		f.fixture.local.add("m", newFakeBackend("m/0", 1))
		if got := target(t, f.pick(t, Request{Images: true, Forwarded: true})); got != "self" {
			t.Errorf("target = %q, want self: the origin chose this node", got)
		}
	})

	t.Run("the scored pick is filtered too", func(t *testing.T) {
		f := newImgFixture(true)
		f.local = imgNoP
		f.fixture.local.add("m", newFakeBackend("m/0", 4))
		fast, slow := scoredCap(4, 0, 100, 100), scoredCap(4, 0, 5000, 5000)
		fast.Images, slow.Images = imgNoP, imgYesP
		f.reports.set(report("fast", t0, time.Millisecond, fast), report("slow", t0, time.Millisecond, slow))
		for range 20 {
			if got := target(t, f.pick(t, Request{Images: true, EstK: 1})); got != "slow" {
				t.Fatalf("target = %q, want slow: the only host that reads images", got)
			}
		}
	})

	t.Run("a session follows the filter", func(t *testing.T) {
		f := newImgFixture(false)
		f.local = imgNoP
		f.fixture.local.add("m", newFakeBackend("m/0", 4))
		f.reports.set(report("A", t0, time.Millisecond, imgCap(4, 0, imgNoP)), report("B", t0, time.Millisecond, imgCap(4, 0, imgYesP)))
		for key := uint64(1); key < 20; key++ {
			if got := target(t, f.pick(t, Request{Images: true, SessionKey: key})); got != "B" {
				t.Fatalf("session %d: target = %q, want B", key, got)
			}
		}
	})

	t.Run("a preferred host that says no is passed over", func(t *testing.T) {
		f := newImgFixture(false)
		f.reports.set(report("A", t0, time.Millisecond, imgCap(4, 0, imgNoP)), report("B", t0, time.Millisecond, imgCap(4, 0, imgYesP)))
		if got := target(t, f.pick(t, Request{Images: true, Prefer: []string{"A", "B"}})); got != "B" {
			t.Errorf("target = %q, want B", got)
		}
	})
}

// A host that cannot say (an older version, an engine without the answer) is
// used only when no host says yes. A mesh of such hosts alone routes as it
// did before the field existed.
func TestImageRequestUnknownOnlyWhenNobodySaysYes(t *testing.T) {
	img := Request{Images: true}

	t.Run("nobody says yes", func(t *testing.T) {
		f := newImgFixture(false)
		f.fixture.local.add("m", newFakeBackend("m/0", 1))
		f.reports.set(report("old", t0, time.Millisecond, imgCap(4, 0, nil)), report("no", t0, time.Millisecond, imgCap(8, 0, imgNoP)))
		if got := target(t, f.pick(t, img)); got != "self" {
			t.Errorf("target = %q, want self: unknown, and local first", got)
		}
		l := f.pick(t, img) // holds the one local slot
		defer l.Release()
		if got := target(t, f.pick(t, img)); got != "old" {
			t.Errorf("target = %q, want old: never the host that says no", got)
		}
	})

	t.Run("somebody says yes and is full", func(t *testing.T) {
		f := newImgFixture(false)
		f.fixture.local.add("m", newFakeBackend("m/0", 1))
		f.reports.set(report("yes", t0, time.Millisecond, imgCap(1, 1, imgYesP)), report("old", t0, time.Millisecond, imgCap(4, 0, nil)))
		if err := f.pickErr(img); !errors.Is(err, ErrNoFreeSlot) {
			t.Errorf("err = %v, want ErrNoFreeSlot: the request waits for the host that reads images", err)
		}
	})

	t.Run("a stale yes proves nothing", func(t *testing.T) {
		f := newImgFixture(false)
		f.fixture.local.add("m", newFakeBackend("m/0", 1))
		f.reports.set(report("yes", t0.Add(-10*time.Second), time.Millisecond, imgCap(4, 0, imgYesP)))
		if got := target(t, f.pick(t, img)); got != "self" {
			t.Errorf("target = %q, want self", got)
		}
	})
}

func TestImageRequestRefusedWhenEveryHostSaysNo(t *testing.T) {
	img := Request{Images: true}

	t.Run("every fresh host says no", func(t *testing.T) {
		f := newImgFixture(false)
		f.local = imgNoP
		f.fixture.local.add("m", newFakeBackend("m/0", 1))
		f.reports.set(report("A", t0, time.Millisecond, imgCap(4, 0, imgNoP)))
		if err := f.pickErr(img); !errors.Is(err, ErrNoImageInput) {
			t.Errorf("err = %v, want ErrNoImageInput", err)
		}
		if _, _, err := f.router.Acquire(context.Background(), Request{Model: "m", Images: true}); !errors.Is(err, ErrNoImageInput) {
			t.Errorf("Acquire err = %v, want ErrNoImageInput at once, not a queue", err)
		}
	})

	t.Run("no fresh host: nothing is ruled out", func(t *testing.T) {
		f := newImgFixture(false)
		f.reports.set(report("A", t0.Add(-10*time.Second), time.Millisecond, imgCap(4, 0, imgNoP)))
		if err := f.pickErr(img); !errors.Is(err, ErrNoFreeSlot) {
			t.Errorf("err = %v, want ErrNoFreeSlot", err)
		}
	})

	t.Run("a peer with no healthy backend says nothing", func(t *testing.T) {
		f := newImgFixture(false)
		down := imgCap(0, 0, nil)
		down.HealthyBackends = 0
		f.reports.set(report("A", t0, time.Millisecond, down))
		if err := f.pickErr(img); !errors.Is(err, ErrNoFreeSlot) {
			t.Errorf("err = %v, want ErrNoFreeSlot", err)
		}
	})

	// This node counts as a peer does: without a healthy backend (loading,
	// down, parked) it says nothing, so it cannot turn the 400 into a wait.
	t.Run("this node has no healthy backend", func(t *testing.T) {
		f := newImgFixture(false)
		b := newFakeBackend("m/0", 1)
		b.set(supervisor.StateStarting)
		f.fixture.local.add("m", b)
		f.reports.set(report("A", t0, time.Millisecond, imgCap(4, 0, imgNoP)))
		if err := f.pickErr(img); !errors.Is(err, ErrNoImageInput) {
			t.Errorf("err = %v, want ErrNoImageInput", err)
		}
		f.reports.set()
		if err := f.pickErr(img); !errors.Is(err, ErrNoFreeSlot) {
			t.Errorf("alone and loading: err = %v, want ErrNoFreeSlot (it queues)", err)
		}
	})

	t.Run("the pinned host says no", func(t *testing.T) {
		f := newImgFixture(false)
		f.reports.set(report("A", t0, time.Millisecond, imgCap(4, 0, imgNoP)), report("B", t0, time.Millisecond, imgCap(4, 0, imgYesP)))
		if err := f.pickErr(Request{Images: true, Host: "A"}); !errors.Is(err, ErrNoImageInput) {
			t.Errorf("err = %v, want ErrNoImageInput: the pin is a demand", err)
		}
		if got := target(t, f.pick(t, Request{Images: true, Host: "B"})); got != "B" {
			t.Errorf("target = %q, want B", got)
		}
	})

	t.Run("this node pinned and it says no", func(t *testing.T) {
		f := newImgFixture(false)
		f.local = imgNoP
		f.fixture.local.add("m", newFakeBackend("m/0", 1))
		f.reports.set(report("B", t0, time.Millisecond, imgCap(4, 0, imgYesP)))
		if err := f.pickErr(Request{Images: true, Host: "self"}); !errors.Is(err, ErrNoImageInput) {
			t.Errorf("err = %v, want ErrNoImageInput", err)
		}
	})
}

// A request that queued with an image is still one when a slot frees: it
// takes the slot of a host that reads images and leaves the others alone,
// and a text request queued behind it is not held up by it.
func TestQueuedImageRequestKeepsItsFilter(t *testing.T) {
	f := &imgFixture{fixture: &fixture{local: newFakeLocal(), reports: &fakeReports{}, clock: &clock{now: time.Now()}}}
	f.local = imgNoP
	f.router = New(Config{
		Self: "self", Local: f.fixture.local, Remote: f.reports, StaleAfter: time.Hour, QueueMax: 8, QueueTimeout: 5 * time.Second,
		LocalImages: func(string) (bool, bool) { return *f.local, true },
	})
	b := newFakeBackend("m/0", 1)
	f.fixture.local.add("m", b)
	now := time.Now()
	f.reports.set(report("eyes", now, time.Millisecond, imgCap(1, 1, imgYesP)))

	held, _, err := f.router.Acquire(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	imgCh := acquireAsync(f.fixture, context.Background(), Request{Model: "m", Images: true})
	waitQueue(t, f.fixture, "m", 1)
	textCh := acquireAsync(f.fixture, context.Background(), Request{Model: "m"})
	waitQueue(t, f.fixture, "m", 2)

	// The local slot frees. The image request is first in line and must not
	// take it; the text request behind it must.
	held.Release()
	text := recv(t, textCh, 2*time.Second)
	if text.err != nil || !text.lease.Target().Local {
		t.Fatalf("text request: %+v, want the local slot", text)
	}
	defer text.lease.Release()
	select {
	case a := <-imgCh:
		t.Fatalf("image request dispatched to %+v, want it still queued", a.lease.Target())
	case <-time.After(100 * time.Millisecond):
	}

	f.reports.set(report("eyes", time.Now(), time.Millisecond, imgCap(1, 0, imgYesP)))
	f.router.Wake()
	img := recv(t, imgCh, 2*time.Second)
	if img.err != nil || img.lease.Target().Node != "eyes" {
		t.Fatalf("image request: %+v, want eyes", img)
	}
	img.lease.Release()
}

// A queued image request is answered, not left to time out, once the last
// host that might read images says it does not.
func TestQueuedImageRequestEndsWhenEveryHostSaysNo(t *testing.T) {
	f := &imgFixture{fixture: &fixture{local: newFakeLocal(), reports: &fakeReports{}, clock: &clock{now: time.Now()}}}
	f.router = New(Config{Self: "self", Local: f.fixture.local, Remote: f.reports, StaleAfter: time.Hour, QueueMax: 8, QueueTimeout: 5 * time.Second})
	f.reports.set(report("eyes", time.Now(), time.Millisecond, imgCap(1, 1, imgYesP)))
	ch := acquireAsync(f.fixture, context.Background(), Request{Model: "m", Images: true})
	waitQueue(t, f.fixture, "m", 1)
	f.reports.set(report("eyes", time.Now(), time.Millisecond, imgCap(1, 0, imgNoP)))
	f.router.Wake()
	if a := recv(t, ch, 2*time.Second); !errors.Is(a.err, ErrNoImageInput) {
		t.Fatalf("err = %v, want ErrNoImageInput", a.err)
	}
}
