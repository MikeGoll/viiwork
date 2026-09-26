package energy

import (
	"os"
	"path/filepath"
	"testing"
)

// A crash part way through an append leaves a last line without its newline.
// Its index was never handed out, so it is dropped — and cut off, so the next
// append does not glue onto it and shift every later index.
func TestModelTableDropsTornLastLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.txt")
	if err := os.WriteFile(path, []byte("\nqwen\ngem"), 0o644); err != nil {
		t.Fatal(err)
	}
	tbl, err := openModelTable(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(tbl.names); got != 2 || tbl.Name(1) != "qwen" || tbl.Name(2) != "" {
		t.Fatalf("names = %q, want the two complete lines", tbl.names)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "\nqwen\n" {
		t.Fatalf("file = %q, want the fragment cut off", raw)
	}

	idx, err := tbl.Index("gemma")
	if err != nil || idx != 2 {
		t.Fatalf("Index(gemma) = %d, %v; want 2", idx, err)
	}
	reopened, err := openModelTable(path)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Name(2) != "gemma" || reopened.Name(1) != "qwen" {
		t.Errorf("after reopen names = %q", reopened.names)
	}
}

// A failed append must leave the in-memory table as it was, or the next
// successful append is read back at a different index than it was given.
func TestModelTableFailedAppendChangesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	tbl, err := openModelTable(filepath.Join(dir, "models.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tbl.Index("qwen"); err == nil {
		t.Fatal("append into a missing directory should fail")
	}
	if len(tbl.names) != 0 || len(tbl.index) != 0 {
		t.Fatalf("failed append left names %q index %v", tbl.names, tbl.index)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	a, err := tbl.Index("gemma")
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := openModelTable(filepath.Join(dir, "models.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Name(a) != "gemma" {
		t.Errorf("index %d reads back as %q, want gemma", a, reopened.Name(a))
	}
	if idx, _ := reopened.Index("qwen"); reopened.Name(idx) != "qwen" || idx == a {
		t.Errorf("qwen got index %d after the failure", idx)
	}
}
