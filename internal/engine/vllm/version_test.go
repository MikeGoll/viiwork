package vllm

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/janit/viiwork/v2/internal/engine"
	"gopkg.in/yaml.v3"
)

func TestVersion(t *testing.T) {
	p := filepath.Join(t.TempDir(), "vllm")
	os.WriteFile(p, []byte("#!/bin/sh\necho 'INFO some log line'\necho '0.11.2'\n"), 0o755)
	var s engine.Spec
	yaml.Unmarshal([]byte("binary: "+p+"\n"), &s.Options)
	s.Options = *s.Options.Content[0]
	e := New()
	var _ engine.Versioner = e
	if got, err := e.Version(context.Background(), s); err != nil || got != "0.11.2" {
		t.Fatalf("Version = %q, %v", got, err)
	}
	for _, c := range []struct {
		installed, min string
		want           bool
	}{{"0.11.2", "0.11.0", true}, {"0.10.9", "0.11.0", false}, {"1.0.0", "0.11.0", true}, {"0.11.0rc1", "0.11.0", true}} {
		if ok, err := e.AtLeast(c.installed, c.min); err != nil || ok != c.want {
			t.Errorf("AtLeast(%s, %s) = %v, %v", c.installed, c.min, ok, err)
		}
	}
}
