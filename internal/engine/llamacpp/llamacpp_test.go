package llamacpp

import (
	"testing"

	"github.com/janit/viiwork/v2/internal/engine"
)

func TestUsageReporting(t *testing.T) {
	const wantUnasked = false // Task 0 Step 1: llama-server sent no usage chunk unasked
	u := engine.UsageOf(&Engine{})
	if u.Unasked != wantUnasked || !u.CachedTokens {
		t.Fatalf("llamacpp usage = %+v", u)
	}
}
