package joincode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/plistenv"
	"github.com/janit/viiwork/v2/meshapi"
)

const Usage = `usage: viiwork join-code [--node host:port] [--config path] [--secret-env NAME] [--open]

Prints the code a new machine pastes into its setup to join this mesh: this
node's gossip address as a seed and, in a secured mesh, the mesh secret. The
code IS the mesh secret — handle it like /etc/viiwork/mesh.env.

  --node host:port    this node's API (default 127.0.0.1:<api.port> from --config, else 127.0.0.1:8086)
  --config path       viiwork.yaml, read for api.port, mesh.bind_port and mesh.secret_env
  --secret-env NAME   variable holding the mesh secret (default mesh.secret_env, else VIIWORK_MESH_SECRET)
  --open              the mesh is open: print a code without a secret
`

type Env struct {
	Stdout, Stderr io.Writer
	LookupEnv      func(string) (string, bool)
	Client         *http.Client
	ReadFile       func(string) ([]byte, error)
	// Plist is a LaunchAgent to read the secret from when the variable is
	// unset: on a Mac, `viiwork init` keeps the secret only there. Empty
	// elsewhere.
	Plist string
}

// Run is `viiwork join-code`.
func Run(ctx context.Context, args []string, env Env) int {
	fs := flag.NewFlagSet("join-code", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	node := fs.String("node", "", "")
	configPath := fs.String("config", "", "")
	secretEnv := fs.String("secret-env", "", "")
	open := fs.Bool("open", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprint(env.Stderr, Usage)
		return 2
	}
	defaults := config.Defaults()
	port, gossip, envName := defaults.API.Port, defaults.Mesh.BindPort, config.DefaultMeshSecretEnv
	if *configPath != "" {
		data, err := env.ReadFile(*configPath)
		if err != nil {
			fmt.Fprintf(env.Stderr, "reading %s: %v\n", *configPath, err)
			return 2
		}
		cfg, err := config.Parse(data)
		if err != nil {
			fmt.Fprintf(env.Stderr, "%s: %v\n", *configPath, err)
			return 2
		}
		port, gossip = cfg.API.Port, cfg.Mesh.BindPort
		if cfg.Mesh.SecretEnv != "" {
			envName = cfg.Mesh.SecretEnv
		}
		*open = *open || cfg.Mesh.Open
	}
	if *secretEnv != "" {
		envName = *secretEnv
	}
	if *node == "" {
		*node = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	}
	var st meshapi.NodeStatus
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+*node+meshapi.PathStatus, nil)
	resp, err := env.Client.Do(req)
	if err != nil {
		fmt.Fprintf(env.Stderr, "reading this node's address from %s: %v\n", *node, err)
		return 1
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil || st.Addr == "" {
		fmt.Fprintf(env.Stderr, "%s did not report its address\n", *node)
		return 1
	}
	c := Code{Seed: net.JoinHostPort(st.Addr, strconv.Itoa(gossip))}
	if !*open {
		v, ok := env.LookupEnv(envName)
		if (!ok || v == "") && env.Plist != "" {
			if data, err := env.ReadFile(env.Plist); err == nil {
				v, ok = plistenv.Value(data, envName)
			}
		}
		if !ok || v == "" {
			where := "load it (set -a; . /etc/viiwork/mesh.env; set +a)"
			if env.Plist != "" {
				where = "it is read from " + env.Plist + " when set there, or set it yourself"
			}
			fmt.Fprintf(env.Stderr, "no mesh secret in %s: %s, or pass --open for an open mesh\n", envName, where)
			return 1
		}
		if c.Secret, err = base64.StdEncoding.DecodeString(v); err != nil || len(c.Secret) != secretLen {
			fmt.Fprintf(env.Stderr, "%s must be the standard base64 encoding of a %d-byte mesh secret\n", envName, secretLen)
			return 1
		}
	}
	code, err := Encode(c)
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	if c.Secret != nil {
		fmt.Fprintln(env.Stderr, "This code contains the mesh secret. Paste it only into the setup of a machine you trust, and do not post it anywhere.")
	}
	fmt.Fprintf(env.Stdout, "%s\n", code)
	return 0
}
