package mesh

import (
	"context"
	"os/exec"
)

func tailnetStatusFallback(ctx context.Context) ([]byte, error) {
	return darwinStatusSource{fetch: fetchLocalAPIStatus, lookPath: exec.LookPath, run: runCommand}.read(ctx)
}
