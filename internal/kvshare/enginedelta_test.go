package kvshare

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/janit/viiwork/v2/internal/kvshare/kvsharetest"
)

// sealed gives a test session a real STRSESS header and trailer, as Strata
// writes them: what a rebuilt replica must equal byte for byte.
func sealed(b []byte) []byte {
	out := bytes.Clone(b)
	payload := binary.LittleEndian.Uint64(out[32:])
	model, config := binary.LittleEndian.Uint64(out[16:]), binary.LittleEndian.Uint64(out[24:])
	copy(out, sessHeaderBytes(model, config, payload))
	binary.LittleEndian.PutUint64(out[len(out)-16:], SessionHash64(out[sessHeader:len(out)-sessTrailer], 0))
	copy(out[len(out)-8:], sessEnd)
	return out
}

// engineDelta writes the STRSDLT v1 delta of next against base, as draft 1
// describes the engine's: every K/V part keeps its whole chunks before the
// first chunk that differs from the base's.
func engineDelta(t *testing.T, base, next []byte, baseID, newID uint64) []byte {
	t.Helper()
	bl, nl := layoutOf(t, base), layoutOf(t, next)
	baseSec := map[string]Section{}
	for _, s := range bl.Sections {
		baseSec[s.Name] = s
	}
	var p bytes.Buffer
	u := func(v uint64) { p.Write(binary.LittleEndian.AppendUint64(nil, v)) }
	pos := int64(sessHeader)
	for _, s := range nl.Sections {
		if !isKVSection(s.Name) {
			p.Write(next[pos : s.Off+s.Len])
			pos = s.Off + s.Len
			continue
		}
		p.Write(next[pos : s.Off-8]) // the u64 count becomes (size, keep)
		data := next[s.Off : s.Off+s.Len]
		bs := baseSec[s.Name]
		bdata := base[bs.Off : bs.Off+bs.Len]
		var keep int64
		for keep+ChunkSize <= min(s.Len, bs.Len) && bytes.Equal(data[keep:keep+ChunkSize], bdata[keep:keep+ChunkSize]) {
			keep += ChunkSize
		}
		u(uint64(s.Len))
		u(uint64(keep))
		p.Write(data[keep:])
		n := (s.Len + ChunkSize - 1) / ChunkSize
		u(uint64(n))
		for i := range n {
			u(SessionHash64(data[i*ChunkSize:min(s.Len, (i+1)*ChunkSize)], 0))
		}
		pos = s.Off + s.Len
	}
	p.Write(next[pos : int64(len(next))-sessTrailer])
	h := make([]byte, engineDeltaHeader)
	copy(h, engineDeltaMagic)
	le := binary.LittleEndian
	le.PutUint32(h[8:], 1)
	le.PutUint32(h[12:], engineDeltaHeader)
	le.PutUint64(h[16:], nl.Model)
	le.PutUint64(h[24:], nl.Config)
	le.PutUint64(h[32:], uint64(p.Len()))
	le.PutUint64(h[40:], baseID)
	le.PutUint64(h[48:], newID)
	le.PutUint64(h[56:], uint64(len(next)))
	le.PutUint64(h[72:], SessionHash64(h[:72], 0))
	out := append(h, p.Bytes()...)
	out = le.AppendUint64(out, SessionHash64(p.Bytes(), 0))
	return append(out, engineDeltaEnd...)
}

// fullPut stores b as a full save with id.
func fullPut(t *testing.T, st *Store, key string, b []byte, id uint64) {
	t.Helper()
	m, _ := st.Manifest(context.Background(), key)
	p, err := Diff(bytes.NewReader(b), layoutOf(t, b), m)
	if err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	_ = WriteDelta(&wire, bytes.NewReader(b), p)
	if _, err := st.PutExpect(context.Background(), key, &wire, FileMeta{Size: p.Size, Digest: p.Digest, SaveID: SaveIDString(id)}); err != nil {
		t.Fatal(err)
	}
}

func applyDelta(st *Store, key string, d []byte, size int64, id uint64) error {
	_, err := st.PutEngineDelta(context.Background(), key, bytes.NewReader(d), FileMeta{Size: size, SaveID: SaveIDString(id), Format: FormatEngineDelta})
	return err
}

