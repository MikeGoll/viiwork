package llamacpp

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

// MinVersion: this viiwork relies on nothing newer than any supported
// llama-server. Raise it (e.g. "b11200") in the change that starts passing a
// flag an older build rejects.
//
// Before raising it: docker/Dockerfile.rocm builds llama.cpp from a
// `git clone --depth 1`, and a shallow clone makes llama-server report
// "build 1" (seen on viiwork:v2.3.1-llama-b11111, 2026-09-26). Every host on
// that image would then be refused as b1. Give that build its real number
// first (-DLLAMA_BUILD_NUMBER, or a full clone).
func (e *Engine) MinVersion() string { return "" }

// buildRe reads the build number from either spelling llama-server has used:
// "version: 0.1.0-dev (build 10438, commit …)" and the older
// "version: 10437 (<commit>)".
var buildRe = regexp.MustCompile(`(?m)^version: (?:[^\n]*\(build ([0-9]+)|([0-9]+) )`)

// Version runs `llama-server --version`, which prints "version: <build>
// (<commit>)" among its startup lines, and reports the build as "b<build>" —
// the spelling llama.cpp's release tags and docker/Dockerfile.rocm use.
func (e *Engine) Version(ctx context.Context, s engine.Spec) (string, error) {
	o, err := options(s)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, o.Binary, "--version")
	// A wrapper script's children may hold the pipe after it exits; what was
	// printed by then is all the check needs.
	cmd.WaitDelay = 2 * time.Second
	out, runErr := cmd.CombinedOutput()
	if m := buildRe.FindSubmatch(out); m != nil {
		n := m[1]
		if len(n) == 0 {
			n = m[2]
		}
		return "b" + string(n), nil
	}
	if runErr != nil {
		return "", fmt.Errorf("%s --version: %v", o.Binary, runErr)
	}
	return "", fmt.Errorf("%s --version printed no build number", o.Binary)
}

// AtLeast compares build numbers.
func (e *Engine) AtLeast(installed, min string) (bool, error) {
	a, err := buildNumber(installed)
	if err != nil {
		return false, err
	}
	b, err := buildNumber(min)
	if err != nil {
		return false, err
	}
	return a >= b, nil
}

func buildNumber(v string) (int, error) {
	n, ok := strings.CutPrefix(v, "b")
	if !ok {
		return 0, fmt.Errorf("%q is not a llama.cpp build (bNNNNN)", v)
	}
	return strconv.Atoi(n)
}
