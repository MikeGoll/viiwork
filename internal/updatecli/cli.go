// Package updatecli is `viiwork update`: rolling a signed release across the
// mesh one host at a time, reading every member's /v1/update, and sending one
// host back a release. Like `viiwork alias` it talks only to
// nodes' HTTP APIs, and it verifies nothing itself: the signature, the
// engines and the confirmation are each node's job.
package updatecli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/meshauth"
	"github.com/janit/viiwork/v2/internal/plistenv"
	"github.com/janit/viiwork/v2/internal/release"
	"github.com/janit/viiwork/v2/internal/update"
	"github.com/janit/viiwork/v2/meshapi"
)

// Phrase is what an operator types to start a rollout. There is no --yes.
const Phrase = "YES I WANT TO UPDATE THE NODES ABOVE"

const Usage = `usage: viiwork update [--to vX.Y.Z] [--hosts a,b] [--allow-downgrade] [--confirm PHRASE] [flags]
       viiwork update status [--json] [flags]
       viiwork update rollback --host NAME [flags]

A rollout stages the release on every chosen host first, then activates them one
at a time — the node you talk to last — and waits for each to confirm itself
before the next. A host that rolls itself back stops the rollout.

A rollback sends one host to its last good release, or, when it is already on
it, to the release that was last good before it (PREVIOUS in status).

flags:
  --node host:port    the node to read the mesh from (default 127.0.0.1:<api.port> from --config, else 127.0.0.1:8086)
  --config path       viiwork.yaml, read only for api.port and mesh.secret_env
  --secret-env NAME   variable holding the mesh secret (default mesh.secret_env, else VIIWORK_MESH_SECRET)
  --repo owner/name   where the latest release is looked up when --to is not given (default janit/viiwork)
`

const (
	secretBytes  = 32
	stageTimeout = 20 * time.Minute
	writeTimeout = 30 * time.Second
	readTimeout  = 10 * time.Second
	// statusTimeout is longer: a node's first GET /v1/update reads every
	// engine's --version, and vLLM's imports torch.
	statusTimeout = 90 * time.Second
)

// Env is everything the command touches outside itself.
type Env struct {
	Stdout, Stderr io.Writer
	Stdin          io.Reader
	LookupEnv      func(string) (string, bool)
	Hostname       func() (string, error)
	Client         *http.Client
	ReadFile       func(string) ([]byte, error)
	// Plist is the node's LaunchAgent, read for the mesh secret when the
	// variable is unset (a Mac installed by viiwork init). "" elsewhere.
	Plist     string
	GitHubAPI string        // "" = https://api.github.com
	PollEvery time.Duration // 0 = 2s
	ComeBack  time.Duration // 0 = 10m: how long a restarting host may be unreachable
}

func withDefaults(env Env) Env {
	if env.Stdout == nil {
		env.Stdout = os.Stdout
	}
	if env.Stderr == nil {
		env.Stderr = os.Stderr
	}
	if env.Stdin == nil {
		env.Stdin = os.Stdin
	}
	if env.LookupEnv == nil {
		env.LookupEnv = os.LookupEnv
	}
	if env.Hostname == nil {
		env.Hostname = os.Hostname
	}
	if env.Client == nil {
		env.Client = &http.Client{}
	}
	if env.ReadFile == nil {
		env.ReadFile = os.ReadFile
	}
	if env.GitHubAPI == "" {
		env.GitHubAPI = "https://api.github.com"
	}
	if env.PollEvery <= 0 {
		env.PollEvery = 2 * time.Second
	}
	if env.ComeBack <= 0 {
		env.ComeBack = 10 * time.Minute
	}
	return env
}

type cli struct {
	env    Env
	node   string
	signer *meshauth.Signer
}

// host is one member as the CLI sees it: its API address and release state,
// or why it has none.
type host struct {
	name, addr string
	status     meshapi.UpdateStatus
	err        error
}

