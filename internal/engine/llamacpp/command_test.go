package llamacpp

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/engine"
	"github.com/janit/viiwork/v2/internal/gpu"
	"gopkg.in/yaml.v3"
)

const gib = int64(1) << 30

// parseModel builds models[0] through config.Parse, so per-model defaults
// apply exactly as they do in production.
func parseModel(t *testing.T, yaml string) config.Model {
	t.Helper()
	cfg, err := config.Parse([]byte("models:\n" + yaml))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Models) != 1 {
		t.Fatalf("want one model, got %d", len(cfg.Models))
	}
	return cfg.Models[0]
}

// testEngine fixes the host: CPU count, RAM and model size.
func testEngine(nproc int, ramBytes, modelBytes int64, sizeErr error) *Engine {
	return &Engine{
		nproc:    func() int { return nproc },
		totalRAM: func() int64 { return ramBytes },
		modelSize: func(string) (int64, error) {
			return modelBytes, sizeErr
		},
	}
}

func command(t *testing.T, e *Engine, m config.Model, backend int) engine.Command {
	t.Helper()
	spec := config.ModelSpec(m)
	spec.GPUs = m.BackendGPUs(backend)
	spec.Port = 40001
	spec.Vendor = gpu.VendorAMD
	cmd, err := e.Command(spec)
	if err != nil {
		t.Fatal(err)
	}
	return cmd
}

// flagValue returns the value after the first occurrence of flag, or "".
func flagValue(args []string, flag string) (string, bool) {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return "", i >= 0
	}
	return args[i+1], true
}

func TestNameAndTimeout(t *testing.T) {
	e := New()
	if e.Name() != Name || e.DefaultStartupTimeout() != 10*time.Minute {
		t.Errorf("Name=%q DefaultStartupTimeout=%v", e.Name(), e.DefaultStartupTimeout())
	}
}

func TestCommandSingleGPUExact(t *testing.T) {
	m := parseModel(t, `  - name: granite-4.2-8b
    engine: llamacpp
    path: /models/granite-4.2-8b-Q8_0.gguf
    gpus: [2]
    context: 16384
    parallel: 2
    args: ["--jinja"]
`)
	cmd := command(t, testEngine(16, 247*gib, 9*gib, nil), m, 0)
	if cmd.Path != "llama-server" {
		t.Errorf("Path = %q", cmd.Path)
	}
	want := strings.Fields("--model /models/granite-4.2-8b-Q8_0.gguf --alias granite-4.2-8b --host 127.0.0.1 --port 40001 --ctx-size 32768 --parallel 2 --n-gpu-layers -1 --slots --log-verbosity 1 --threads 16 --jinja")
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("Args =\n %v\nwant\n %v", cmd.Args, want)
	}
	wantEnv := []string{"MALLOC_MMAP_THRESHOLD_=65536", "MALLOC_TRIM_THRESHOLD_=65536", "MALLOC_ARENA_MAX=4"}
	if !slices.Equal(cmd.Env, wantEnv) {
		t.Errorf("Env = %v, want %v", cmd.Env, wantEnv)
	}
}

func TestCommandSplitPairWithWeights(t *testing.T) {
	m := parseModel(t, `  - name: Qwen3.8-27B
    engine: llamacpp
    path: /models/Qwen3.8-27B.gguf
    gpus: [0, 1]
    gpus_per_backend: 2
    context: 49152
    parallel: 2
    llamacpp:
      split_weights: [1.16, 1.0]
`)
	cmd := command(t, testEngine(16, 247*gib, 18*gib, nil), m, 0)
	for flag, want := range map[string]string{"--split-mode": "layer", "--tensor-split": "1.16,1", "--ctx-size": "98304"} {
		if got, _ := flagValue(cmd.Args, flag); got != want {
			t.Errorf("%s = %q, want %q (args %v)", flag, got, want, cmd.Args)
		}
	}
	if _, ok := flagValue(cmd.Args, "--main-gpu"); ok {
		t.Errorf("layer mode must not pass --main-gpu: %v", cmd.Args)
	}
}

