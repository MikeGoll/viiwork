package kvshare

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestEngineVectors applies the engine's own STRSDLT test vectors
// (janit/Strata kv-export): KVSHARE_VECTORS names a directory holding
// <name>-base.strsess, <name>-delta.strsdlt and <name>-full.strsess. The base
// goes in as a full put, the delta is applied, and the assembled file must
// equal the engine's full save of the same state byte for byte.
func TestEngineVectors(t *testing.T) {
	dir := os.Getenv("KVSHARE_VECTORS")
	if dir == "" {
		t.Skip("set KVSHARE_VECTORS to the vectors directory")
	}
	for _, name := range []string{"small", "real50k"} {
		t.Run(name, func(t *testing.T) {
			base := filepath.Join(dir, name+"-base.strsess")
			delta := filepath.Join(dir, name+"-delta.strsdlt")
			full := filepath.Join(dir, name+"-full.strsess")
			df, err := os.Open(delta)
			if err != nil {
				t.Skip(err)
			}
			defer df.Close()
			h, err := ReadEngineDeltaHeader(df)
			if err != nil {
				t.Fatal(err)
			}
			st := NewStore(t.TempDir())
			ctx := context.Background()
			if _, err := PushToStore(ctx, st, "k", base, SaveIDString(h.Base)); err != nil {
				t.Fatal(err)
			}
			if _, err := df.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			res, err := st.PutEngineDelta(ctx, "k", df, FileMeta{Size: int64(h.FileSize), SaveID: SaveIDString(h.New), Format: FormatEngineDelta})
			if err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(t.TempDir(), "rebuilt.strsess")
			m, err := st.Materialize("k", out)
			if err != nil {
				t.Fatal(err)
			}
			if !sameFiles(t, out, full) {
				t.Fatal("the rebuilt file differs from the engine's full save")
			}
			var tr [8]byte
			f, _ := os.Open(out)
			_, _ = f.ReadAt(tr[:], int64(h.FileSize)-sessTrailer)
			f.Close()
			t.Logf("%s: %d MiB rebuilt equal to the engine's full save (trailer %016x): apply %.0f ms, assemble %.0f ms (check and seal %.0f ms)",
				name, h.FileSize>>20, binary.LittleEndian.Uint64(tr[:]), res.ApplyMs, m.Ms, m.DigestMs)
		})
	}
}

// PushToStore puts the session file at path into st under key as a full
// save with saveID, the way a sender's diff would.
func PushToStore(ctx context.Context, st *Store, key, path, saveID string) (PutResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return PutResult{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return PutResult{}, err
	}
	l, err := ParseLayout(f, info.Size())
	if err != nil {
		return PutResult{}, err
	}
	m, _ := st.Manifest(ctx, key)
	p, err := Diff(f, l, m)
	if err != nil {
		return PutResult{}, err
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(WriteDelta(pw, f, p)) }()
	return st.PutExpect(ctx, key, pr, FileMeta{Size: p.Size, Digest: p.Digest, SaveID: saveID})
}

func sameFiles(t *testing.T, a, b string) bool {
	t.Helper()
	fa, err := os.Open(a)
	if err != nil {
		t.Fatal(err)
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		t.Fatal(err)
	}
	defer fb.Close()
	ba, bb := make([]byte, 4<<20), make([]byte, 4<<20)
	for {
		na, ea := io.ReadFull(fa, ba)
		nb, eb := io.ReadFull(fb, bb)
		if na != nb || !bytes.Equal(ba[:na], bb[:nb]) {
			return false
		}
		if ea != nil || eb != nil {
			return (ea == io.EOF || ea == io.ErrUnexpectedEOF) && (eb == io.EOF || eb == io.ErrUnexpectedEOF)
		}
	}
}
