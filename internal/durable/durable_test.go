package durable

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestWriteFileModeKeepsSecretsPrivate(t *testing.T) {
	dir := t.TempDir()
	if err := WriteFileMode(dir, "mesh.env", []byte("S=1\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "mesh.env"))
	if err != nil || fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v, %v", fi.Mode(), err)
	}
}

func TestWriteFile(t *testing.T) {
	dir := t.TempDir()
	if err := WriteFile(dir, "a.json", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(dir, "a.json", []byte("two")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "a.json"))
	if string(b) != "two" {
		t.Fatalf("content = %q", b)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
	if err := WriteFile(filepath.Join(dir, "missing"), "x", []byte("x")); err == nil {
		t.Fatal("wrote into a directory that does not exist")
	}
}

// Concurrent writers never tear the file: each writes its own temporary file,
// so the result is exactly one writer's content, and nothing is left behind.
func TestConcurrentWritersNeverTear(t *testing.T) {
	dir := t.TempDir()
	valid := map[string]bool{}
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		body := fmt.Sprintf(`{"writer":%d,"pad":%q}`, i, strings.Repeat("x", 4096))
		valid[body] = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := WriteFile(dir, "state.json", []byte(body)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	b, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if !valid[string(b)] {
		t.Fatalf("torn file: %d bytes", len(b))
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("left behind: %v", entries)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "state.json")); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644", fi.Mode().Perm())
	}
}