func materialized(t *testing.T, st *Store, key string) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out.bin")
	if _, err := st.Materialize(key, out); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEngineDeltaRebuildsTheSave(t *testing.T) {
	st := NewStore(t.TempDir())
	base := sealed(fakeSession{model: 1, config: 2, cells: 4000, stateSeed: 1}.bytes())
	fullPut(t, st, "c", base, 1)

	turns := []fakeSession{
		{model: 1, config: 2, cells: 4600, stateSeed: 2},            // grows
		{model: 1, config: 2, cells: 5200, stateSeed: 3},            // grows again, against a delta-built replica
		{model: 1, config: 2, cells: 3000, stateSeed: 4, kvSeed: 9}, // rewritten from cell 0, shorter
	}
	prev := base
	for i, s := range turns {
		next := sealed(s.bytes())
		d := engineDelta(t, prev, next, uint64(i+1), uint64(i+2))
		if i < 2 && len(d) >= len(next)/2 {
			t.Fatalf("turn %d: the delta is %d of %d bytes; the K/V prefix should be kept", i, len(d), len(next))
		}
		if err := applyDelta(st, "c", d, int64(len(next)), uint64(i+2)); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		if got := materialized(t, st, "c"); !bytes.Equal(got, next) {
			t.Fatalf("turn %d: the rebuilt file differs from the engine's full save", i)
		}
		m, _ := st.Manifest(context.Background(), "c")
		if m == nil || m.SaveID != SaveIDString(uint64(i+2)) {
			t.Fatalf("turn %d: manifest %+v", i, m)
		}
		prev = next
	}
	// a full save after deltas diffs against the rebuilt replica as before
	last := sealed(fakeSession{model: 1, config: 2, cells: 3400, stateSeed: 5, kvSeed: 9}.bytes())
	fullPut(t, st, "c", last, 9)
	if got := materialized(t, st, "c"); !bytes.Equal(got, last) {
		t.Fatal("a full save after deltas")
	}
}

func TestEngineDeltaAgainstAnotherBaseKeepsTheReplica(t *testing.T) {
	st := NewStore(t.TempDir())
	base := sealed(fakeSession{model: 1, config: 2, cells: 4000, stateSeed: 1}.bytes())
	fullPut(t, st, "c", base, 1)
	next := sealed(fakeSession{model: 1, config: 2, cells: 4600, stateSeed: 2}.bytes())
	err := applyDelta(st, "c", engineDelta(t, base, next, 7, 8), int64(len(next)), 8)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusConflict {
		t.Fatalf("another base: %v", err)
	}
	if got := materialized(t, st, "c"); !bytes.Equal(got, base) {
		t.Fatal("a refused delta must leave the replica as it was")
	}
	other := sealed(fakeSession{model: 1, config: 3, cells: 4600, stateSeed: 2}.bytes())
	if err := applyDelta(st, "c", engineDelta(t, base, other, 1, 8), int64(len(other)), 8); !errors.As(err, &se) || se.Code != http.StatusConflict {
		t.Fatalf("another configuration: %v", err)
	}
}

func TestEngineDeltaRefusesDamage(t *testing.T) {
	base := sealed(fakeSession{model: 1, config: 2, cells: 4000, stateSeed: 1}.bytes())
	next := sealed(fakeSession{model: 1, config: 2, cells: 4600, stateSeed: 2}.bytes())
	good := engineDelta(t, base, next, 1, 2)
	for _, tc := range []struct {
		name string
		mod  func([]byte)
	}{
		{"header", func(d []byte) { d[50] ^= 1 }},
		{"payload byte", func(d []byte) { d[len(d)/2] ^= 1 }},
		{"trailer", func(d []byte) { d[len(d)-12] ^= 1 }},
		{"truncated", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := NewStore(t.TempDir())
			fullPut(t, st, "c", base, 1)
			d := bytes.Clone(good)
			if tc.mod != nil {
				tc.mod(d)
			} else {
				d = d[:len(d)-100]
			}
			if err := applyDelta(st, "c", d, int64(len(next)), 2); err == nil {
				t.Fatal("a damaged delta was applied")
			}
		})
	}
}

// A replica damaged at rest fails its assembly on the chunk check, before the
// engine sees it.
func TestEngineReplicaDamagedAtRest(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	base := sealed(fakeSession{model: 1, config: 2, cells: 4000, stateSeed: 1}.bytes())
	fullPut(t, st, "c", base, 1)
	next := sealed(fakeSession{model: 1, config: 2, cells: 4600, stateSeed: 2}.bytes())
	if err := applyDelta(st, "c", engineDelta(t, base, next, 1, 2), int64(len(next)), 2); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "c", "s-kv1.k"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteAt([]byte{0xde, 0xad}, 1000)
	f.Close()
	if _, err := st.Materialize("c", filepath.Join(t.TempDir(), "x.bin")); err == nil {
		t.Fatal("a damaged chunk must fail the assembly")
	}
}

// kvsharetest's delta writer, which the other packages' fake engines use,
// writes what this file's writer writes from the parsed layout.
func TestKvsharetestWritesTheSameDelta(t *testing.T) {
	base := kvsharetest.Session{Model: 1, Config: 2, Cells: 4000, StateSeed: 1}
	next := kvsharetest.Session{Model: 1, Config: 2, Cells: 4600, StateSeed: 2}
	if !bytes.Equal(next.Sealed(), sealed(next.Bytes())) {
		t.Fatal("Sealed differs from sealing Bytes")
	}
	if !bytes.Equal(next.EngineDelta(base, 1, 2), engineDelta(t, base.Sealed(), next.Sealed(), 1, 2)) {
		t.Fatal("the two delta writers differ")
	}
}
