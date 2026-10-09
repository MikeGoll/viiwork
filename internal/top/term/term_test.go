package term

import (
	"os"
	"testing"
)

// A pipe is not a terminal: IsTerminal says so and Open refuses it, so the
// command falls back to --once rather than writing escape codes into a file.
func TestPipeIsNotATerminal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if IsTerminal(r) {
		t.Fatal("a pipe reported as a terminal")
	}
	if _, err := Open(r, w); err == nil {
		t.Fatal("Open succeeded on a pipe")
	}
}
