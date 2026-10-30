package supervisor

import (
	"context"

	"github.com/janit/viiwork/v2/internal/engine"
)

// imageAnswer is what an engine said about one backend reading images. The
// zero value is "did not say", so a backend that was never asked, or whose
// engine has no engine.ImageReader, is unknown and not "no".
type imageAnswer int8

const (
	imagesUnknown imageAnswer = iota
	imagesYes
	imagesNo
)

// checkImages asks the engine whether this launch reads images. Once per
// launch, on the transition to healthy, like checkGPUBinding: the answer
// follows from what the process was started with, and a respawn asks again.
// An error leaves the answer unknown until the next launch.
func (l *loop) checkImages() {
	b := l.b
	if _, ok := b.eng.(engine.ImageReader); !ok {
		return
	}
	addr := b.Addr()
	if addr == "" {
		return
	}
	b.mu.Lock()
	spec := b.spec
	b.mu.Unlock()
	ctx, cancel := context.WithTimeout(l.ctx, b.deps.Health.Timeout.Duration)
	defer cancel()
	reads, ok := engine.ImagesOf(ctx, b.eng, spec, addr)
	if !ok {
		return
	}
	ans := imagesNo
	if reads {
		ans = imagesYes
		b.emit("reads images")
	}
	b.mu.Lock()
	b.images = ans
	b.mu.Unlock()
}

// modelImages is a model's answer on this node: what its healthy backends
// agree on. A request may land on any of them, so one that cannot say, or
// two that disagree, make the model unknown. Nil is unknown.
func modelImages(views []backendView) *bool {
	ans, seen := imagesUnknown, false
	for _, v := range views {
		if v.State != StateHealthy {
			continue
		}
		if v.Images == imagesUnknown || (seen && v.Images != ans) {
			return nil
		}
		ans, seen = v.Images, true
	}
	if !seen {
		return nil
	}
	reads := ans == imagesYes
	return &reads
}

// Images is whether model reads images on this node; known is false when the
// node cannot say, and for a model it does not run.
func (s *Supervisor) Images(model string) (reads, known bool) {
	m, ok := s.Model(model)
	if !ok {
		return false, false
	}
	if p := modelImages(s.views(m)); p != nil {
		return *p, true
	}
	return false, false
}
