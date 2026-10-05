// Package modelscli is `viiwork down` and `viiwork up`: park this machine's
// models — their engines stop and their GPUs are freed — and bring them back,
// while the node stays in the mesh. It talks only to the node's HTTP API
// (PathModelsDown, PathModelsUp), reaching it and signing exactly as
// `viiwork update` does.
package modelscli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/janit/viiwork/v2/internal/updatecli"
	"github.com/janit/viiwork/v2/meshapi"
)

const Usage = `usage: viiwork down [flags] [model...]
       viiwork up [flags] [model...]

down parks this machine's models (every configured one, or those named): the
node stops admitting requests to them, gives the ones in flight
health.respawn_grace, then stops their engines and frees their GPUs. The node
stays in the mesh — dashboards, top and routing to other hosts keep working —
and members send those models elsewhere. up loads them again from the running
config. Parked models stay down until viiwork up or the node restarts.

flags:
  --node host:port    the node (default 127.0.0.1:<api.port> from --config, else 127.0.0.1:8086)
  --config path       viiwork.yaml, read only for api.port and mesh.secret_env
  --secret-env NAME   variable holding the mesh secret (default mesh.secret_env, else VIIWORK_MESH_SECRET)
`

// Run is `viiwork down` (verb "down") and `viiwork up` (verb "up"). Exit 0 on
// success, 1 on failure, 2 on a usage error.
func Run(ctx context.Context, verb string, args []string, env updatecli.Env) int {
	stderr := env.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	node := fs.String("node", "", "")
	configPath := fs.String("config", "", "")
	secretEnv := fs.String("secret-env", "", "")
	if err := fs.Parse(args); err != nil || (verb != "down" && verb != "up") {
		fmt.Fprint(stderr, Usage)
		return 2
	}
	models := fs.Args()
	for _, m := range models {
		if strings.HasPrefix(m, "-") {
			fmt.Fprintf(stderr, "viiwork %s: flags go before the model names (%s)\n\n%s", verb, m, Usage)
			return 2
		}
	}
	t, code := updatecli.Connect(env, *node, *configPath, *secretEnv)
	if code != 0 {
		return code
	}
	path := meshapi.PathModelsUp
	if verb == "down" {
		path = meshapi.PathModelsDown
	}
	var resp meshapi.ParkResponse
	if err := t.Post(ctx, path, meshapi.ParkRequest{Models: models}, &resp); err != nil {
		fmt.Fprintf(stderr, "viiwork %s: %s: %v\n", verb, t.Node(), err)
		if strings.Contains(err.Error(), "HTTP 401") && !t.Signed() {
			fmt.Fprintf(stderr, "this mesh is secured and no mesh secret is loaded: "+
				"sudo sh -c 'set -a; . /etc/viiwork/mesh.env; viiwork %s …' on a Linux node, or --secret-env\n", verb)
		}
		return 1
	}
	out := env.Stdout
	if out == nil {
		out = io.Discard
	}
	host := resp.Node
	if host == "" {
		host = t.Node()
	}
	if len(resp.Models) == 0 {
		fmt.Fprintf(out, "%s: no models configured\n", host)
		return 0
	}
	width := 0
	for _, m := range resp.Models {
		width = max(width, len(m.Name))
	}
	for _, m := range resp.Models {
		fmt.Fprintf(out, "%s  %-*s  %s\n", host, width, m.Name, describe(m, verb))
	}
	return 0
}

func describe(m meshapi.ModelPark, verb string) string {
	switch {
	case verb == "down" && m.Changed:
		return "down: in-flight requests finish, then its engines stop and free their GPUs"
	case verb == "down":
		return "already down"
	case m.Changed:
		return "up: loading (progress in viiwork top or /mesh)"
	}
	return "already up"
}
