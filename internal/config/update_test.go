package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const updateBase = "node:\n  name: n\n  state_dir: /tmp/viiwork-state\nmesh:\n  open: true\n"

func parseUpdate(t *testing.T, extra string) (*Config, error) {
	t.Helper()
	c, err := Parse([]byte(updateBase + extra))
	if err != nil {
		return nil, err
	}
	return c, c.Validate(func(string) (string, bool) { return "", false })
}

func TestUpdateDefaults(t *testing.T) {
	c, err := parseUpdate(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Update.Enabled || c.Update.Source != DefaultUpdateSource || c.Update.ConfirmTimeout.Duration != 0 {
		t.Fatalf("defaults = %+v", c.Update)
	}
}

func TestUpdateValidation(t *testing.T) {
	ok := []string{
		"update:\n  enabled: true\n",
		"update:\n  source: https://mirror.example/releases\n",
		"update:\n  source: http://127.0.0.1:8123/r\n", // a test server on this machine
		"update:\n  confirm_timeout: 45m\n",
	}
	for _, in := range ok {
		if _, err := parseUpdate(t, in); err != nil {
			t.Errorf("%q: %v", in, err)
		}
	}
	bad := map[string]string{
		"update:\n  source: http://mirror.example/r\n": "update.source",
		"update:\n  source: ftp://mirror.example/r\n":  "update.source",
		"update:\n  source: https://m.example/r?x=1\n": "update.source",
		"update:\n  source: \"\"\n":                    "update.source",
		"update:\n  confirm_timeout: -1s\n":            "update.confirm_timeout",
		"update:\n  enable: true\n":                    "enable", // strict keys
	}
	for in, field := range bad {
		if _, err := parseUpdate(t, in); err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%q: error %v, want one naming %s", in, err, field)
		}
	}
}

func TestConfirmWindow(t *testing.T) {
	c := Defaults()
	c.Models = []Model{
		{Name: "big", Engine: "llamacpp", StartupTimeout: Duration{45 * time.Minute}},
		{Name: "small", Engine: "llamacpp"}, // the test engine's default, 1m (fakeengine_test.go)
	}
	if got := c.ConfirmWindow(); got != 45*time.Minute+time.Minute+10*time.Minute {
		t.Errorf("derived window = %v, want 56m", got)
	}
	c.Update.ConfirmTimeout = Duration{5 * time.Minute}
	if got := c.ConfirmWindow(); got != 5*time.Minute {
		t.Errorf("explicit window = %v", got)
	}
	empty := Defaults()
	if got := empty.ConfirmWindow(); got != 10*time.Minute {
		t.Errorf("no models = %v, want 10m", got)
	}
}

// The engine helper reads update.source from the host's config as root: the
// default when absent, and only what Validate would accept.
func TestPeekSource(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "viiwork.yaml")
	for body, want := range map[string]string{
		"node: {name: n}\n": DefaultUpdateSource,
		"update: {source: 'https://example.org/r'}\nfuture: 1\n": "https://example.org/r",
		"update: {source: 'http://evil.example/r'}\n":            "",
		"update: {source: 'file:///etc'}\n":                      "",
	} {
		os.WriteFile(p, []byte(body), 0o644)
		got, err := PeekSource(p)
		if want == "" {
			if err == nil {
				t.Errorf("%q: accepted %q", body, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("%q: %q, %v", body, got, err)
		}
	}
}
