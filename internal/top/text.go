package top

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// width is a string's width in terminal cells. Every glyph this package draws
// (box drawing, blocks, arrows, the em dash) is one cell wide.
func width(s string) int { return utf8.RuneCountInString(s) }

// fit pads or truncates s to exactly w cells, marking a cut with an ellipsis.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	n := width(s)
	if n <= w {
		return s + strings.Repeat(" ", w-n)
	}
	r := []rune(s)
	return string(r[:w-1]) + "…"
}

// bar is a horizontal gauge of pct percent, w cells wide.
func bar(pct float64, w int) string {
	n := int(math.Round(clamp(pct, 0, 100) / 100 * float64(w)))
	return strings.Repeat("█", n) + strings.Repeat("░", w-n)
}

var levels = []rune("▁▂▃▄▅▆▇█")

// spark is one cell showing pct percent.
func spark(pct float64) string {
	i := int(math.Round(clamp(pct, 0, 100) / 100 * float64(len(levels)-1)))
	return string(levels[i])
}

// graph is the newest w samples as sparks, oldest on the left.
func graph(h []Sample, w int, value func(Sample) float64) string {
	if w <= 0 {
		return ""
	}
	if len(h) > w {
		h = h[len(h)-w:]
	}
	var b strings.Builder
	for _, s := range h {
		b.WriteString(spark(value(s)))
	}
	return b.String()
}

// dur formats a duration for a column: 12s, 3m04s, 5h12m.
func dur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int64(d / time.Second)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	default:
		return fmt.Sprintf("%dh%02dm", s/3600, s%3600/60)
	}
}

func clamp(v, lo, hi float64) float64 {
	if math.IsNaN(v) {
		return lo
	}
	return math.Max(lo, math.Min(hi, v))
}
