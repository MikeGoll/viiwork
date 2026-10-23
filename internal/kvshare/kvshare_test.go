package kvshare

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/kvshare/kvsharetest"
)

// fakeSession is kvsharetest.Session under the names these tests grew up with.
type fakeSession struct {
	model, config uint64
	cells         int
	stateSeed     uint64
	kvSeed        uint64
}

const (
	fakeState  = kvsharetest.StateBytes
	fakeNKV    = kvsharetest.Layers
	fakeCellKV = kvsharetest.CellKV
)

func (f fakeSession) bytes() []byte {
	return kvsharetest.Session{Model: f.model, Config: f.config, Cells: f.cells, StateSeed: f.stateSeed, KVSeed: f.kvSeed}.Bytes()
}

func layoutOf(t *testing.T, b []byte) *Layout {
	t.Helper()
	l, err := ParseLayout(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestParseLayout(t *testing.T) {
	b := fakeSession{model: 1, config: 2, cells: 3000, stateSeed: 1}.bytes()
	l := layoutOf(t, b)
	if l.Model != 1 || l.Config != 2 {
		t.Fatalf("identity %d/%d", l.Model, l.Config)
	}
	want := 2*7 + fakeNKV*5
	if len(l.Sections) != want {
		t.Fatalf("%d sections, want %d", len(l.Sections), want)
	}
	byName := map[string]Section{}
	for _, s := range l.Sections {
		byName[s.Name] = s
	}
	if byName["live.gdn"].Len != fakeState || byName["ckpt0.ids"].Len != 4*2995 || byName["kv2.k"].Len != 3000*fakeCellKV {
		t.Fatalf("section sizes: %+v", byName)
	}
	if _, err := ParseLayout(bytes.NewReader(b[:len(b)-1]), int64(len(b)-1)); err == nil {
		t.Fatal("a truncated file must not parse")
	}
	bad := bytes.Clone(b)
	bad[0] = 'X'
	if _, err := ParseLayout(bytes.NewReader(bad), int64(len(bad))); err == nil {
		t.Fatal("a wrong magic must not parse")
	}
}

// push sends next to the store under key through the in-process delta path,
// materializes it, and checks the assembled file is next.
func push(t *testing.T, st *Store, key string, next []byte) *Plan {
	t.Helper()
	m, err := st.Manifest(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	nl := layoutOf(t, next)
	p, err := Diff(bytes.NewReader(next), nl, m)
	if err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	if err := WriteDelta(&wire, bytes.NewReader(next), p); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(context.Background(), key, &wire); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out.bin")
	if _, err := st.Materialize(key, out); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, next) {
		t.Fatal("materialized file differs")
	}
	return p
}

// expectedData: everything travels except each K/V part's whole blocks inside
// the base's length (the K/V only grows; the state is rewritten).
func expectedData(t *testing.T, base, next []byte) int64 {
	kept := int64(0)
	for _, s := range layoutOf(t, base).Sections {
		if s.Name[:2] == "kv" {
			kept += s.Len / BlockSize * BlockSize
		}
	}
	return int64(len(next)) - kept
}

func TestDeltaNextTurn(t *testing.T) {
	st := NewStore(t.TempDir())
	base := fakeSession{model: 1, config: 2, cells: 6000, stateSeed: 1}.bytes()
	next := fakeSession{model: 1, config: 2, cells: 6600, stateSeed: 2}.bytes()
	if p := push(t, st, "c", base); p.Data != int64(len(base)) {
		t.Fatalf("first push sent %d of %d bytes", p.Data, len(base))
	}
	if p := push(t, st, "c", next); p.Data != expectedData(t, base, next) {
		t.Fatalf("delta carries %d bytes, want %d (of %d)", p.Data, expectedData(t, base, next), len(next))
	}
	// a third turn uses the manifest the store rebuilt incrementally
	third := fakeSession{model: 1, config: 2, cells: 7100, stateSeed: 3}.bytes()
	if p := push(t, st, "c", third); p.Data != expectedData(t, next, third) {
		t.Fatalf("third turn carries %d bytes, want %d", p.Data, expectedData(t, next, third))
	}
}

func TestDeltaIgnoresAnotherIdentity(t *testing.T) {
	st := NewStore(t.TempDir())
	push(t, st, "c", fakeSession{model: 1, config: 2, cells: 6000, stateSeed: 1}.bytes())
	next := fakeSession{model: 1, config: 3, cells: 6000, stateSeed: 1}.bytes()
	if p := push(t, st, "c", next); p.Data != int64(len(next)) {
		t.Fatalf("another config: %d of %d bytes kept", int64(len(next))-p.Data, len(next))
	}
}

func TestDeltaRewindRewritesKV(t *testing.T) {
	st := NewStore(t.TempDir())
	push(t, st, "c", fakeSession{model: 1, config: 2, cells: 6000, stateSeed: 1}.bytes())
	push(t, st, "c", fakeSession{model: 1, config: 2, cells: 5000, stateSeed: 2, kvSeed: 77}.bytes())
	// shorter, from the same K/V stream: a prefix of every part is kept
	push(t, st, "c", fakeSession{model: 1, config: 2, cells: 3000, stateSeed: 4, kvSeed: 77}.bytes())
}

func TestMaterializeRefusesACorruptReplica(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	b := fakeSession{model: 1, config: 2, cells: 6000, stateSeed: 1}.bytes()
	push(t, st, "c", b)
	f, err := os.OpenFile(filepath.Join(dir, "c", "s-kv1.v"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteAt([]byte{0xde, 0xad}, 100)
	f.Close()
	if _, err := st.Materialize("c", filepath.Join(t.TempDir(), "x.bin")); err == nil {
		t.Fatal("a changed section must fail the digest check")
	}
}

func TestPutRejectsKeepBeyondReplica(t *testing.T) {
	st := NewStore(t.TempDir())
	push(t, st, "c", fakeSession{model: 1, config: 2, cells: 2000, stateSeed: 1}.bytes())
	big := fakeSession{model: 1, config: 2, cells: 9000, stateSeed: 2}.bytes()
	l := layoutOf(t, big)
	// a manifest claiming more than the store holds: the plan keeps too much
	m, _ := FileManifest(bytes.NewReader(big), l)
	p, _ := Diff(bytes.NewReader(big), l, m)
	var wire bytes.Buffer
	_ = WriteDelta(&wire, bytes.NewReader(big), p)
	if _, err := st.Put(context.Background(), "c", &wire); err == nil {
		t.Fatal("keeping bytes the replica does not have must fail")
	}
	if m, _ := st.Manifest(context.Background(), "c"); m != nil {
		t.Fatal("a failed put must drop the replica")
	}
}

func TestTransports(t *testing.T) {
	for _, spec := range []string{"tcp", "http", "flate+tcp", "flate+http"} {
		t.Run(spec, func(t *testing.T) {
			tr, err := New(spec)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			store := NewStore(dir)
			srv, err := tr.Listen("127.0.0.1:0", store)
			if err != nil {
				t.Fatal(err)
			}
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			var prev []byte
			for turn, s := range []fakeSession{
				{model: 1, config: 2, cells: 4000, stateSeed: 1},
				{model: 1, config: 2, cells: 4400, stateSeed: 2},
			} {
				b := s.bytes()
				src := filepath.Join(t.TempDir(), "s.bin")
				if err := os.WriteFile(src, b, 0o600); err != nil {
					t.Fatal(err)
				}
				m, err := tr.Manifest(ctx, srv.Addr(), "conv-1")
				if err != nil {
					t.Fatal(err)
				}
				if (m == nil) != (turn == 0) {
					t.Fatalf("turn %d: manifest %v", turn, m != nil)
				}
				res, err := Push(ctx, tr, srv.Addr(), "conv-1", src, m)
				if err != nil {
					t.Fatal(err)
				}
				out := filepath.Join(t.TempDir(), "out.bin")
				if _, err := store.Materialize("conv-1", out); err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(out)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, b) || res.Put.Size != int64(len(b)) {
					t.Fatalf("turn %d: stored file differs", turn)
				}
				if turn == 1 && res.Plan.Data != expectedData(t, prev, b) {
					t.Fatalf("second turn sent %d bytes, want %d", res.Plan.Data, expectedData(t, prev, b))
				}
				prev = b
			}
			if _, err := tr.Manifest(ctx, srv.Addr(), "../etc"); err == nil {
				t.Fatal("a key with a path must be refused")
			}
		})
	}
}
