package uninstall

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"

	"github.com/janit/viiwork/v2/internal/setup/install"
	"github.com/janit/viiwork/v2/internal/setup/prompt"
)

const Usage = `usage: viiwork uninstall [--yes] [--delete-models] [--keep-images] [--from-config]

Removes exactly what ` + "`viiwork init`" + ` recorded in its manifest, and nothing else.
Model weights are kept unless --delete-models is given.

  --yes            skip the typed confirmations (for scripts)
  --delete-models  also delete the model files this node served
  --keep-images    keep the engine image, so a reinstall does not pull it again
  --from-config    on a host set up by hand: list what its config points at, remove nothing
`

// Env is the command's surroundings.
type Env struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// Main is `viiwork uninstall`.
func Main(ctx context.Context, args []string, env Env) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var o Options
	fs.BoolVar(&o.Yes, "yes", false, "")
	fs.BoolVar(&o.DeleteModels, "delete-models", false, "")
	fs.BoolVar(&o.KeepImages, "keep-images", false, "")
	fs.BoolVar(&o.FromConfig, "from-config", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprint(env.Stderr, Usage)
		return 2
	}
	home, _ := os.UserHomeDir()
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	h := Host{GOOS: runtime.GOOS, Home: home, Euid: os.Geteuid(), UID: os.Getuid(), Exec: install.ExecCommand,
		HTTP: &http.Client{Transport: tr}, NodeAPI: "127.0.0.1:8086", Out: env.Stdout}
	err := Run(ctx, h, prompt.NewLine(env.Stdin, env.Stdout), o)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, prompt.ErrAbort):
		fmt.Fprintln(env.Stderr, "\nviiwork uninstall: aborted, nothing was removed")
	default:
		fmt.Fprintf(env.Stderr, "viiwork uninstall: %v\n", err)
	}
	return 1
}
