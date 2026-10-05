package nodectl

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/janit/viiwork/v2/internal/setup/install"
)

const Usage = `usage: viiwork stop     (sudo on Linux)
       viiwork start    (sudo on Linux)

stop stops this machine's node and every model it runs: the node leaves the
mesh first and finishes in-flight requests, so members route elsewhere. It
stays stopped until viiwork start, or the next boot (Linux) or login (Mac).
Only a machine set up by ` + "`viiwork init`" + `; other nodes are not touched.
`

// Env is the command's surroundings.
type Env struct{ Stdout, Stderr io.Writer }

// Main is `viiwork stop` (verb "stop") and `viiwork start` (verb "start").
func Main(ctx context.Context, verb string, args []string, env Env) int {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprint(env.Stderr, Usage)
		return 2
	}
	home, _ := os.UserHomeDir()
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DisableKeepAlives = true // every check is a fresh connection to a process that may be gone
	h := Host{GOOS: runtime.GOOS, Home: home, Euid: os.Geteuid(), UID: os.Getuid(), Exec: install.ExecCommand,
		HTTP: &http.Client{Transport: tr, Timeout: 5 * time.Second}, NodeAPI: "127.0.0.1:8086", Out: env.Stdout,
		Wait: 120 * time.Second, Poll: time.Second}
	do := Stop
	if verb == "start" {
		do = Start
	}
	if err := do(ctx, h); err != nil {
		fmt.Fprintf(env.Stderr, "viiwork %s: %v\n", verb, err)
		return 1
	}
	return 0
}
