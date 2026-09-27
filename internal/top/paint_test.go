package top

import (
	"strings"
	"testing"
)

func TestPaint(t *testing.T) {
	lines := []Line{{Text: "head  ", Style: Bold}, {Text: "bad", Style: Alert}, {Text: "row", Style: Selected}}
	col := string(Paint(lines, true))
	if !strings.HasPrefix(col, "\x1b[H") || !strings.HasSuffix(col, "\x1b[J") {
		t.Errorf("frame must home the cursor and clear below: %q", col)
	}
	for _, want := range []string{"\x1b[1mhead  \x1b[0m", "\x1b[31mbad\x1b[0m", "\x1b[7mrow\x1b[0m", "\x1b[K\r\n"} {
		if !strings.Contains(col, want) {
			t.Errorf("missing %q in %q", want, col)
		}
	}
	// Without colour only the selected row keeps an emphasis (bold), so the
	// cursor stays visible.
	mono := string(Paint(lines, false))
	if strings.Contains(mono, "\x1b[31m") || strings.Contains(mono, "\x1b[7m") || strings.Contains(mono, "\x1b[1mhead") {
		t.Errorf("colour without colour: %q", mono)
	}
	if !strings.Contains(mono, "\x1b[1mrow") {
		t.Errorf("selected row lost its emphasis: %q", mono)
	}
	if got := Plain(lines); got != "head\nbad\nrow\n" {
		t.Errorf("plain = %q", got)
	}
}

func TestColorEnabled(t *testing.T) {
	env := func(kv ...string) func(string) (string, bool) {
		m := map[string]string{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	cases := []struct {
		env  func(string) (string, bool)
		want bool
	}{
		{env("TERM", "xterm-256color"), true},
		{env("TERM", "xterm-256color", "NO_COLOR", "1"), false},
		{env("TERM", "xterm-256color", "NO_COLOR", ""), true}, // present but empty does not count
		{env("TERM", "dumb"), false},
		{env(), false},
	}
	for i, c := range cases {
		if got := ColorEnabled(c.env); got != c.want {
			t.Errorf("case %d: %v, want %v", i, got, c.want)
		}
	}
}
