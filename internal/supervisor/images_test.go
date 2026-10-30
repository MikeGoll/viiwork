package supervisor

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/engine"
)

// imageEngine is the fake engine plus engine.ImageReader, under its own name
// so that every other test's engine stays one that cannot say.
type imageEngine struct{ fakeEngine }

var imageAns struct {
	sync.Mutex
	reads, ok bool
	err       error
	asked     int
}

func setImages(reads, ok bool, err error) {
	imageAns.Lock()
	defer imageAns.Unlock()
	imageAns.reads, imageAns.ok, imageAns.err, imageAns.asked = reads, ok, err, 0
}

func (*imageEngine) ReadsImages(context.Context, engine.Spec, string) (bool, bool, error) {
	imageAns.Lock()
	defer imageAns.Unlock()
	imageAns.asked++
	return imageAns.reads, imageAns.ok, imageAns.err
}

func init() {
	engine.Register(&imageEngine{fakeEngine{name: "fake-images", client: &http.Client{}}})
}

func imgView(state State, a imageAnswer) backendView {
	return backendView{State: state, Images: a}
}

// A request may land on any healthy backend of the model, so the node only
// says what all of them agree on.
func TestModelImagesIsWhatHealthyBackendsAgreeOn(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name  string
		views []backendView
		want  *bool
	}{
		{"no backend", nil, nil},
		{"all yes", []backendView{imgView(StateHealthy, imagesYes), imgView(StateHealthy, imagesYes)}, &yes},
		{"all no", []backendView{imgView(StateHealthy, imagesNo)}, &no},
		{"one cannot say", []backendView{imgView(StateHealthy, imagesYes), imgView(StateHealthy, imagesUnknown)}, nil},
		{"they disagree", []backendView{imgView(StateHealthy, imagesYes), imgView(StateHealthy, imagesNo)}, nil},
		{"none healthy", []backendView{imgView(StateStarting, imagesYes)}, nil},
		{"an unhealthy one does not count", []backendView{imgView(StateHealthy, imagesYes), imgView(StateUnhealthy, imagesNo)}, &yes},
	}
	for _, c := range cases {
		got := modelImages(c.views)
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("%s: got %v, want %v", c.name, deref(got), deref(c.want))
		}
		if cap := modelCapacity(qwen, c.views); (cap.Images == nil) != (c.want == nil) {
			t.Errorf("%s: capacity images %v", c.name, deref(cap.Images))
		}
		if st := modelStatus(qwen, c.views); (st.Images == nil) != (c.want == nil) {
			t.Errorf("%s: status images %v", c.name, deref(st.Images))
		}
	}
}

func deref(p *bool) string {
	switch {
	case p == nil:
		return "unknown"
	case *p:
		return "yes"
	}
	return "no"
}

func backendImages(b *Backend) imageAnswer {
	return b.view(time.Now(), time.Minute).Images
}

// The answer belongs to a launch. A respawn asks again, and until the new
// process is healthy the last launch's answer is gone.
func TestImagesAnswerIsPerLaunch(t *testing.T) {
	setImages(true, true, nil)
	h := newHarness(t)
	m := fakeModel("img", []int{0})
	m.Engine = "fake-images"
	mod, _ := h.start(m)
	b := mod.Backends()[0]
	waitHealthy(t, b, 3*time.Second)
	within(t, 2*time.Second, "the first answer", func() bool { return backendImages(b) == imagesYes })
	if !h.events.has("reads images") {
		t.Error("a backend that reads images says so once in the activity log")
	}

	setImages(false, true, nil)
	leader := b.PIDs()[0]
	_ = syscall.Kill(leader, syscall.SIGKILL)
	within(t, 2*time.Second, "new leader", func() bool {
		p := b.PIDs()
		return len(p) > 0 && p[0] != leader
	})
	waitHealthy(t, b, 3*time.Second)
	within(t, 2*time.Second, "the second answer", func() bool { return backendImages(b) == imagesNo })
	imageAns.Lock()
	asked := imageAns.asked
	imageAns.Unlock()
	if asked != 1 {
		t.Errorf("asked %d times after the respawn, want once per launch", asked)
	}
}

// An engine that cannot say, or fails, leaves the backend unknown. So does an
// engine without the capability, which is every other test's engine.
func TestImagesUnknownStaysUnknown(t *testing.T) {
	for _, c := range []struct {
		name string
		ok   bool
		err  error
	}{{"cannot say", false, nil}, {"error", true, errors.New("boom")}} {
		setImages(true, c.ok, c.err)
		h := newHarness(t)
		m := fakeModel("imgu", []int{0})
		m.Engine = "fake-images"
		mod, _ := h.start(m)
		b := mod.Backends()[0]
		waitHealthy(t, b, 3*time.Second)
		within(t, 2*time.Second, "the engine to be asked", func() bool {
			imageAns.Lock()
			defer imageAns.Unlock()
			return imageAns.asked > 0
		})
		if got := backendImages(b); got != imagesUnknown {
			t.Errorf("%s: answer %v, want unknown", c.name, got)
		}
	}
	h := newHarness(t)
	mod, _ := h.start(fakeModel("imgn", []int{0}))
	b := mod.Backends()[0]
	waitHealthy(t, b, 3*time.Second)
	time.Sleep(100 * time.Millisecond)
	if got := backendImages(b); got != imagesUnknown {
		t.Errorf("no capability: answer %v, want unknown", got)
	}
}
