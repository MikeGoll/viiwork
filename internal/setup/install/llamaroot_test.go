package install

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestManagedLlama(t *testing.T) {
	cases := []struct {
		binary, root, tag string
		ok                bool
	}{
		{"/u/.local/share/viiwork/llama.cpp/b10437/llama-b10437/llama-server", "/u/.local/share/viiwork/llama.cpp", "b10437", true},
		{"/r/b1/llama-b2/llama-server", "", "", false},             // the tag disagrees with itself
		{"/r/b1/llama-b1/llama-cli", "", "", false},                // not llama-server
		{"/r/b1/llama-b1/../llama-b1/llama-server", "", "", false}, // not clean
		{"r/b1/llama-b1/llama-server", "", "", false},              // relative
		{"/r/.x/llama-.x/llama-server", "", "", false},             // not a tag
		{"/opt/homebrew/bin/llama-server", "", "", false},
		{"llama-server", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		root, tag, ok := ManagedLlama(c.binary)
		if root != c.root || tag != c.tag || ok != c.ok {
			t.Errorf("%q: %q %q %v", c.binary, root, tag, ok)
		}
	}
}

func TestFollowPin(t *testing.T) {
	const root = "/r"
	present := func(have ...string) func(string, string) bool {
		return func(r, tag string) bool { return r == root && slices.Contains(have, tag) }
	}
	old := LlamaServerPath(root, "b1")
	cases := []struct {
		name, binary, root, pin string
		have                    []string
		want                    string
	}{
		{"follows the pin", old, root, "b2", []string{"b1", "b2"}, LlamaServerPath(root, "b2")},
		{"the pin's build is absent", old, root, "b2", []string{"b1"}, old},
		{"already the pin", old, root, "b1", []string{"b1"}, old},
		{"another root", LlamaServerPath("/elsewhere", "b1"), root, "b2", []string{"b2"}, LlamaServerPath("/elsewhere", "b1")},
		{"not the managed layout", "/opt/homebrew/bin/llama-server", root, "b2", []string{"b2"}, "/opt/homebrew/bin/llama-server"},
		{"a bare name", "llama-server", root, "b2", []string{"b2"}, "llama-server"},
		{"no install root", old, "", "b2", []string{"b2"}, old},
		{"an unstamped build", old, root, "", []string{"b2"}, old},
		{"a pin that is not a tag", old, root, "../x", []string{"../x"}, old},
	}
	for _, c := range cases {
		if got := FollowPin(c.binary, c.root, c.pin, present(c.have...)); got != c.want {
			t.Errorf("%s: %s", c.name, got)
		}
	}
}

func TestPruneLlamaRemovesOnlyUnkeptBuilds(t *testing.T) {
	root := t.TempDir()
	for _, tag := range []string{"b1", "b2", "b3"} {
		os.MkdirAll(filepath.Dir(LlamaServerPath(root, tag)), 0o755)
		os.WriteFile(LlamaServerPath(root, tag), []byte("#!server"), 0o755)
	}
	// Nothing that is not a build in the managed layout is ever touched.
	os.MkdirAll(filepath.Join(root, "notes"), 0o755)           // no llama-notes inside
	os.MkdirAll(filepath.Join(root, "b9", "something"), 0o755) // no llama-b9 inside
	os.MkdirAll(filepath.Join(root, ".fetch-123"), 0o755)      // a fetch in flight
	os.WriteFile(filepath.Join(root, "b8"), []byte("a file"), 0o644)
	os.Symlink(filepath.Join(root, "b1"), filepath.Join(root, "b7"))

	removed, err := PruneLlama(root, []string{"b1", "b3"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(removed, []string{"b2"}) {
		t.Errorf("removed %q", removed)
	}
	for _, p := range []string{"b1", "b3", "notes", "b9", ".fetch-123", "b8", "b7"} {
		if _, err := os.Lstat(filepath.Join(root, p)); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "b2")); err == nil {
		t.Error("b2 is still there")
	}
}

// With nothing to keep, something went wrong upstream: removing every build
// would leave the node with no engine at all.
func TestPruneLlamaKeepsEverythingWhenToldToKeepNothing(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Dir(LlamaServerPath(root, "b1")), 0o755)
	if _, err := PruneLlama(root, nil); err == nil {
		t.Error("pruned with an empty keep list")
	}
	if _, err := os.Stat(filepath.Join(root, "b1")); err != nil {
		t.Error(err)
	}
}

func TestPruneLlamaMissingRootIsNothingToDo(t *testing.T) {
	if removed, err := PruneLlama(filepath.Join(t.TempDir(), "absent"), []string{"b1"}); err != nil || len(removed) != 0 {
		t.Errorf("%q %v", removed, err)
	}
}

func TestLlamaRootFor(t *testing.T) {
	managed := LlamaServerPath("/Users/u/.local/share/viiwork/llama.cpp", "b1")
	cases := []struct {
		name     string
		man      Manifest
		binaries []string
		want     string
	}{
		{"recorded", Manifest{OS: "darwin", LlamaRoot: "/Users/u/llama"}, []string{managed}, "/Users/u/llama"},
		{"older manifest: from a managed binary", Manifest{OS: "darwin"}, []string{"llama-server", managed}, "/Users/u/.local/share/viiwork/llama.cpp"},
		{"older manifest, nothing managed", Manifest{OS: "darwin"}, []string{"/opt/homebrew/bin/llama-server"}, ""},
		{"a Linux install", Manifest{OS: "linux", LlamaRoot: "/x"}, []string{managed}, ""},
	}
	for _, c := range cases {
		if got := LlamaRootFor(c.man, c.binaries); got != c.want {
			t.Errorf("%s: %q", c.name, got)
		}
	}
}
