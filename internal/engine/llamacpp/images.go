package llamacpp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/janit/viiwork/v2/internal/engine"
	"github.com/janit/viiwork/v2/internal/gguf"
)

// projectorArch is the GGUF architecture of a vision projector.
const projectorArch = "clip"

// Flags of llama-server that decide whether it reads images. A hub model may
// come with a projector the node never sees, so those leave the answer open.
var (
	projectorFlags   = []string{"--mmproj", "-mm", "--mmproj-url", "-mmu"}
	noProjectorFlags = []string{"--no-mmproj"}
	hubFlags         = []string{"-hf", "-hfr", "--hf-repo"}
)

// hasFlag reports whether args carry one of flags, as "--flag value" or
// "--flag=value".
func hasFlag(args []string, flags ...string) (string, bool) {
	for _, a := range args {
		for _, f := range flags {
			if a == f || strings.HasPrefix(a, f+"=") {
				return f, true
			}
		}
	}
	return "", false
}

// validateMmproj applies the rules about llamacpp.mmproj that need no file,
// and the one about a file that is there. A missing file is left to Command:
// validation also runs where the node's files cannot be seen (the setup
// wizard checks container paths from the host).
func validateMmproj(path string, o Options, s engine.Spec) error {
	if o.Mmproj == "" {
		return nil
	}
	if !filepath.IsAbs(o.Mmproj) {
		return fmt.Errorf("%s.llamacpp.mmproj must be an absolute path to the model's vision projector (got %q)", path, o.Mmproj)
	}
	if f, ok := hasFlag(s.Args, append(projectorFlags, noProjectorFlags...)...); ok {
		return fmt.Errorf("%s.args must not carry %s beside %s.llamacpp.mmproj: name the projector in one place", path, f, path)
	}
	info, err := gguf.Read(o.Mmproj)
	switch {
	case os.IsNotExist(err):
		return nil
	case err != nil:
		return fmt.Errorf("%s.llamacpp.mmproj: %w", path, err)
	case info.Arch != projectorArch:
		return fmt.Errorf("%s.llamacpp.mmproj %s is not a vision projector: its architecture is %q, a projector's is %q", path, o.Mmproj, info.Arch, projectorArch)
	}
	return nil
}

// ReadsImages is engine.ImageReader, answered from what the backend was
// started with: llama-server reads images exactly when it loaded a
// projector. It needs no request, so addr is not used.
func (e *Engine) ReadsImages(_ context.Context, s engine.Spec, _ string) (reads, ok bool, err error) {
	o, err := options(s)
	if err != nil {
		return false, false, err
	}
	if _, off := hasFlag(s.Args, noProjectorFlags...); off {
		return false, true, nil
	}
	if _, on := hasFlag(s.Args, projectorFlags...); on || o.Mmproj != "" {
		return true, true, nil
	}
	if _, hub := hasFlag(s.Args, hubFlags...); hub {
		return false, false, nil
	}
	return false, true, nil
}