// Run is `viiwork update`. Exit 0 on success, 1 on failure, 2 on a usage error.
func Run(ctx context.Context, args []string, env Env) int {
	env = withDefaults(env)
	cmd := ""
	if len(args) > 0 && (args[0] == "status" || args[0] == "rollback") {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	node := fs.String("node", "", "")
	configPath := fs.String("config", "", "")
	secretEnv := fs.String("secret-env", "", "")
	repo := fs.String("repo", "janit/viiwork", "")
	to := fs.String("to", "", "")
	hosts := fs.String("hosts", "", "")
	allowDowngrade := fs.Bool("allow-downgrade", false, "")
	confirm := fs.String("confirm", "", "")
	hostFlag := fs.String("host", "", "")
	asJSON := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprint(env.Stderr, Usage)
		return 2
	}
	c, code := newCLI(env, *node, *configPath, *secretEnv)
	if code != 0 {
		return code
	}
	switch cmd {
	case "status":
		return c.status(ctx, *asJSON)
	case "rollback":
		if *hostFlag == "" {
			fmt.Fprint(env.Stderr, Usage)
			return 2
		}
		return c.rollback(ctx, *hostFlag)
	}
	return c.rollout(ctx, rolloutOpts{to: *to, repo: *repo, hosts: *hosts, allowDowngrade: *allowDowngrade, confirm: *confirm})
}

// newCLI resolves the node and the signer exactly as `viiwork alias` does.
func newCLI(env Env, node, configPath, secretEnv string) (*cli, int) {
	c := &cli{env: env}
	port := config.Defaults().API.Port
	envName := config.DefaultMeshSecretEnv
	if configPath != "" {
		data, err := env.ReadFile(configPath)
		if err != nil {
			fmt.Fprintf(env.Stderr, "reading %s: %v\n", configPath, err)
			return nil, 2
		}
		cfg, err := config.Parse(data)
		if err != nil {
			fmt.Fprintf(env.Stderr, "%s: %v\n", configPath, err)
			return nil, 2
		}
		port = cfg.API.Port
		if cfg.Mesh.SecretEnv != "" {
			envName = cfg.Mesh.SecretEnv
		}
	}
	c.node = node
	if c.node == "" {
		c.node = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	}
	if secretEnv != "" {
		envName = secretEnv
	}
	v, ok := env.LookupEnv(envName)
	if (!ok || v == "") && env.Plist != "" {
		if data, err := env.ReadFile(env.Plist); err == nil {
			v, ok = plistenv.Value(data, envName)
		}
	}
	if ok && v != "" {
		secret, err := base64.StdEncoding.DecodeString(v)
		if err != nil || len(secret) != secretBytes {
			fmt.Fprintf(env.Stderr, "%s must be the standard base64 encoding of a %d-byte mesh secret\n", envName, secretBytes)
			return nil, 2
		}
		h, err := env.Hostname()
		if err != nil || h == "" {
			h = "unknown"
		}
		if c.signer, err = meshauth.NewSigner(secret, "viiwork-cli@"+h); err != nil {
			fmt.Fprintf(env.Stderr, "%s: %v\n", envName, err)
			return nil, 2
		}
	}
	return c, 0
}

// errNoUpdateAPI is a member older than the update feature.
var errNoUpdateAPI = errors.New("no /v1/update (older than rolling updates)")

func (c *cli) getJSON(ctx context.Context, url string, v any) error {
	return c.getJSONWithin(ctx, url, v, readTimeout)
}

func (c *cli) getJSONWithin(ctx context.Context, url string, v any, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.env.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errNoUpdateAPI
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(v)
}

// updateStatus is one member's GET /v1/update.
func (c *cli) updateStatus(ctx context.Context, addr string) (meshapi.UpdateStatus, error) {
	var st meshapi.UpdateStatus
	err := c.getJSONWithin(ctx, "http://"+addr+meshapi.PathUpdate, &st, statusTimeout)
	return st, err
}

// view is the mesh as the CLI sees it: every machine, sorted by name, with its
// release state; the node the CLI reads it from (the entry node); and whether
// the mesh is open or secured.
type view struct {
	hosts []host
	entry string
	mode  string
}

