//go:build !darwin

package mesh

import (
	"context"
	"errors"
	"testing"
)

// Off darwin the default socket is the only place tailscaled can be, so a
// Linux node's behaviour is exactly what it was.
func TestNoStatusFallbackOffDarwin(t *testing.T) {
	if _, err := tailnetStatusFallback(context.Background()); !errors.Is(err, errNoStatusFallback) {
		t.Errorf("err = %v, want errNoStatusFallback", err)
	}
}
