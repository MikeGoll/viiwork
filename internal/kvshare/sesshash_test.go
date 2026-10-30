package kvshare

import (
	"bufio"
	"encoding/binary"
	"io"
	"math/rand/v2"
	"os"
	"testing"
)

// The streaming hash gives the one-shot digest for any split.
func TestSessionHashSplits(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for _, n := range []int{0, 1, 7, 8, 31, 32, 33, 63, 64, 100, 1000, 4097} {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(r.Uint32())
		}
		want := SessionHash64(b, 0)
		for trial := 0; trial < 20; trial++ {
			h := NewSessionHash(0)
			for rest := b; len(rest) > 0; {
				k := min(len(rest), 1+r.IntN(40))
				h.Write(rest[:k])
				rest = rest[k:]
			}
			if got := h.Sum64(); got != want {
				t.Fatalf("n=%d: split digest %x, one-shot %x", n, got, want)
			}
		}
	}
}

// KVSHARE_REAL_SESSION names a session file Strata wrote: its header hash and
// payload hash must both verify with the port.
func TestSessionHashOnARealFile(t *testing.T) {
	path := os.Getenv("KVSHARE_REAL_SESSION")
	if path == "" {
		t.Skip("set KVSHARE_REAL_SESSION to a Strata session file")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	var h [64]byte
	if _, err := io.ReadFull(f, h[:]); err != nil {
		t.Fatal(err)
	}
	if got, want := SessionHash64(h[:56], 0), binary.LittleEndian.Uint64(h[56:]); got != want {
		t.Fatalf("header hash %x, file says %x", got, want)
	}
	payload := int64(binary.LittleEndian.Uint64(h[32:]))
	if 64+payload+16 != st.Size() {
		t.Fatalf("payload %d in a %d-byte file", payload, st.Size())
	}
	ph := NewSessionHash(0)
	if _, err := io.Copy(ph, io.LimitReader(bufio.NewReaderSize(f, 4<<20), payload)); err != nil {
		t.Fatal(err)
	}
	var tr [16]byte
	if _, err := f.ReadAt(tr[:], 64+payload); err != nil { // the buffered reader read past the payload
		t.Fatal(err)
	}
	if got, want := ph.Sum64(), binary.LittleEndian.Uint64(tr[:8]); got != want {
		t.Fatalf("payload hash %x, file says %x", got, want)
	}
	t.Logf("%s: header and payload hashes verify (%d MiB)", path, st.Size()>>20)
}
