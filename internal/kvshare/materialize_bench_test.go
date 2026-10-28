package kvshare

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestMaterializeTiming times assembling a replica of about 1 GB, split into
// the copy and the digest. Run with KVSHARE_BIG=1 and a TMPDIR on a real disk.
func TestMaterializeTiming(t *testing.T) {
	if os.Getenv("KVSHARE_BIG") == "" {
		t.Skip("set KVSHARE_BIG=1")
	}
	b := fakeSession{model: 1, config: 2, cells: 120000, stateSeed: 1}.bytes()
	t.Logf("file %d MiB", len(b)>>20)
	st := NewStore(t.TempDir())
	p, err := Diff(bytes.NewReader(b), layoutOf(t, b), nil)
	if err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	_ = WriteDelta(&wire, bytes.NewReader(b), p)
	b = nil
	if _, err := st.Put(context.Background(), "k", &wire); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		out := filepath.Join(t.TempDir(), "x.bin")
		r, err := st.Materialize("k", out)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("run %d: total %.0f ms, digest %.0f ms, copy %.0f ms", i, r.Ms, r.DigestMs, r.Ms-r.DigestMs)
		os.Remove(out)
	}
}