// members reads the mesh from the entry node and every member's release state,
// in parallel. The entry node is addressed exactly as the CLI reached it (its
// loopback address when the CLI runs on its machine): in an open mesh that is
// the only address from which it accepts update writes.
func (c *cli) members(ctx context.Context) (view, error) {
	var cl meshapi.ClusterResponse
	if err := c.getJSON(ctx, "http://"+c.node+meshapi.PathCluster, &cl); err != nil {
		return view{}, fmt.Errorf("reading the mesh from %s: %w", c.node, err)
	}
	var out []host
	for _, m := range cl.Members {
		if m.Role == meshapi.RoleGateway {
			continue
		}
		h := host{name: m.Node}
		switch {
		case m.State != meshapi.MemberAlive:
			h.err = fmt.Errorf("not alive (%s)", m.State)
		case m.Status == nil:
			h.err = errors.New("no status yet")
		case m.Node == cl.View:
			h.addr = c.node
		case m.Status.Addr == "":
			h.err = errors.New("no API address")
		default:
			h.addr = net.JoinHostPort(m.Status.Addr, strconv.Itoa(m.Status.APIPort))
		}
		out = append(out, h)
	}
	var wg sync.WaitGroup
	for i := range out {
		if out[i].addr == "" {
			continue
		}
		wg.Add(1)
		go func(h *host) {
			defer wg.Done()
			h.status, h.err = c.updateStatus(ctx, h.addr)
		}(&out[i])
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return view{hosts: out, entry: cl.View, mode: cl.Mesh}, nil
}

// errOpenMesh is a write to another machine in an open mesh, which that
// machine refuses: it accepts update writes only from its own loopback.
var errOpenMesh = errors.New("open mesh: a node accepts update writes only from its own machine — run viiwork update there")

// canWrite says why this CLI cannot write to the mesh at all, before anything
// is asked or sent.
func (c *cli) canWrite(v view) error {
	if v.mode == meshapi.MeshSecured && c.signer == nil {
		return errors.New("this mesh is secured and no mesh secret is loaded, so every node would refuse the writes: " +
			"sudo sh -c 'set -a; . /etc/viiwork/mesh.env; viiwork update …' on a Linux node, " +
			"or run it on a Mac node installed by viiwork init, which reads the secret from its LaunchAgent (or --secret-env)")
	}
	return nil
}

// post sends one signed write and returns the node's refusal as an error.
// A reply the caller wants is decoded into out, when out is not nil.
func (c *cli) post(ctx context.Context, addr, path string, in, out any, timeout time.Duration) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.signer != nil {
		if _, err := c.signer.SignRequest(req, body); err != nil {
			return err
		}
	}
	resp, err := c.env.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 {
		var e meshapi.ErrorResponse
		msg := strings.TrimSpace(string(b))
		if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}
	if out != nil {
		// The write took effect; a reply it cannot read does not undo that.
		json.Unmarshal(b, out)
	}
	return nil
}

