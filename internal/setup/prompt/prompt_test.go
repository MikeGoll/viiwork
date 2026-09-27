package prompt

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

func TestAskDefault(t *testing.T) {
	var out bytes.Buffer
	p := Script(&out, "", "custom")
	if s, err := p.Ask("Dir", "/models"); err != nil || s != "/models" {
		t.Errorf("empty line: %q, %v", s, err)
	}
	if s, err := p.Ask("Dir", "/models"); err != nil || s != "custom" {
		t.Errorf("answer: %q, %v", s, err)
	}
	if !strings.Contains(out.String(), "Dir [/models]: ") {
		t.Errorf("prompt: %q", out.String())
	}
}

func TestChooseRetriesThenPicks(t *testing.T) {
	var out bytes.Buffer
	i, err := Script(&out, "9", "x", "2").Choose("Mesh", []string{"a", "b", "c"}, 0)
	if err != nil || i != 1 {
		t.Fatalf("got %d, %v", i, err)
	}
	if !strings.Contains(out.String(), "  3) c") || !strings.Contains(out.String(), "from 1 to 3") {
		t.Errorf("output: %q", out.String())
	}
	if i, err := Script(io.Discard, "").Choose("Mesh", []string{"a", "b"}, 1); err != nil || i != 1 {
		t.Errorf("default: %d, %v", i, err)
	}
}

func TestSelect(t *testing.T) {
	for in, want := range map[string][]int{"1, 3": {0, 2}, "3 1 3": {0, 2}, "all": {0, 1, 2}, "ALL": {0, 1, 2}} {
		got, err := Script(io.Discard, in).Select("Models", []string{"a", "b", "c"})
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%q: %v, %v", in, got, err)
		}
	}
	// Nothing, out of range and zero are asked again.
	got, err := Script(io.Discard, "", "0", "4", "2").Select("Models", []string{"a", "b", "c"})
	if err != nil || !slices.Equal(got, []int{1}) {
		t.Errorf("retry: %v, %v", got, err)
	}
}

func TestConfirm(t *testing.T) {
	p := Script(io.Discard, "", "yes", "N", "maybe", "y")
	for i, want := range []bool{true, true, false, true} {
		got, err := p.Confirm("Go?", true)
		if err != nil || got != want {
			t.Errorf("answer %d: %v, %v", i, got, err)
		}
	}
	if got, _ := Script(io.Discard, "").Confirm("Go?", false); got {
		t.Error("default no answered yes")
	}
}

func TestEndOfInputAborts(t *testing.T) {
	if _, err := Script(io.Discard).Ask("q", "d"); !errors.Is(err, ErrAbort) {
		t.Errorf("Ask: %v", err)
	}
	if _, err := Script(io.Discard, "9").Choose("q", []string{"a"}, 0); !errors.Is(err, ErrAbort) {
		t.Errorf("Choose after a bad answer: %v", err)
	}
	if _, err := Script(io.Discard).Confirm("q", true); !errors.Is(err, ErrAbort) {
		t.Errorf("Confirm: %v", err)
	}
}

func TestLastLineWithoutNewline(t *testing.T) {
	if s, err := NewLine(strings.NewReader("abc"), io.Discard).Ask("q", ""); err != nil || s != "abc" {
		t.Errorf("%q, %v", s, err)
	}
}
