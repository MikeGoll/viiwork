package meshauth

import (
	"errors"
	"math"
	"strconv"
	"testing"
	"time"
)

// A timestamp at any extreme of int64 is outside the window. The far-future
// ones used to pass: now.Sub saturated at the minimum Duration, whose negation
// is itself, so the "absolute" skew was negative and under the window.
func TestCheckSkewExtremes(t *testing.T) {
	s, err := NewSigner([]byte("0123456789abcdef0123456789abcdef"), "gb1")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 500_000_000)
	s.now = func() time.Time { return now }
	w := int64(SkewWindow / time.Second)

	for _, secs := range []int64{
		math.MaxInt64, math.MinInt64, math.MaxInt64 - 62135596800, math.MinInt64 + 1,
		1 << 62, -(1 << 62), 0, -1,
		now.Unix() + w + 1, now.Unix() - w - 1,
	} {
		if err := s.checkSkew(strconv.FormatInt(secs, 10)); !errors.Is(err, ErrBadProof) {
			t.Errorf("checkSkew(%d) = %v, want ErrBadProof", secs, err)
		}
	}
	for _, secs := range []int64{now.Unix(), now.Unix() + w, now.Unix() - w} {
		if err := s.checkSkew(strconv.FormatInt(secs, 10)); err != nil {
			t.Errorf("checkSkew(%d) = %v, want accepted", secs, err)
		}
	}
}
