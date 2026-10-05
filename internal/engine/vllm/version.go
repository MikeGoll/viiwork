package vllm

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/janit/viiwork/v2/internal/engine"
)

// MinVersion: no requirement yet; raise it with the change that needs it.
func (e *Engine) MinVersion() string { return "" }

var semverRe = regexp.MustCompile(`^([0-9]+)\.([0-9]+)\.([0-9]+)`)

// Version runs `vllm --version`, whose last line matching X.Y.Z is the version
// (it may log first).
func (e *Engine) Version(ctx context.Context, s engine.Spec) (string, error) {
	opts := defaultOptions()
	if err := engine.DecodeOptions(s, &opts); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, opts.Binary, "--version")
	// A wrapper script's children may hold the pipe after it exits; what was
	// printed by then is all the check needs.
	cmd.WaitDelay = 2 * time.Second
	out, runErr := cmd.CombinedOutput()
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); semverRe.MatchString(l) {
			return l, nil
		}
	}
	if runErr != nil {
		return "", fmt.Errorf("%s --version: %v", opts.Binary, runErr)
	}
	return "", fmt.Errorf("%s --version printed no version", opts.Binary)
}

// AtLeast compares X.Y.Z numerically; a suffix (0.11.0rc1) counts as X.Y.Z.
func (e *Engine) AtLeast(installed, min string) (bool, error) {
	a, err := triple(installed)
	if err != nil {
		return false, err
	}
	b, err := triple(min)
	if err != nil {
		return false, err
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i], nil
		}
	}
	return true, nil
}

func triple(v string) ([3]int, error) {
	m := semverRe.FindStringSubmatch(v)
	if m == nil {
		return [3]int{}, fmt.Errorf("%q is not a vLLM version", v)
	}
	var out [3]int
	for i := range out {
		out[i], _ = strconv.Atoi(m[i+1])
	}
	return out, nil
}
