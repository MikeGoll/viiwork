package top

import (
	"strings"
	"unicode"
)

// sanitize makes a line safe to write to a terminal. Names, versions and
// errors come from the network — in an open mesh any tailnet host can join
// under any name — so a control byte would reach the terminal as a live escape
// sequence (clear the screen, set the title, write the clipboard), and a rune
// that takes two cells or none would break the width every line is laid out
// to. Each such rune becomes '?', one rune for one: the rune count the layout
// measured stays the line's width in cells.
func sanitize(s string) string {
	if !strings.ContainsFunc(s, unsafeRune) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if unsafeRune(r) {
			return '?'
		}
		return r
	}, s)
}

// unsafeRune is a control character (C0 and C1, tab included), a combining or
// zero-width rune, or one East Asian terminals draw two cells wide.
func unsafeRune(r rune) bool {
	return unicode.IsControl(r) ||
		unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf) ||
		wide(r)
}

// wide covers the East Asian Wide and Fullwidth blocks and the emoji planes,
// without a dependency on a width table.
func wide(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115f, // Hangul Jamo initials
		r >= 0x2e80 && r <= 0xa4cf && r != 0x303f, // CJK radicals .. Yi
		r >= 0xac00 && r <= 0xd7a3,                // Hangul syllables
		r >= 0xf900 && r <= 0xfaff,                // CJK compatibility ideographs
		r >= 0xfe30 && r <= 0xfe4f,                // CJK compatibility forms
		r >= 0xff00 && r <= 0xff60,                // fullwidth forms
		r >= 0xffe0 && r <= 0xffe6,
		r >= 0x1f300 && r <= 0x1faff, // emoji and pictographs
		r >= 0x20000 && r <= 0x3fffd: // CJK extension planes
		return true
	}
	return false
}
