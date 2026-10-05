package install

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

// A Mac install keeps one llama.cpp build per tag under its llama root, in
// the layout the release tarball unpacks to: <root>/<tag>/llama-<tag>/. The
// node runs the build of the pin its own binary carries, so an update or a
// rollback moves the engine with the binary and records nothing. Everything
// here recognises that layout exactly and leaves any other path alone: a
// binary an operator pointed elsewhere is theirs.

// ValidTag reports whether tag can name a build directory: a plain name,
// never a path, a dotfile or empty. llama.cpp's tags are b<number>.
func ValidTag(tag string) bool {
	if tag == "" || !isAlnum(tag[0]) {
		return false
	}
	for i := 0; i < len(tag); i++ {
		if c := tag[i]; !isAlnum(c) && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// ManagedLlama splits a llama-server path of the managed layout into its
// root and tag. The path must be absolute, clean, and name the same tag
// twice; anything else is not a build the install manages.
func ManagedLlama(binary string) (root, tag string, ok bool) {
	if !filepath.IsAbs(binary) || filepath.Base(binary) != "llama-server" {
		return "", "", false
	}
	inner := filepath.Dir(binary)
	tagDir := filepath.Dir(inner)
	tag = filepath.Base(tagDir)
	root = filepath.Dir(tagDir)
	if !ValidTag(tag) || filepath.Base(inner) != "llama-"+tag || LlamaServerPath(root, tag) != binary {
		return "", "", false
	}
	return root, tag, true
}

// FollowPin is the llama-server a model configured with binary runs on a
// node whose binary carries pin. A binary that is exactly a managed build
// under root runs pin's build instead, when present says it is there;
// otherwise, and for every other binary, it is used as written.
func FollowPin(binary, root, pin string, present func(root, tag string) bool) string {
	r, tag, ok := ManagedLlama(binary)
	if !ok || root == "" || r != filepath.Clean(root) || !ValidTag(pin) || tag == pin || !present(r, pin) {
		return binary
	}
	return LlamaServerPath(r, pin)
}

// PruneLlama removes every build under root whose tag is not in keep, and
// returns the tags it removed. Only a real directory <tag>/llama-<tag> is a
// build: a file, a symlink, a fetch in flight or any directory of another
// shape is left alone. An empty keep list is refused, since it can only mean
// the caller failed to read what it runs.
func PruneLlama(root string, keep []string) ([]string, error) {
	if len(keep) == 0 {
		return nil, errors.New("no llama.cpp build to keep: refusing to remove them all")
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, e := range entries {
		tag := e.Name()
		if !e.IsDir() || !ValidTag(tag) || slices.Contains(keep, tag) {
			continue
		}
		fi, err := os.Lstat(filepath.Join(root, tag, "llama-"+tag))
		if err != nil || !fi.IsDir() {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, tag)); err != nil {
			return removed, err
		}
		removed = append(removed, tag)
	}
	return removed, nil
}

// LlamaRootFor is where a wizard install keeps its llama.cpp builds, or ""
// when it keeps none. A manifest from before v2.6.0 does not record it; the
// first configured binary in the managed layout then says where it is, which
// is where that wizard put it.
func LlamaRootFor(man Manifest, binaries []string) string {
	if man.OS != "darwin" {
		return ""
	}
	if man.LlamaRoot != "" {
		if !filepath.IsAbs(man.LlamaRoot) {
			return "" // not something this install wrote
		}
		return filepath.Clean(man.LlamaRoot)
	}
	for _, b := range binaries {
		if root, _, ok := ManagedLlama(b); ok {
			return root
		}
	}
	return ""
}
