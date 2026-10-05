package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"slices"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/engine/llamacpp"
	"github.com/janit/viiwork/v2/internal/setup/install"
	"github.com/janit/viiwork/v2/internal/update"
)

// A Mac wizard install keeps its llama.cpp builds under one root, one per
// tag, and a model it configured names one of them. That model runs the
// build of the pin this viiwork carries, when the build is there: an update
// fetches its pin's build while staging, so activating the release moves the
// engine with the binary, and a rollback moves it back, with nothing
// recorded anywhere. A binary configured anywhere else is used as written,
// and a node that is not a wizard install on a Mac follows nothing.

// llamaFollow is what a node needs to know to follow its pin.
type llamaFollow struct {
	root    string // the install's llama root; "" follows nothing
	pin     string // the llama.cpp pin this binary carries
	present func(root, tag string) bool
}

// newLlamaFollow reads the install manifest, when there is one.
func newLlamaFollow(o Options, models []config.Model, logf func(string, ...any)) llamaFollow {
	if o.InstallManifest == "" {
		return llamaFollow{}
	}
	man, err := install.ReadManifest(o.InstallManifest)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logf("llama.cpp: %v; every model runs its binary as configured", err)
		}
		return llamaFollow{}
	}
	return llamaFollow{root: install.LlamaRootFor(man, llamaBinaries(models)), pin: o.LlamaPin, present: install.Fetched}
}

// follow is models with every managed llama-server replaced by pin's build
// where that build is present. models itself is never modified.
func (l llamaFollow) follow(models []config.Model, pin string) []config.Model {
	if l.root == "" {
		return models
	}
	var out []config.Model
	for i, m := range models {
		bin, ok := llamaBinary(m)
		if !ok {
			continue
		}
		if b := install.FollowPin(bin, l.root, pin, l.present); b != bin {
			if out == nil {
				out = slices.Clone(models)
			}
			out[i] = withLlamaBinary(m, b)
		}
	}
	if out == nil {
		return models
	}
	return out
}

// applyModels hands the supervisor models as this viiwork runs them, and
// says which ones follow its pin.
func (n *Node) applyModels(models []config.Model) error {
	run := n.llama.follow(models, n.llama.pin)
	for i := range models {
		if a, _ := llamaBinary(models[i]); a != "" {
			if b, _ := llamaBinary(run[i]); b != a {
				n.logf("llama.cpp: model %s runs %s, the build of this viiwork's pin (configured: %s)", models[i].Name, b, a)
			}
		}
	}
	return n.sup.Apply(run)
}

// prepareLlama is the Stager's PrepareLlama on a node that follows its pin:
// it fetches the staged release's build, checked against the digest signed
// into that release, and judges the engine requirement against it.
func (n *Node) prepareLlama(fetch install.Fetch, models func() []config.Model) func(context.Context, update.BuildInfo) (func(context.Context, map[string]string) error, error) {
	l := n.llama
	return func(ctx context.Context, info update.BuildInfo) (func(context.Context, map[string]string) error, error) {
		// A release without the digest predates following the pin: it runs
		// the engine as configured, which Engines checks.
		if info.LlamaCpp == "" || info.LlamaCppMacSHA256 == "" {
			return nil, nil
		}
		if !install.ValidTag(info.LlamaCpp) {
			return nil, fmt.Errorf("the release's llama.cpp pin %q is not a tag", info.LlamaCpp)
		}
		if _, _, err := fetch.Llama(ctx, info.LlamaCpp, l.root, info.LlamaCppMacSHA256); err != nil {
			return nil, fmt.Errorf("llama.cpp %s: %w", info.LlamaCpp, err)
		}
		return func(ctx context.Context, required map[string]string) error {
			return update.CheckEngines(ctx, l.follow(models(), info.LlamaCpp), required)
		}, nil
	}
}