func TestCommandRowModeEvenSplit(t *testing.T) {
	m := parseModel(t, `  - name: m
    engine: llamacpp
    path: /models/m.gguf
    gpus: [4, 5]
    gpus_per_backend: 2
    context: 4096
    llamacpp:
      split_mode: row
      main_gpu: 1
`)
	cmd := command(t, testEngine(16, 247*gib, gib, nil), m, 0)
	for flag, want := range map[string]string{"--split-mode": "row", "--tensor-split": "1,1", "--main-gpu": "1"} {
		if got, _ := flagValue(cmd.Args, flag); got != want {
			t.Errorf("%s = %q, want %q (args %v)", flag, got, want, cmd.Args)
		}
	}
}

func TestCommandCPUModel(t *testing.T) {
	m := parseModel(t, "  - name: tiny\n    engine: llamacpp\n    path: /models/tiny.gguf\n    context: 2048\n")
	cmd := command(t, testEngine(4, 16*gib, gib, nil), m, 0)
	if got, _ := flagValue(cmd.Args, "--n-gpu-layers"); got != "0" {
		t.Errorf("--n-gpu-layers = %q, want 0", got)
	}
	for _, flag := range []string{"--split-mode", "--tensor-split", "--main-gpu"} {
		if slices.Contains(cmd.Args, flag) {
			t.Errorf("a CPU backend must not pass %s: %v", flag, cmd.Args)
		}
	}
}

func TestCommandThreads(t *testing.T) {
	const base = "  - name: m\n    engine: llamacpp\n    path: /models/m.gguf\n    context: 4096\n"
	cases := []struct {
		name  string
		yaml  string
		nproc int
		want  string // "" = no generated --threads
	}{
		{"explicit llamacpp.threads", base + "    gpus: [0]\n    llamacpp:\n      threads: 4\n", 16, "4"},
		{"operator -t in args", base + "    gpus: [0]\n    args: [\"-t\", \"8\"]\n", 16, ""},
		{"3 backends on 16 CPUs", base + "    gpus: [0, 1, 2]\n", 16, "5"},
		{"3 backends on 2 CPUs", base + "    gpus: [0, 1, 2]\n", 2, "1"},
	}
	for _, tc := range cases {
		m := parseModel(t, tc.yaml)
		cmd := command(t, testEngine(tc.nproc, 64*gib, gib, nil), m, 0)
		n := 0
		for _, a := range cmd.Args {
			if a == "--threads" {
				n++
			}
		}
		got, _ := flagValue(cmd.Args, "--threads")
		switch {
		case tc.want == "" && n != 0:
			t.Errorf("%s: generated --threads %q, want none (args %v)", tc.name, got, cmd.Args)
		case tc.want != "" && (n != 1 || got != tc.want):
			t.Errorf("%s: --threads %q (%d occurrences), want %q", tc.name, got, n, tc.want)
		}
	}
}

func TestCommandNoMmap(t *testing.T) {
	const base = "  - name: m\n    engine: llamacpp\n    path: /models/m.gguf\n    gpus: [0]\n    context: 4096\n"
	cases := []struct {
		name    string
		args    string
		ram     int64
		model   int64
		sizeErr error
		want    bool
	}{
		{"100 GiB on 46 GiB", "", 46 * gib, 100 * gib, nil, true},
		{"exactly 80% of RAM", "", 100 * gib, 80 * gib, nil, true},
		{"79% of RAM", "", 100 * gib, 79 * gib, nil, false},
		{"operator --mmap", "    args: [\"--mmap\"]\n", 46 * gib, 100 * gib, nil, false},
		{"operator --load-mode", "    args: [\"--load-mode\", \"mmap\"]\n", 46 * gib, 100 * gib, nil, false},
		{"RAM unknown", "", 0, 100 * gib, nil, false},
		{"size lookup fails", "", 46 * gib, 0, errors.New("stat: permission denied"), false},
	}
	for _, tc := range cases {
		m := parseModel(t, base+tc.args)
		cmd := command(t, testEngine(16, tc.ram, tc.model, tc.sizeErr), m, 0)
		mode, _ := flagValue(cmd.Args, "--load-mode")
		if got := mode == "none"; got != tc.want || slices.Contains(cmd.Args, "--no-mmap") {
			t.Errorf("%s: --load-mode none present = %v, want %v (args %v)", tc.name, got, tc.want, cmd.Args)
		}
	}

	// A build older than --load-mode gets the spelling it reads.
	e := testEngine(16, 46*gib, 100*gib, nil)
	e.help = func(string) helpFacts { return helpFacts{levelLogs: true, noMmap: true} }
	cmd := command(t, e, parseModel(t, base), 0)
	if !slices.Contains(cmd.Args, "--no-mmap") || slices.Contains(cmd.Args, "--load-mode") {
		t.Errorf("old build: want --no-mmap only (args %v)", cmd.Args)
	}
}

