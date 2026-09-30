package node

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/janit/viiwork/v2/internal/config"
)

func TestPerfKeysChangeWithWhatMakesSpeed(t *testing.T) {
	base := config.Model{Name: "m", Engine: "llamacpp", Path: "/m.gguf", GPUs: []int{0, 1}, Args: []string{"-fa"}}
	k := perfKeys([]config.Model{base})["m"]
	same := base
	same.Env = map[string]string{"X": "y"} // env does not make speed
	if perfKeys([]config.Model{same})["m"] != k {
		t.Error("an env change must keep the baseline")
	}
	for name, mut := range map[string]func(*config.Model){
		"engine": func(m *config.Model) { m.Engine = "vllm" },
		"path":   func(m *config.Model) { m.Path = "/other.gguf" },
		"gpus":   func(m *config.Model) { m.GPUs = []int{2, 3} },
		"args":   func(m *config.Model) { m.Args = nil },
	} {
		m := base
		m.GPUs = append([]int(nil), base.GPUs...)
		mut(&m)
		if perfKeys([]config.Model{m})["m"] == k {
			t.Errorf("a %s change must drop the baseline", name)
		}
	}
}

// A corrupt perf.json costs minutes of learning, never a start.
func TestCorruptPerfFileDoesNotStopTheNode(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(state, "perf.json"), "garbage{")
	path := filepath.Join(dir, "viiwork.yaml")
	writeFile(t, path, nodeConfig("n1", state, []fakeModel{{name: "m"}}, ""))
	cfg, err := config.Load(path, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	n, err := New(cfg, Options{ConfigPath: path, Version: "v2-test", Log: logs, LookupEnv: noEnv})
	if err != nil {
		t.Fatalf("New must not fail on a corrupt perf.json: %v", err)
	}
	if n.perf == nil {
		t.Fatal("the node must still have a perf tracker")
	}
	if !strings.Contains(logs.String(), "perf.json") {
		t.Errorf("the corrupt file must be logged; log = %q", logs.String())
	}
}