func (c *cli) status(ctx context.Context, asJSON bool) int {
	v, err := c.members(ctx)
	if err != nil {
		fmt.Fprintln(c.env.Stderr, err)
		return 1
	}
	hosts := v.hosts
	if asJSON {
		out := map[string]any{}
		for _, h := range hosts {
			if h.err != nil {
				out[h.name] = map[string]string{"error": h.err.Error()}
			} else {
				out[h.name] = h.status
			}
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Fprintln(c.env.Stdout, string(b))
		return 0
	}
	tw := tabwriter.NewWriter(c.env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "HOST\tRUNNING\tCURRENT\tLAST GOOD\tPREVIOUS\tPENDING\tUPDATES\tENGINES")
	for _, h := range hosts {
		if h.err != nil {
			fmt.Fprintf(tw, "%s\t—\t—\t—\t—\t—\t%s\t\n", h.name, h.err)
			continue
		}
		s := h.status
		pending, enabled, previous := "—", "off", "—"
		if s.Previous != "" {
			previous = s.Previous
		}
		if s.Pending != nil {
			pending = fmt.Sprintf("%s (start %d)", s.Pending.Version, s.Pending.Attempts)
		}
		if s.Enabled {
			enabled = "on"
		}
		var engines []string
		for e, v := range s.Engines {
			engines = append(engines, e+" "+v)
		}
		sort.Strings(engines)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", h.name, s.Running, s.Current, s.LastGood, previous, pending, enabled, strings.Join(engines, ", "))
	}
	tw.Flush()
	return 0
}

func (c *cli) rollback(ctx context.Context, name string) int {
	v, err := c.members(ctx)
	if err != nil {
		fmt.Fprintln(c.env.Stderr, err)
		return 1
	}
	if err := c.canWrite(v); err != nil {
		fmt.Fprintln(c.env.Stderr, err)
		return 1
	}
	for _, h := range v.hosts {
		if h.name != name {
			continue
		}
		if v.mode == meshapi.MeshOpen && h.name != v.entry {
			fmt.Fprintf(c.env.Stderr, "%s: %v\n", name, errOpenMesh)
			return 1
		}
		if h.addr == "" {
			fmt.Fprintf(c.env.Stderr, "%s: %v\n", name, h.err)
			return 1
		}
		// The node decides where it goes: its last good release, or the one
		// before that when it is already on its last good release.
		var reply struct {
			To string `json:"rolling_back_to"`
		}
		if err := c.post(ctx, h.addr, meshapi.PathUpdateRollback, struct{}{}, &reply, writeTimeout); err != nil {
			fmt.Fprintf(c.env.Stderr, "%s: %v\n", name, err)
			return 1
		}
		if reply.To == "" {
			reply.To = "its last good release"
		}
		fmt.Fprintf(c.env.Stdout, "%s: rolling back to %s\n", name, reply.To)
		return 0
	}
	fmt.Fprintf(c.env.Stderr, "%s is not a member of this mesh\n", name)
	return 1
}

type rolloutOpts struct {
	to, repo, hosts, confirm string
	allowDowngrade           bool
}

var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// latest is the newest release of repo on GitHub, refused until it carries
// its signature: nodes would refuse it anyway, but only after every stage.
func (c *cli) latest(ctx context.Context, repo string) (string, error) {
	if !repoRe.MatchString(repo) {
		return "", fmt.Errorf("--repo %q is not owner/name", repo)
	}
	var rel struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
		} `json:"assets"`
	}
	if err := c.getJSON(ctx, strings.TrimSuffix(c.env.GitHubAPI, "/")+"/repos/"+repo+"/releases/latest", &rel); err != nil {
		return "", fmt.Errorf("looking up the latest release of %s: %w", repo, err)
	}
	for _, a := range rel.Assets {
		if a.Name == "SHA256SUMS.sig" {
			return rel.TagName, nil
		}
	}
	return "", fmt.Errorf("%s is not signed yet (no SHA256SUMS.sig): run scripts/release-sign.sh first", rel.TagName)
}

// skipReason is why h is left out of a rollout to version to, or "".
func skipReason(h host, v view, to string, o rolloutOpts, selected map[string]bool) string {
	switch {
	case len(selected) > 0 && !selected[h.name]:
		return "not selected (--hosts)"
	case h.err != nil:
		return h.err.Error()
	case v.mode == meshapi.MeshOpen && h.name != v.entry:
		return errOpenMesh.Error()
	case !h.status.Enabled:
		return "update.enabled is off"
	}
	if cmp, ok := update.Compare(h.status.Running, to); ok {
		switch {
		case cmp == 0:
			return "already on " + to
		case cmp > 0 && h.status.Current == meshapi.UpdateBuiltin:
			// Running is the installed binary itself: at restart the
			// newer-floor rule would undo the downgrade, after reloading
			// every model for nothing.
			return "the installed binary is newer than " + to + "; a downgrade would be undone at restart"
		case cmp > 0 && !o.allowDowngrade:
			return "newer than " + to + " (--allow-downgrade to go back)"
		}
	}
	return ""
}

