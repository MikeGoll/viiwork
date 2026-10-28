// Package sesshash is Strata's session_hash64, for kvshare and its tests.
package sesshash

import (
	"encoding/binary"
	"math/bits"
)

// Hash is Strata's session_hash64 (src/core/conversation_file.cpp,
// SessionHasher): an xxHash64-style hash that always merges its four lanes,
// with the byte count added before the tail. Streaming: any split of the same
// bytes gives the same digest. The STRSESS header hash (bytes 0..55) and
// payload hash, and an STRSDLT delta's chunk hashes, are this with seed 0.
type Hash struct {
	lane    [4]uint64
	pending [32]byte
	np      int
	total   uint64
}

const (
	sp1 = 0x9E3779B185EBCA87
	sp2 = 0xC2B2AE3D27D4EB4F
	sp3 = 0x165667B19E3779F9
	sp4 = 0x85EBCA77C2B2AE63
	sp5 = 0x27D4EB2F165667C5
)

func sround(acc, w uint64) uint64 { return bits.RotateLeft64(acc+w*sp2, 31) * sp1 }

// New starts a hash with seed.
func New(seed uint64) *Hash {
	return &Hash{lane: [4]uint64{seed + sp1 + sp2, seed + sp2, seed, seed - sp1}}
}

func (h *Hash) block(p []byte) {
	for i := range h.lane {
		h.lane[i] = sround(h.lane[i], binary.LittleEndian.Uint64(p[8*i:]))
	}
}

// Write never fails.
func (h *Hash) Write(p []byte) (int, error) {
	n := len(p)
	h.total += uint64(n)
	if h.np > 0 {
		take := copy(h.pending[h.np:], p)
		h.np += take
		p = p[take:]
		if h.np < len(h.pending) {
			return n, nil
		}
		h.block(h.pending[:])
		h.np = 0
	}
	for ; len(p) >= 32; p = p[32:] {
		h.block(p)
	}
	h.np = copy(h.pending[:], p)
	return n, nil
}

// Sum64 is the digest of everything written so far.
func (h *Hash) Sum64() uint64 {
	l := h.lane
	d := bits.RotateLeft64(l[0], 1) + bits.RotateLeft64(l[1], 7) + bits.RotateLeft64(l[2], 12) + bits.RotateLeft64(l[3], 18)
	for _, v := range l {
		d ^= sround(0, v)
		d = d*sp1 + sp4
	}
	d += h.total
	i := 0
	for ; i+8 <= h.np; i += 8 {
		d ^= sround(0, binary.LittleEndian.Uint64(h.pending[i:]))
		d = bits.RotateLeft64(d, 27)*sp1 + sp4
	}
	for ; i < h.np; i++ {
		d ^= uint64(h.pending[i]) * sp5
		d = bits.RotateLeft64(d, 11) * sp1
	}
	d ^= d >> 33
	d *= sp2
	d ^= d >> 29
	d *= sp3
	d ^= d >> 32
	return d
}

// Sum is the one-shot form.
func Sum(p []byte, seed uint64) uint64 {
	h := New(seed)
	h.Write(p)
	return h.Sum64()
}
