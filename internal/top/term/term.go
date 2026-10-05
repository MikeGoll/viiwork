// Package term is the only part of viiwork top that touches the terminal:
// raw mode, the alternate screen, the size, and putting it all back.
package term

import (
	"io"
	"os"
	"sync"

	xterm "golang.org/x/term"
)

// Terminal is a terminal in raw mode on the alternate screen.
type Terminal struct {
	in   *os.File
	out  io.Writer
	old  *xterm.State
	once sync.Once
	err  error
}

// IsTerminal reports whether f is a terminal.
func IsTerminal(f *os.File) bool { return xterm.IsTerminal(int(f.Fd())) }

// Open puts in into raw mode and switches out to the alternate screen with the
// cursor hidden. The caller must call Restore, which is safe to call twice.
func Open(in *os.File, out io.Writer) (*Terminal, error) {
	old, err := xterm.MakeRaw(int(in.Fd()))
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(out, "\x1b[?1049h\x1b[?25l"); err != nil {
		_ = xterm.Restore(int(in.Fd()), old)
		return nil, err
	}
	return &Terminal{in: in, out: out, old: old}, nil
}

// Size is the terminal's width and height in cells.
func (t *Terminal) Size() (int, int, error) { return xterm.GetSize(int(t.in.Fd())) }

func (t *Terminal) Write(p []byte) (int, error) { return t.out.Write(p) }

// Restore shows the cursor, leaves the alternate screen and restores the
// terminal's mode. Only the first call does anything.
func (t *Terminal) Restore() error {
	t.once.Do(func() {
		_, _ = io.WriteString(t.out, "\x1b[?25h\x1b[?1049l")
		t.err = xterm.Restore(int(t.in.Fd()), t.old)
	})
	return t.err
}
