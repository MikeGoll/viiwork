package node

import (
	"slices"
	"strings"
	"testing"

	"github.com/janit/viiwork/v2/internal/config"
)

// An engine process starts without the node's secrets, under their default
// names and under the names the config gives them.
func TestEngineEnvironDropsTheNodesSecrets(t *testing.T) {
	for _, kv := range []string{"VIIWORK_MESH_SECRET=a", "MY_SECRET=b", "MY_PREV=c", "BMC_PASSWORD=d", "MY_BMC=e", "ENTSOE_API_KEY=f", "HF_TOKEN=keep", "VIIWORK_MESH_SECRET_NOTE=keep"} {
		name, value, _ := strings.Cut(kv, "=")
		t.Setenv(name, value)
	}
	names := func(cfg *config.Config) []string {
		var out []string
		for _, kv := range engineEnviron(cfg)() {
			name, _, _ := strings.Cut(kv, "=")
			out = append(out, name)
		}
		return out
	}

	got := names(&config.Config{})
	for _, gone := range []string{"VIIWORK_MESH_SECRET", "BMC_PASSWORD", "ENTSOE_API_KEY"} {
		if slices.Contains(got, gone) {
			t.Errorf("defaults: %s reached the engine", gone)
		}
	}
	for _, kept := range []string{"HF_TOKEN", "VIIWORK_MESH_SECRET_NOTE", "MY_SECRET"} {
		if !slices.Contains(got, kept) {
			t.Errorf("defaults: %s was dropped", kept)
		}
	}

	var cfg config.Config
	cfg.Mesh.SecretEnv, cfg.Mesh.SecretPrevEnv = "MY_SECRET", "MY_PREV"
	cfg.Power.Control.BMC.PasswordEnv = "MY_BMC"
	got = names(&cfg)
	for _, gone := range []string{"MY_SECRET", "MY_PREV", "MY_BMC", "ENTSOE_API_KEY"} {
		if slices.Contains(got, gone) {
			t.Errorf("named: %s reached the engine", gone)
		}
	}
	if !slices.Contains(got, "HF_TOKEN") {
		t.Error("named: HF_TOKEN was dropped")
	}
}
