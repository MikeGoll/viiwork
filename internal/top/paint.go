package top

import (
	"bytes"
	"strings"
)

var styleCodes = map[Style]string{Bold: "\x1b[1m", Dim: "\x1b[2m", Alert: "\x1b[31m", Selected: "\x1b[7m"}

// Paint is one full frame: cursor home, every line cleared to its end, and
// everything below cleared, written in one piece so the screen never flickers.
// Raw mode needs \r\n. Selection stays visible without colour through bold.
func Paint(lines []Line, color bool) []byte {
	var b bytes.Buffer
	b.WriteString("\x1b[H")
	for i, l := range lines {
		code := ""
		if color {
			code = styleCodes[l.Style]
		} else if l.Style == Selected {
			code = "\x1b[1m"
		}
		if code != "" {
			b.WriteString(code + l.Text + "\x1b[0m")
		} else {
			b.WriteString(l.Text)
		}
		b.WriteString("\x1b[K")
		if i < len(lines)-1 {
			b.WriteString("\r\n")
		}
	}
	b.WriteString("\x1b[J")
	return b.Bytes()
}

// Plain is a frame as text, for --once and for a stdout that is not a terminal.
func Plain(lines []Line) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(strings.TrimRight(l.Text, " ") + "\n")
	}
	return b.String()
}

// ColorEnabled follows no-color.org (NO_COLOR present and non-empty) and
// refuses colour on a dumb or unknown terminal.
func ColorEnabled(lookup func(string) (string, bool)) bool {
	if v, ok := lookup("NO_COLOR"); ok && v != "" {
		return false
	}
	term, _ := lookup("TERM")
	return term != "" && term != "dumb"
}
