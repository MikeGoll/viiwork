package top

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/janit/viiwork/v2/meshapi"
)

// cellSafe is the set of runes a frame may contain: printable ASCII, the
// Latin-1 letters, the replacement mark, and the glyphs the renderer draws
// itself. Measured independently of the code under test, so a rune that
// takes two cells or none, or a control byte, fails here even when the
// renderer's own rune count says the line fits.
func cellSafe(r rune) bool {
	switch {
	case r >= 0x20 && r < 0x7f:
		return true
	case r >= 0xc0 && r <= 0xff && unicode.IsLetter(r):
		return true
	}
	return strings.ContainsRune("?…█░─·→↑↓—▁▂▃▄▅▆▇€*", r)
}

func TestNetworkStringsCannotEscapeOrWiden(t *testing.T) {
	s := fixture()
	b := s.Cluster.Members[0].Status // node-b
	b.Ver = "v\x1b]0;pwned\x07"
	b.Models[0].Name = "evil\x1b[2J\tmodel"
	b.GPUs[0].Name = "漢字漢字漢字漢字漢字漢字漢字漢字"
	b.Models[0].Backends = []meshapi.BackendStatus{{ID: "é​x\u009b31m", Status: meshapi.StatusHealthy, Phase: "\x1b[31mred"}}
	s.Cluster.Members = append(s.Cluster.Members, meshapi.Member{
		Node: "é-漢字漢字漢字漢字漢字漢字", Role: meshapi.RoleNode, State: meshapi.MemberAlive,
		Status: &meshapi.NodeStatus{Node: "x", Models: []meshapi.ModelStatus{{Name: "漢字漢字漢字漢字漢字漢字漢字漢字漢字", Slots: 1}}},
	})
	s.Err = "dial: \x1b[2Jboom"
	for _, v := range []View{{}, {Screen: Host, Host: "node-b"}} {
		for _, w := range []int{60, 120} {
			for _, l := range Render(s, v, w, 60, t0) {
				if n := utf8.RuneCountInString(l.Text); n > w {
					t.Errorf("screen %d w=%d: %d cells: %q", v.Screen, w, n, l.Text)
				}
				for _, r := range l.Text {
					if !cellSafe(r) {
						t.Errorf("screen %d w=%d: unsafe rune %U in %q", v.Screen, w, r, l.Text)
						break
					}
				}
			}
		}
	}
}