func (c *cli) rollout(ctx context.Context, o rolloutOpts) int {
	if o.to != "" && !release.ValidVersion(o.to) {
		fmt.Fprintf(c.env.Stderr, "%q is not a release version\n", o.to)
		return 2
	}
	v, err := c.members(ctx)
	if err != nil {
		fmt.Fprintln(c.env.Stderr, err)
		return 1
	}
	hosts, entry := v.hosts, v.entry
	if err := c.canWrite(v); err != nil {
		fmt.Fprintln(c.env.Stderr, err)
		return 1
	}
	to := o.to
	if to == "" {
		if to, err = c.latest(ctx, o.repo); err != nil {
			fmt.Fprintln(c.env.Stderr, err)
			return 1
		}
	}
	if !release.ValidVersion(to) {
		fmt.Fprintf(c.env.Stderr, "%q is not a release version\n", to)
		return 2
	}
	selected := map[string]bool{}
	for _, h := range strings.Split(o.hosts, ",") {
		if h = strings.TrimSpace(h); h != "" {
			selected[h] = true
		}
	}
	// A typo must not quietly shrink the rollout, or make it a no-op that
	// exits 0.
	known := map[string]bool{}
	for _, h := range hosts {
		known[h.name] = true
	}
	var unknown []string
	for name := range selected {
		if !known[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		fmt.Fprintf(c.env.Stderr, "--hosts names machines that are not in this mesh: %s\n", strings.Join(unknown, ", "))
		return 2
	}

	var targets []host
	tw := tabwriter.NewWriter(c.env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "HOST\tRUNNING\tPLAN\n")
	for _, h := range hosts {
		reason := skipReason(h, v, to, o, selected)
		running := h.status.Running
		if running == "" {
			running = "—"
		}
		if reason != "" {
			fmt.Fprintf(tw, "%s\t%s\tskip: %s\n", h.name, running, reason)
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t→ %s\n", h.name, running, to)
		targets = append(targets, h)
	}
	tw.Flush()
	if len(targets) == 0 {
		fmt.Fprintln(c.env.Stdout, "nothing to update")
		return 0
	}

	if !c.confirmed(o.confirm) {
		fmt.Fprintln(c.env.Stderr, "aborted: the confirmation phrase did not match")
		return 1
	}

	// Stage everywhere first, so a download, signature or engine problem is
	// found before any host restarts.
	failed := false
	for _, h := range targets {
		fmt.Fprintf(c.env.Stdout, "staging %s on %s…\n", to, h.name)
		if err := c.post(ctx, h.addr, meshapi.PathUpdateStage, meshapi.UpdateRequest{Version: to}, nil, stageTimeout); err != nil {
			fmt.Fprintf(c.env.Stderr, "%s: stage failed: %v\n", h.name, err)
			failed = true
		}
	}
	if failed {
		fmt.Fprintln(c.env.Stderr, "staging failed: nothing was activated")
		return 1
	}

	// One at a time, the node we read the mesh from last.
	sort.SliceStable(targets, func(i, j int) bool {
		return targets[i].name != entry && targets[j].name == entry
	})
	for _, h := range targets {
		fmt.Fprintf(c.env.Stdout, "activating %s on %s…\n", to, h.name)
		req := meshapi.UpdateRequest{Version: to, AllowDowngrade: o.allowDowngrade}
		if err := c.post(ctx, h.addr, meshapi.PathUpdateActivate, req, nil, writeTimeout); err != nil {
			fmt.Fprintf(c.env.Stderr, "%s: activate failed: %v; later hosts untouched\n", h.name, err)
			return 1
		}
		if err := c.waitConfirmed(ctx, h, to); err != nil {
			fmt.Fprintf(c.env.Stderr, "rollout stopped at %s: %v; later hosts untouched\n", h.name, err)
			return 1
		}
		if err := c.waitInMesh(ctx, h, to); err != nil {
			fmt.Fprintf(c.env.Stderr, "rollout stopped at %s: confirmed, but %v; later hosts untouched\n", h.name, err)
			return 1
		}
		fmt.Fprintf(c.env.Stdout, "%s: on %s, confirmed\n", h.name, to)
	}
	fmt.Fprintf(c.env.Stdout, "rollout of %s complete\n", to)
	return 0
}

// confirmed checks the phrase, from --confirm or typed on stdin.
func (c *cli) confirmed(flagValue string) bool {
	if flagValue != "" {
		return flagValue == Phrase
	}
	fmt.Fprintf(c.env.Stdout, "Type %q to continue: ", Phrase)
	line, _ := bufio.NewReader(c.env.Stdin).ReadString('\n')
	return strings.TrimSpace(line) == Phrase
}

// waitInMesh waits until the node the CLI talks to sees h alive on to. A host
// confirms itself from its own backends only, so a release that breaks mesh
// membership would confirm while alone; checking the entry node's view keeps
// the rollout from splitting the fleet one host at a time. When h is the entry
// node itself, its view must still hold another alive node if it had one.
func (c *cli) waitInMesh(ctx context.Context, h host, to string) error {
	start := time.Now()
	var last string
	for {
		var cl meshapi.ClusterResponse
		if err := c.getJSON(ctx, "http://"+c.node+meshapi.PathCluster, &cl); err != nil {
			last = err.Error()
		} else {
			others, peers := 0, 0
			found := false
			for _, m := range cl.Members {
				if m.Role != "" && m.Role != meshapi.RoleNode {
					continue
				}
				if m.Node == h.name {
					found = m.State == meshapi.MemberAlive && m.Status != nil && m.Status.Ver == to
					continue
				}
				peers++
				if m.State == meshapi.MemberAlive {
					others++
				}
			}
			switch {
			case cl.View == h.name && found && (peers == 0 || others > 0):
				return nil
			case cl.View != h.name && found:
				return nil
			case cl.View == h.name:
				last = "it sees no other node alive"
			default:
				last = cl.View + " does not see it alive on " + to
			}
		}
		if time.Since(start) > c.env.ComeBack {
			return fmt.Errorf("it is not back in the mesh: %s", last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.env.PollEvery):
		}
	}
}

// waitConfirmed follows one host through its restart until it runs to and has
// confirmed it. Unreachable while restarting is expected, for at most
// ComeBack. The old process answering first (running the old version, current
// already to) is still restarting. A host that comes back on anything but to,
// or does not confirm by its own deadline plus a minute, has failed.
func (c *cli) waitConfirmed(ctx context.Context, h host, to string) error {
	start := time.Now()
	var deadline time.Time
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.env.PollEvery):
		}
		st, err := c.updateStatus(ctx, h.addr)
		if err != nil {
			if deadline.IsZero() && time.Since(start) > c.env.ComeBack {
				return fmt.Errorf("unreachable for %s after activate: %v", c.env.ComeBack, err)
			}
			if !deadline.IsZero() && time.Now().After(deadline.Add(time.Minute)) {
				return fmt.Errorf("unreachable past its confirmation deadline: %v", err)
			}
			continue
		}
		switch {
		case st.Running == to && st.Pending == nil && st.LastGood == to:
			return nil
		case st.Current == meshapi.UpdateBuiltin && st.Pending == nil:
			return fmt.Errorf("it came back on its installed binary (running %s), not %s", st.Running, to)
		case st.Current != to && st.Pending == nil:
			return fmt.Errorf("it rolled back (running %s, current %s)", st.Running, st.Current)
		}
		if st.Pending != nil && st.Pending.Deadline != "" {
			if d, err := time.Parse(time.RFC3339, st.Pending.Deadline); err == nil {
				deadline = d
			}
		}
		if !deadline.IsZero() && time.Now().After(deadline.Add(time.Minute)) {
			return fmt.Errorf("not confirmed by its deadline %s", deadline.Format(time.RFC3339))
		}
		if deadline.IsZero() && time.Since(start) > c.env.ComeBack {
			return fmt.Errorf("did not come back on %s within %s", to, c.env.ComeBack)
		}
	}
}
