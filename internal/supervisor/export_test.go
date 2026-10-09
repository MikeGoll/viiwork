package supervisor

import (
	"time"

	"github.com/janit/viiwork/v2/internal/engine"
)

// lastLoad is the last successful engine load since the current launch, with
// its time (zero when there is none). A test hook: production code reads the
// fields under b.mu where it needs them.
func (b *Backend) lastLoad() (load engine.Load, decoded, remain int64, at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.load, b.decoded, b.remain, b.loadAt
}
