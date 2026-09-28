// Package llamacpp is the llama.cpp engine: llama-server backends that are
// ready when /health answers 200 and whose occupancy is read from /slots. The
// command line and host tuning (auto --threads, auto --no-mmap, the malloc
// environment) are lifted from the v1 process manager.
package llamacpp

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/janit/viiwork/v2/internal/engine"
	"github.com/janit/viiwork/v2/internal/hostinfo"
)

func init() { engine.Register(New()) }

var _ engine.Engine = (*Engine)(nil)

// Engine runs llama-server. The function fields fix the host in tests.
type Engine struct {
	client    *http.Client
	nproc     func() int
	totalRAM  func() int64
	modelSize func(path string) (int64, error)
	// levelLogs reports whether a llama-server binary reads --log-verbosity
	// as levels (1 = errors only). nil in tests means yes.
	levelLogs func(binary string) bool
}

// New returns the engine for this host. Its HTTP client keeps up to 100 idle
// connections (v1's healthClient) and has no client-wide timeout: every probe
// and load poll is bounded by its caller's context.
func New() *Engine {
	return &Engine{
		client: &http.Client{Transport: &http.Transport{
			MaxIdleConns:    100,
			IdleConnTimeout: 30 * time.Second,
		}},
		nproc:     runtime.NumCPU,
		totalRAM:  hostinfo.TotalRAMBytes,
		modelSize: modelTotalSize,
		levelLogs: cachedLevelLogs,
	}
}

func (e *Engine) Name() string { return Name }

// DefaultStartupTimeout is the spec's 10 minutes. Large split models on slow
// risers need more, set per model with startup_timeout.
func (e *Engine) DefaultStartupTimeout() time.Duration { return 10 * time.Minute }

// mallocEnv is v1's heap-fragmentation fix for long-running llama-server
// processes: allocations over 64 KB use mmap and are returned on free, the
// heap is trimmed aggressively, and arenas are capped.
var mallocEnv = []string{
	"MALLOC_MMAP_THRESHOLD_=65536",
	"MALLOC_TRIM_THRESHOLD_=65536",
	"MALLOC_ARENA_MAX=4",
}

// Command builds the llama-server command line for one backend. The model's
// own args come last, so an operator's flag wins: llama.cpp takes the last
// occurrence of a repeated flag.
func (e *Engine) Command(s engine.Spec) (engine.Command, error) {
	o, err := options(s)
	if err != nil {
		return engine.Command{}, fmt.Errorf("llamacpp: model %s: %w", s.Name, err)
	}

	// A viiwork-parrot folder model resolves to a directory. A missing path
	// is left to llama-server, as before; only an existing directory is
	// refused here, because it can never load.
	if fi, err := os.Stat(s.Path); err == nil && fi.IsDir() {
		return engine.Command{}, fmt.Errorf("llamacpp: model %s: %s is a directory; llama.cpp loads a GGUF file, so a folder model needs an engine that takes a directory (vllm, freetoken)", s.Name, s.Path)
	}

	parallel := max(s.Parallel, 1)

	args := []string{
		"--model", s.Path,
		// --alias makes the backend answer to the model's name (C7).
		"--alias", s.Name,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(s.Port),
		// v2 context is per slot; llama.cpp divides --ctx-size across slots.
		"--ctx-size", strconv.Itoa(s.Context * parallel),
		"--parallel", strconv.Itoa(parallel),
	}
	if len(s.GPUs) == 0 {
		args = append(args, "--n-gpu-layers", "0")
	} else {
		args = append(args, "--n-gpu-layers", "-1")
	}
	// --slots enables /slots, which Load reads. --log-verbosity 1 keeps
	// llama-server's errors (a model that cannot load says why, in the node's
	// log) and drops its info lines: the per-request ones the once-a-second
	// load poll would flood. --log-disable silenced the errors too.
	// An older build reads --log-verbosity as a threshold where 1 also turns
	// debug on, request and response bodies included: prompts would reach the
	// node's log. Such a build keeps --log-disable.
	if e.levelLogs == nil || e.levelLogs(o.Binary) {
		args = append(args, "--slots", "--log-verbosity", "1")
	} else {
		args = append(args, "--slots", "--log-disable")
	}

	if len(s.GPUs) >= 2 {
		mode := o.SplitMode
		if mode == "" {
			mode = SplitLayer
		}
		args = append(args, "--split-mode", mode, "--tensor-split", tensorSplit(o.SplitWeights, len(s.GPUs)))
		if mode == SplitRow {
			args = append(args, "--main-gpu", strconv.Itoa(o.MainGPU))
		}
	}

	if !hasAnyArg(s.Args, "--threads", "-t") {
		threads := o.Threads
		if threads == 0 {
			threads = autoThreads(e.nproc(), max(s.Backends, 1))
		}
		args = append(args, "--threads", strconv.Itoa(threads))
	}

	if !hasAnyArg(s.Args, "--mmap", "--no-mmap") {
		if size, err := e.modelSize(s.Path); err == nil && needsNoMmap(size, e.totalRAM()) {
			args = append(args, "--no-mmap")
		}
	}

	args = append(args, s.Args...)
	return engine.Command{
		Path: o.Binary,
		Args: args,
		Env:  slices.Clone(mallocEnv),
	}, nil
}

// tensorSplit is the configured weights with the shortest exact decimal (1.0
// prints as 1), or an even 1,1,... split across n GPUs.
func tensorSplit(weights []float64, n int) string {
	parts := make([]string, 0, max(len(weights), n))
	if len(weights) > 0 {
		for _, w := range weights {
			parts = append(parts, strconv.FormatFloat(w, 'f', -1, 64))
		}
	} else {
		for range n {
			parts = append(parts, "1")
		}
	}
	return strings.Join(parts, ",")
}

func hasAnyArg(args []string, flags ...string) bool {
	for _, a := range args {
		if slices.Contains(flags, a) {
			return true
		}
	}
	return false
}

var levelLogsCache sync.Map // binary path -> bool

// cachedLevelLogs asks a binary's --help once per process.
func cachedLevelLogs(binary string) bool {
	if v, ok := levelLogsCache.Load(binary); ok {
		return v.(bool)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--help")
	cmd.WaitDelay = time.Second
	out, _ := cmd.CombinedOutput()
	ok := levelList(string(out))
	levelLogsCache.Store(binary, ok)
	return ok
}

// levelList reports whether a llama-server --help lists --log-verbosity's
// levels, as builds do since verbosity became levels ("1: error").
func levelList(help string) bool {
	return strings.Contains(help, "- 1: error")
}