func TestLoadModeFlag(t *testing.T) {
	if !loadModeFlag("-lm,   --load-mode MODE                 model loading mode (default: auto)") {
		t.Error("--load-mode not recognised in a current --help")
	}
	if loadModeFlag("--mmap, --no-mmap                       whether to memory-map model") {
		t.Error("--load-mode invented for a build without it")
	}
}

func TestCommandOperatorArgsComeLast(t *testing.T) {
	m := parseModel(t, "  - name: m\n    engine: llamacpp\n    path: /models/m.gguf\n    gpus: [0]\n    context: 4096\n    args: [\"--n-gpu-layers\", \"20\"]\n")
	cmd := command(t, testEngine(16, 64*gib, gib, nil), m, 0)
	var at []int
	for i, a := range cmd.Args {
		if a == "--n-gpu-layers" {
			at = append(at, i)
		}
	}
	if len(at) != 2 || cmd.Args[at[0]+1] != "-1" || cmd.Args[at[1]+1] != "20" {
		t.Errorf("the operator's --n-gpu-layers must follow the generated one (llama.cpp takes the last): %v", cmd.Args)
	}
}

// A model with no llamacpp: block at all is the ordinary case now that
// defaults live in the engine, so it must work rather than fail.
func TestCommandWithoutOptionsBlock(t *testing.T) {
	m := parseModel(t, "  - name: bare\n    engine: llamacpp\n    path: /m.gguf\n    gpus: [0]\n    context: 4096\n")
	cmd := command(t, testEngine(4, gib, gib, nil), m, 0)
	if cmd.Path != "llama-server" {
		t.Errorf("binary = %q, want the engine's default", cmd.Path)
	}
	if v, _ := flagValue(cmd.Args, "--alias"); v != "bare" {
		t.Errorf("--alias = %q, want the model name", v)
	}
}

// An unusable block is an error from Command, not a panic, and it names the
// model so the log says which backend failed to start.
func TestCommandRejectsUnknownOptionKey(t *testing.T) {
	spec := engine.Spec{Name: "handmade", Path: "/m.gguf", GPUs: []int{0}, Port: 1, Context: 1, Parallel: 1, Backends: 1}
	if err := yaml.Unmarshal([]byte("split_mod: layer\n"), &spec.Options); err != nil {
		t.Fatal(err)
	}
	spec.Options = *spec.Options.Content[0]
	_, err := testEngine(4, gib, gib, nil).Command(spec)
	if err == nil || !strings.Contains(err.Error(), "handmade") || !strings.Contains(err.Error(), "split_mod") {
		t.Errorf("err = %v, want an error naming the model and the key", err)
	}
}

func TestModelTotalSize(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, size int) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	part1 := write("model-00001-of-00003.gguf", 10)
	write("model-00002-of-00003.gguf", 20)
	write("model-00003-of-00003.gguf", 30)
	if n, err := modelTotalSize(part1); err != nil || n != 60 {
		t.Errorf("multi-part = %d, %v; want 60", n, err)
	}
	if n, err := modelTotalSize(write("single.gguf", 7)); err != nil || n != 7 {
		t.Errorf("single file = %d, %v; want 7", n, err)
	}
	if _, err := modelTotalSize(filepath.Join(dir, "missing.gguf")); err == nil {
		t.Error("a missing file must be an error")
	}
}

// A viiwork-parrot folder model resolves to a directory. llama.cpp loads a
// GGUF file, so a directory is a configuration mistake no respawn fixes: an
// error from Command marks the model dead at once instead of respawning a
// llama-server that cannot load.
func TestCommandRejectsADirectory(t *testing.T) {
	dir := t.TempDir()
	_, err := testEngine(8, 64*gib, 0, nil).Command(engine.Spec{Name: "folder", Path: dir, Port: 41000, Context: 512, Parallel: 1, Backends: 1})
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("err = %v, want a directory refusal", err)
	}
}

func TestCommandAcceptsAFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "m.gguf")
	if err := os.WriteFile(f, []byte("gguf"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := testEngine(8, 64*gib, 0, nil).Command(engine.Spec{Name: "file", Path: f, Port: 41000, Context: 512, Parallel: 1, Backends: 1}); err != nil {
		t.Fatalf("a file must be accepted: %v", err)
	}
}

// llama-server's own errors must reach the node's log: with --log-disable a
// backend that cannot load (found on teddy: a chat template llama.cpp could
// not parse) died as "dead" with no reason anywhere. Verbosity 1 keeps errors
// and drops the per-request info lines the load poll would otherwise flood.
func TestBackendErrorsAreLogged(t *testing.T) {
	m := parseModel(t, `  - name: m
    engine: llamacpp
    path: /models/m.gguf
    gpus: [0]
    context: 8192
    parallel: 1
`)
	args := command(t, testEngine(16, 247*gib, 9*gib, nil), m, 0).Args
	if slices.Contains(args, "--log-disable") {
		t.Errorf("--log-disable silences the backend's errors: %v", args)
	}
	i := slices.Index(args, "--log-verbosity")
	if i < 0 || i+1 >= len(args) || args[i+1] != "1" {
		t.Errorf("want --log-verbosity 1 (errors only): %v", args)
	}
}

// Older llama-server builds read --log-verbosity as a threshold where 1 also
// enables debug: every request and response body, prompts included, and the
// once-a-second /slots poll would go to the node's log. Only a build whose
// --help lists the levels (1 = error) gets it; any other keeps --log-disable.
func TestOlderBuildsKeepLogsDisabled(t *testing.T) {
	m := parseModel(t, `  - name: m
    engine: llamacpp
    path: /models/m.gguf
    gpus: [0]
    context: 8192
    parallel: 1
    llamacpp:
      binary: /opt/old/llama-server
`)
	e := testEngine(16, 247*gib, 9*gib, nil)
	var asked []string
	e.help = func(binary string) helpFacts {
		asked = append(asked, binary)
		return helpFacts{noMmap: true}
	}
	args := command(t, e, m, 0).Args
	if !slices.Contains(args, "--log-disable") || slices.Contains(args, "--log-verbosity") {
		t.Errorf("an older build: %v", args)
	}
	if len(asked) != 1 || asked[0] != "/opt/old/llama-server" {
		t.Errorf("detection asked about %q", asked)
	}
}

func TestLevelListDetection(t *testing.T) {
	newHelp := "-lv,   --verbosity, --log-verbosity N   Set the verbosity threshold.\n  - 0: generic output\n  - 1: error\n  - 2: warning\n"
	oldHelp := "-lv,   --verbosity, --log-verbosity N   set the verbosity threshold. messages with a higher verbosity will be ignored.\n"
	if !levelList(newHelp) || levelList(oldHelp) || levelList("") {
		t.Error("levelList misreads a --help")
	}
}

// helpBinary is a stand-in llama-server whose --help prints text.
func helpBinary(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "llama-server")
	if err := os.WriteFile(p, []byte("#!/bin/sh\ncat <<'EOF'\n"+text+"\nEOF\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHelpFactsFromTheBinary(t *testing.T) {
	for _, tc := range []struct {
		name, help string
		want       helpFacts
	}{
		{"b10437: both spellings", "--no-mmap\n--load-mode MODE\n--log-verbosity N\n  - 1: error", helpFacts{levelLogs: true, loadMode: true, noMmap: true}},
		{"b11371: the new one only", "--load-mode MODE\n  - 1: error", helpFacts{levelLogs: true, loadMode: true}},
		{"an old build", "--no-mmap\n--log-disable", helpFacts{noMmap: true}},
	} {
		if got := cachedHelp(helpBinary(t, tc.help)); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// A --help that could not be read says nothing about the binary. It must not
// be remembered, and what is assumed meanwhile is the pinned build's spelling:
// b11371 exits on --no-mmap, so "unknown" read as "old" kills every large
// model on every respawn until the node restarts.
func TestUnreadableHelpIsNotCachedAndAssumesThePinnedBuild(t *testing.T) {
	defer func(d time.Duration) { helpRetry = d }(helpRetry)
	helpRetry = 0 // ask again at once: this test repairs the binary between calls
	p := filepath.Join(t.TempDir(), "llama-server")
	if got := cachedHelp(p); !got.loadMode {
		t.Errorf("a missing binary read as an old build: %+v", got)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 127\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := cachedHelp(p); !got.loadMode {
		t.Errorf("a binary that printed nothing read as an old build: %+v", got)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho ' --no-mmap'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := cachedHelp(p); got.loadMode {
		t.Errorf("the failure was remembered: a binary that now lists only --no-mmap read as %+v", got)
	}
}

func TestHelpOutputIsBounded(t *testing.T) {
	p := filepath.Join(t.TempDir(), "llama-server")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho ' --no-mmap'\nhead -c 8000000 /dev/zero\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := helpOutput(p)
	if len(out) > helpLimit {
		t.Errorf("kept %d bytes of --help, limit %d", len(out), helpLimit)
	}
}

// An operator's own --no-mmap was valid on b10437 and makes b11371 exit at
// argument parsing: the image moves under a config that did not change, and
// the only clue is llama-server's stderr. The node already knows what the
// binary reads, so it passes the spelling the binary takes.
func TestOperatorNoMmapFollowsTheBinary(t *testing.T) {
	m := parseModel(t, `  - name: m
    engine: llamacpp
    path: /models/m.gguf
    gpus: [0]
    context: 4096
    parallel: 1
    args: ["--no-mmap", "--mlock"]
`)
	for _, tc := range []struct {
		name  string
		facts helpFacts
		want  []string // the tail of the command line
	}{
		{"b11371 has no --no-mmap", helpFacts{levelLogs: true, loadMode: true}, []string{"--load-mode", "none", "--mlock"}},
		{"b10437 reads both", helpFacts{levelLogs: true, loadMode: true, noMmap: true}, []string{"--no-mmap", "--mlock"}},
		{"an old build", helpFacts{noMmap: true}, []string{"--no-mmap", "--mlock"}},
	} {
		e := testEngine(16, 247*gib, 9*gib, nil)
		e.help = func(string) helpFacts { return tc.facts }
		args := command(t, e, m, 0).Args
		if n := len(args) - len(tc.want); n < 0 || !slices.Equal(args[n:], tc.want) {
			t.Errorf("%s: args end %v, want %v", tc.name, args, tc.want)
		}
	}
}

// Respawns of one model can ask about one binary at the same moment. One
// --help answers them all.
func TestConcurrentHelpRunsTheBinaryOnce(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "llama-server")
	runs := filepath.Join(dir, "runs")
	script := "#!/bin/sh\necho run >> " + runs + "\nsleep 0.2\necho ' --load-mode MODE'\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !cachedHelp(p).loadMode {
				t.Error("a caller got the answer of a failed probe")
			}
		}()
	}
	wg.Wait()
	b, _ := os.ReadFile(runs)
	if n := strings.Count(string(b), "run"); n != 1 {
		t.Errorf("--help ran %d times for 8 concurrent callers", n)
	}
}

// Not knowing what a binary reads is no evidence that it stopped reading
// --no-mmap: the operator's flag is translated only when --help showed it gone.
func TestUnreadableHelpLeavesTheOperatorsFlagAlone(t *testing.T) {
	if f := cachedHelp(filepath.Join(t.TempDir(), "absent")); !f.loadMode || !f.noMmap {
		t.Errorf("an unreadable --help must read as: generate --load-mode, leave --no-mmap alone; got %+v", f)
	}
}

// A binary whose --help fails is not asked again on every launch: N backends
// of it would each wait out the probe in turn.
func TestFailedHelpIsNotRetriedAtOnce(t *testing.T) {
	dir := t.TempDir()
	p, runs := filepath.Join(dir, "llama-server"), filepath.Join(dir, "runs")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho run >> "+runs+"\nexit 127\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cachedHelp(p)
	cachedHelp(p)
	b, _ := os.ReadFile(runs)
	if n := strings.Count(string(b), "run"); n != 1 {
		t.Errorf("a failing --help ran %d times for two calls in a row", n)
	}
}