// llamaFetch downloads builds into the llama root. With a pinned digest it
// never asks GitHub's API; the token is for a lookup that does.
func (n *Node) llamaFetch() install.Fetch {
	return install.Fetch{
		HTTP: &http.Client{}, API: install.LlamaReleaseAPI, Releases: install.LlamaReleases,
		Token: func() string { v, _ := n.o.LookupEnv("GITHUB_TOKEN"); return v },
		Exec:  install.ExecCommand, Out: io.Discard,
	}
}

// pruneLlama is the Confirmer's PruneEngines on a node that follows its pin.
// It keeps the build of every release a rollback or restart could run —
// current, last good, previous, the floor, this process — and every build a
// model is configured with, which a release that predates following runs.
// When any of those cannot be read, it removes nothing.
func (n *Node) pruneLlama(dir string) func(update.State) {
	l := n.llama
	return func(st update.State) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		keep, err := n.keptLlama(ctx, dir, st)
		if err != nil {
			n.logf("llama.cpp: keeping every build: %v", err)
			return
		}
		removed, err := install.PruneLlama(l.root, keep)
		if len(removed) > 0 {
			n.logf("llama.cpp: removed %v, which no kept release runs (kept %v)", removed, keep)
		}
		if err != nil {
			n.logf("llama.cpp: pruning builds: %v", err)
		}
	}
}

func (n *Node) keptLlama(ctx context.Context, dir string, st update.State) ([]string, error) {
	l := n.llama
	var keep []string
	add := func(tag string) {
		if tag != "" && !slices.Contains(keep, tag) {
			keep = append(keep, tag)
		}
	}
	add(l.pin)
	if st.Launcher == nil {
		return nil, errors.New("no floor is recorded in state.json")
	}
	bins := []string{st.Launcher.Path}
	for _, v := range []string{st.Current, st.LastGood, st.Previous} {
		if v == "" || v == update.Builtin {
			continue
		}
		b, err := update.Binary(dir, v)
		if err != nil {
			return nil, err
		}
		bins = append(bins, b)
	}
	for _, b := range bins {
		info, err := update.ReadBuildInfo(ctx, b)
		if err != nil {
			return nil, err
		}
		add(info.LlamaCpp)
	}
	for _, b := range llamaBinaries(n.runningConfig().Models) {
		if root, tag, ok := install.ManagedLlama(b); ok && root == l.root {
			add(tag)
		}
	}
	return keep, nil
}

// llamaBinaries is the llama-server every llamacpp model is configured with.
func llamaBinaries(models []config.Model) []string {
	var out []string
	for _, m := range models {
		if b, ok := llamaBinary(m); ok {
			out = append(out, b)
		}
	}
	return out
}

// llamaBinary reads a llamacpp model's binary as written. A model that
// writes none runs llama-server from PATH, which is nothing a wizard manages.
func llamaBinary(m config.Model) (string, bool) {
	if m.Engine != llamacpp.Name {
		return "", false
	}
	if v := binaryNode(m.EngineBlock()); v != nil {
		return v.Value, true
	}
	return "", false
}

func binaryNode(block yaml.Node) *yaml.Node {
	if block.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(block.Content); i += 2 {
		if k, v := block.Content[i], block.Content[i+1]; k.Value == "binary" && v.Kind == yaml.ScalarNode {
			return v
		}
	}
	return nil
}

// withLlamaBinary is m running binary, sharing nothing with m that it changes.
func withLlamaBinary(m config.Model, binary string) config.Model {
	block := m.EngineBlock()
	block.Content = slices.Clone(block.Content)
	for i := 0; i+1 < len(block.Content); i += 2 {
		if block.Content[i].Value == "binary" {
			v := *block.Content[i+1]
			v.Value = binary
			block.Content[i+1] = &v
		}
	}
	opts := make(map[string]yaml.Node, len(m.Options))
	for k, v := range m.Options {
		opts[k] = v
	}
	opts[m.Engine] = block
	m.Options = opts
	return m
}
