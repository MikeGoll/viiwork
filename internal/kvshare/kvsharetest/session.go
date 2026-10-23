// Package kvsharetest builds session files for tests: STRSESS v1 shaped like
// Strata's, small, and deterministic, so that two turns of one conversation
// share the prefixes a real continuation shares.
package kvsharetest

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand/v2"

	"github.com/janit/viiwork/v2/internal/kvshare/sesshash"
)

// Session is a live state, one checkpoint and three K/V layers of Cells
// cells. Bytes come from the seeds: two sessions with the same KVSeed share
// every K/V prefix, and StateSeed rewrites the running state.
type Session struct {
	Model, Config uint64
	Cells         int
	StateSeed     uint64
	KVSeed        uint64
}

const (
	// StateBytes is the GDN state: several whole 64 KiB blocks and a tail.
	StateBytes = 3*(64<<10) + 123
	// Layers is the K/V layers.
	Layers = 3
	// CellKV is the K bytes per cell, as at 2 heads x 256 int8.
	CellKV = 512
)

// Bytes is the session file. Its header hash and trailer are placeholders;
// Sealed is the same file as Strata would write it.
func (f Session) Bytes() []byte {
	return f.build(func(p *bytes.Buffer, _ string, part []byte) {
		_ = binary.Write(p, binary.LittleEndian, uint64(len(part)))
		p.Write(part)
	}, false)
}

// Sealed is Bytes with a real STRSESS header hash and payload hash.
func (f Session) Sealed() []byte {
	return f.build(func(p *bytes.Buffer, _ string, part []byte) {
		_ = binary.Write(p, binary.LittleEndian, uint64(len(part)))
		p.Write(part)
	}, true)
}

// ChunkSize is the engine's K/V chunk in a delta save.
const ChunkSize = 256 << 10

// EngineDelta is the STRSDLT v1 delta the engine writes for f against base:
// every K/V part keeps its whole chunks before the first chunk that differs
// from base's, and the rebuilt file is f.Sealed().
func (f Session) EngineDelta(base Session, baseID, newID uint64) []byte {
	baseParts := map[string][]byte{}
	base.build(func(_ *bytes.Buffer, name string, part []byte) { baseParts[name] = part }, false)
	le := binary.LittleEndian
	payload := f.build(func(p *bytes.Buffer, name string, part []byte) {
		b := baseParts[name]
		keep := 0
		for keep+ChunkSize <= min(len(part), len(b)) && bytes.Equal(part[keep:keep+ChunkSize], b[keep:keep+ChunkSize]) {
			keep += ChunkSize
		}
		p.Write(le.AppendUint64(nil, uint64(len(part))))
		p.Write(le.AppendUint64(nil, uint64(keep)))
		p.Write(part[keep:])
		n := (len(part) + ChunkSize - 1) / ChunkSize
		p.Write(le.AppendUint64(nil, uint64(n)))
		for i := range n {
			p.Write(le.AppendUint64(nil, sesshash.Sum(part[i*ChunkSize:min(len(part), (i+1)*ChunkSize)], 0)))
		}
	}, false)
	payload = payload[64 : len(payload)-16] // the STRSESS header and trailer
	h := make([]byte, 80)
	copy(h, "STRSDLT\x01")
	le.PutUint32(h[8:], 1)
	le.PutUint32(h[12:], 80)
	le.PutUint64(h[16:], f.Model)
	le.PutUint64(h[24:], f.Config)
	le.PutUint64(h[32:], uint64(len(payload)))
	le.PutUint64(h[40:], baseID)
	le.PutUint64(h[48:], newID)
	le.PutUint64(h[56:], uint64(len(f.Sealed())))
	le.PutUint64(h[72:], sesshash.Sum(h[:72], 0))
	out := append(h, payload...)
	out = le.AppendUint64(out, sesshash.Sum(payload, 0))
	return append(out, "STRSDEND"...)
}

// build writes the file, handing each K/V part to kv (which writes it in
// either encoding); seal writes real hashes.
func (f Session) build(kv func(p *bytes.Buffer, name string, part []byte), seal bool) []byte {
	var p bytes.Buffer
	u := func(v uint64) { _ = binary.Write(&p, binary.LittleEndian, v) }
	vec := func(n uint64, elem int, seed uint64) {
		u(n)
		r := rand.New(rand.NewPCG(seed, 1))
		b := make([]byte, int(n)*elem)
		for i := range b {
			b[i] = byte(r.Uint32())
		}
		p.Write(b)
	}
	for range 18 + 3 {
		u(7) // geometry, layer range, cvec
	}
	ckpt := func(tokens int, seed uint64) {
		vec(uint64(tokens), 4, 100) // ids share a prefix
		u(0)                        // no images
		vec(StateBytes, 1, seed)
		vec(4096, 1, seed+1)
		vec(512, 1, seed+2)
		vec(64, 1, 99) // dead: unchanged
		vec(48, 1, seed+3)
		u(1) // used
	}
	ckpt(f.Cells, f.StateSeed)
	u(1)
	ckpt(f.Cells-5, f.StateSeed+10)
	u(Layers)
	for l := range Layers {
		for range 7 {
			u(uint64(f.Cells))
		}
		for part := range 5 {
			// a K/V part is a deterministic per-cell stream: a longer conversation
			// extends it, and KVSeed rewrites it from cell 0
			n := f.Cells * CellKV
			if part >= 2 {
				n = f.Cells * 16
			}
			r := rand.New(rand.NewPCG(f.KVSeed+uint64(l*5+part), 2))
			b := make([]byte, n)
			for i := range b {
				b[i] = byte(r.Uint32())
			}
			kv(&p, fmt.Sprintf("kv%d.%s", l, []string{"k", "v", "k_scale", "v_scale", "pooled"}[part]), b)
		}
	}
	var h [64]byte
	copy(h[:], "STRSESS\x01")
	binary.LittleEndian.PutUint32(h[8:], 1)
	binary.LittleEndian.PutUint32(h[12:], 64)
	binary.LittleEndian.PutUint64(h[16:], f.Model)
	binary.LittleEndian.PutUint64(h[24:], f.Config)
	binary.LittleEndian.PutUint64(h[32:], uint64(p.Len()))
	trailer := []byte("hashhashSTRSEND\x01")
	if seal {
		binary.LittleEndian.PutUint64(h[56:], sesshash.Sum(h[:56], 0))
		binary.LittleEndian.PutUint64(trailer, sesshash.Sum(p.Bytes(), 0))
	}
	out := append(h[:], p.Bytes()...)
	return append(out, trailer...)
}
