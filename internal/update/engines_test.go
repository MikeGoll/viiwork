package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/janit/viiwork/v2/internal/config"
	_ "github.com/janit/viiwork/v2/internal/engine/llamacpp"
)

func modelsWithLlama(t *testing.T, versionLine string) []config.Model {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "llama-server")
	os.WriteFile(bin, []byte("#!/bin/sh\necho '"+versionLine+"' >&2\n"), 0o755)
	c, err := config.Parse([]byte("node:\n  name: n\n  state_dir: /tmp/s\nmesh:\n  open: true\nmodels:\n" +
		"  - name: m\n    engine: llamacpp\n    path: /models/m.gguf\n    gpus: [0]\n    context: 512\n    llamacpp:\n      binary: " + bin + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return c.Models
}

func TestCheckEngines(t *testing.T) {
	ctx := context.Background()
	models := modelsWithLlama(t, "version: 120 (abc)")
	if err := CheckEngines(ctx, models, map[string]string{"llamacpp": "b100"}); err != nil {
		t.Errorf("new enough: %v", err)
	}
	err := CheckEngines(ctx, models, map[string]string{"llamacpp": "b200"})
	if !errors.Is(err, ErrEngineTooOld) || !strings.Contains(err.Error(), "b200") || !strings.Contains(err.Error(), "b120") {
		t.Errorf("too old: %v", err)
	}
	if err := CheckEngines(ctx, models, map[string]string{"vllm": "0.11.0"}); err != nil {
		t.Errorf("an engine no model uses was checked: %v", err)
	}
	unreadable := modelsWithLlama(t, "nothing")
	if err := CheckEngines(ctx, unreadable, map[string]string{"llamacpp": "b100"}); !errors.Is(err, ErrEngineTooOld) {
		t.Errorf("unreadable version: %v", err)
	}
	if got := InstalledEngines(ctx, models); got["llamacpp"] != "b120" {
		t.Errorf("InstalledEngines = %v", got)
	}
	if got := InstalledEngines(ctx, unreadable); len(got) != 0 {
		t.Errorf("an unreadable version was reported: %v", got)
	}
}
