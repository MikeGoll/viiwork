package top

// Key is one action from the keyboard.
type Key int

const (
	KeyUp Key = iota + 1
	KeyDown
	KeyEnter
	KeyEsc
	KeyQuit
	KeySort
	KeyPause
)

// ParseKeys turns bytes read from a raw-mode terminal into keys. A lone ESC in
// one read is the Esc key; ESC [ or ESC O starts a sequence, of which only the
// up and down arrows mean anything here. Other sequences are skipped whole.
func ParseKeys(b []byte) []Key {
	var out []Key
	for i := 0; i < len(b); i++ {
		switch c := b[i]; c {
		case 0x1b:
			if i+1 >= len(b) || (b[i+1] != '[' && b[i+1] != 'O') {
				out = append(out, KeyEsc)
				continue
			}
			j := i + 2
			for j < len(b) && (b[j] < 0x40 || b[j] > 0x7e) { // parameter bytes
				j++
			}
			if j < len(b) && j == i+2 {
				switch b[j] {
				case 'A':
					out = append(out, KeyUp)
				case 'B':
					out = append(out, KeyDown)
				}
			}
			i = j
		case '\r', '\n':
			out = append(out, KeyEnter)
		case 'q', 'Q', 0x03:
			out = append(out, KeyQuit)
		case 'm':
			out = append(out, KeySort)
		case 'p':
			out = append(out, KeyPause)
		}
	}
	return out
}
