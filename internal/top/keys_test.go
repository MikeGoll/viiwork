package top

import (
	"slices"
	"testing"
)

func TestParseKeys(t *testing.T) {
	cases := []struct {
		in   string
		want []Key
	}{
		{"q", []Key{KeyQuit}},
		{"\x03", []Key{KeyQuit}}, // Ctrl-C arrives as a byte in raw mode
		{"\x1b[A\x1b[B", []Key{KeyUp, KeyDown}},
		{"\x1bOA", []Key{KeyUp}}, // application cursor mode
		{"\r", []Key{KeyEnter}},
		{"\x1b", []Key{KeyEsc}},
		{"mpx", []Key{KeySort, KeyPause}}, // unknown bytes are ignored
		{"\x1b[5~", nil},                  // unhandled sequences are skipped whole
	}
	for _, c := range cases {
		if got := ParseKeys([]byte(c.in)); !slices.Equal(got, c.want) {
			t.Errorf("%q: %v, want %v", c.in, got, c.want)
		}
	}
}
