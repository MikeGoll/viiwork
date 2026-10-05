package llamacpp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/engine"
	"gopkg.in/yaml.v3"
)

// fakeBinary writes a shell script that prints out on both streams.
func fakeBinary(t *testing.T, out string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "llama-server")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nprintf '%s\\n' \""+out+"\" >&2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func specWith(t *testing.T, bin string) engine.Spec {
	t.Helper()
	var s engine.Spec
	if err := yaml.Unmarshal([]byte("binary: "+bin+"\n"), &s.Options); err != nil {
		t.Fatal(err)
	}
	s.Options = *s.Options.Content[0]
	return s
}

func TestVersion(t *testing.T) {
	e := New()
	var _ engine.Versioner = e
	got, err := e.Version(context.Background(), specWith(t, fakeBinary(t, "load_backend: loaded CPU backend\nversion: 10437 (61c9f0e)\nbuilt with cc")))
	if err != nil || got != "b10437" {
		t.Fatalf("Version = %q, %v", got, err)
	}
	// The current spelling (seen on ghcr.io/ggml-org/llama.cpp:server-*-b10438).
	got, err = e.Version(context.Background(), specWith(t, fakeBinary(t, "version: 0.1.0-dev (build 10438, commit 9d57ce456)\nbuilt with cc")))
	if err != nil || got != "b10438" {
		t.Fatalf("Version (build N spelling) = %q, %v", got, err)
	}
	if _, err := e.Version(context.Background(), specWith(t, fakeBinary(t, "nothing useful"))); err == nil {
		t.Error("a version was invented")
	}
	for _, c := range []struct {
		installed, min string
		want           bool
	}{{"b10437", "b10437", true}, {"b10437", "b9999", true}, {"b9999", "b10437", false}} {
		if ok, err := e.AtLeast(c.installed, c.min); err != nil || ok != c.want {
			t.Errorf("AtLeast(%s, %s) = %v, %v", c.installed, c.min, ok, err)
		}
	}
	if _, err := e.AtLeast("10437", "b1"); err == nil {
		t.Error("a malformed build number was compared")
	}
}

// A wrapper script (a --no-mmap shim, say) whose child keeps the output pipe
// open after the wrapper exits must not hang the check: the version it
// already printed is enough.
func TestVersionDoesNotWaitForAWrappersChildren(t *testing.T) {
	p := filepath.Join(t.TempDir(), "llama-server")
	os.WriteFile(p, []byte("#!/bin/sh\necho 'version: 5 (x)' >&2\nsleep 30 &\n"), 0o755)
	start := time.Now()
	got, err := New().Version(context.Background(), specWith(t, p))
	if got != "b5" || err != nil {
		t.Errorf("Version = %q, %v", got, err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("waited %v for a child holding the pipe", d)
	}
}
