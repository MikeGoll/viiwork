// Package prompt asks the setup wizard's questions one line at a time. There
// is no raw mode, so every answer is a line: a script can drive it, and a test
// feeds it a string.
package prompt

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

// ErrAbort is returned when input ends: Ctrl-D, a closed pipe, or a script
// that has run out of answers.
var ErrAbort = errors.New("setup aborted")

// Prompter is what the wizard asks through.
type Prompter interface {
	Say(format string, a ...any)
	Ask(question, def string) (string, error)
	Choose(question string, options []string, def int) (int, error)
	Select(question string, options []string) ([]int, error)
	Confirm(question string, def bool) (bool, error)
}

// Line is the terminal Prompter.
type Line struct {
	in  *bufio.Reader
	out io.Writer
}

func NewLine(in io.Reader, out io.Writer) *Line {
	return &Line{in: bufio.NewReader(in), out: out}
}

// Script is a Line fed one answer per line, for tests.
func Script(out io.Writer, answers ...string) *Line {
	s := strings.Join(answers, "\n")
	if len(answers) > 0 {
		s += "\n"
	}
	return NewLine(strings.NewReader(s), out)
}

func (l *Line) Say(format string, a ...any) { fmt.Fprintf(l.out, format+"\n", a...) }

// line reads one answer. A last line without a newline still counts.
func (l *Line) line() (string, error) {
	s, err := l.in.ReadString('\n')
	if err != nil && s == "" {
		return "", ErrAbort
	}
	return strings.TrimSpace(s), nil
}

// Ask returns the answer, or def for an empty line.
func (l *Line) Ask(question, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(l.out, "%s [%s]: ", question, def)
	} else {
		fmt.Fprintf(l.out, "%s: ", question)
	}
	s, err := l.line()
	if err != nil {
		return "", err
	}
	if s == "" {
		return def, nil
	}
	return s, nil
}

// Choose returns the index of one option. def is the index an empty line
// picks.
func (l *Line) Choose(question string, options []string, def int) (int, error) {
	for i, o := range options {
		fmt.Fprintf(l.out, "  %d) %s\n", i+1, o)
	}
	for {
		s, err := l.Ask(question, strconv.Itoa(def+1))
		if err != nil {
			return 0, err
		}
		if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= len(options) {
			return n - 1, nil
		}
		fmt.Fprintf(l.out, "Enter a number from 1 to %d.\n", len(options))
	}
}

// Select returns one or more option indexes in ascending order. The answer is
// numbers separated by spaces or commas, or "all".
func (l *Line) Select(question string, options []string) ([]int, error) {
	for i, o := range options {
		fmt.Fprintf(l.out, "  %d) %s\n", i+1, o)
	}
	for {
		s, err := l.Ask(question+" (numbers, or all)", "")
		if err != nil {
			return nil, err
		}
		if picked, ok := parseSelection(s, len(options)); ok {
			return picked, nil
		}
		fmt.Fprintf(l.out, "Enter numbers from 1 to %d, separated by spaces or commas, or all.\n", len(options))
	}
}

func parseSelection(s string, n int) ([]int, bool) {
	if strings.EqualFold(s, "all") {
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out, n > 0
	}
	var out []int
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		v, err := strconv.Atoi(f)
		if err != nil || v < 1 || v > n {
			return nil, false
		}
		if !slices.Contains(out, v-1) {
			out = append(out, v-1)
		}
	}
	slices.Sort(out)
	return out, len(out) > 0
}

// Confirm asks a yes or no question.
func (l *Line) Confirm(question string, def bool) (bool, error) {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	for {
		fmt.Fprintf(l.out, "%s [%s]: ", question, hint)
		s, err := l.line()
		if err != nil {
			return false, err
		}
		switch strings.ToLower(s) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		fmt.Fprintln(l.out, "Answer y or n.")
	}
}
