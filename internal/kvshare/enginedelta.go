package kvshare

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// An engine delta (STRSDLT v1, janit/Strata kv-export) is what the engine
// writes when asked to save against a base save it still knows: the payload in
// STRSESS order, every section whole except the K/V buffers, which carry only
// what follows the last whole chunk shared with the base, plus the hash of
// every chunk of the new part. The receiver holds exactly the base save's
// bytes (its replica records the base's save_id), cuts each K/V section to the
// kept chunks, appends the rest, and checks every new chunk as it writes it.
//
//	header, 80 bytes:  "STRSDLT\x01" | u32 version=1 | u32 header_size=80 |
//	                   u64 model | u64 config | u64 delta payload length |
//	                   u64 base save_id | u64 new save_id |
//	                   u64 new STRSESS file size | u64 reserved |
//	                   u64 SessionHash64(bytes 0..71)
//	K/V buffer:        u64 size | u64 keep | size-keep bytes |
//	                   u64 n = ceil(size/ChunkSize) | n x u64 chunk hash
//	trailer, 16 bytes: u64 SessionHash64(delta payload) | "STRSDEND"
//
// The rebuilt file is a plain STRSESS file: the receiver writes its header,
// and computes its trailer when it assembles the file (Materialize), in the
// same pass that checks every chunk hash again, so a replica damaged at rest
// fails there rather than in the engine.

// ChunkSize is the engine's K/V chunk for delta saves and their hashes.
const ChunkSize = 256 << 10

const (
	engineDeltaMagic  = "STRSDLT\x01"
	engineDeltaEnd    = "STRSDEND"
	engineDeltaHeader = 80
	sessEnd           = "STRSEND\x01"
)

// FormatEngineDelta is the query value that marks a put as an engine delta.
const FormatEngineDelta = "strata-delta"

// EngineDeltaHeader is what an engine delta says about itself.
type EngineDeltaHeader struct {
	Model, Config uint64
	Payload       uint64 // the delta's own payload length
	Base, New     uint64 // save ids
	FileSize      uint64 // the rebuilt STRSESS file
}

// ReadEngineDeltaHeader reads and checks the header.
func ReadEngineDeltaHeader(r io.Reader) (EngineDeltaHeader, error) {
	var b [engineDeltaHeader]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return EngineDeltaHeader{}, fmt.Errorf("kvshare: engine delta header: %w", err)
	}
	le := binary.LittleEndian
	switch {
	case string(b[:8]) != engineDeltaMagic:
		return EngineDeltaHeader{}, errors.New("kvshare: not an engine delta")
	case le.Uint32(b[8:]) != 1 || le.Uint32(b[12:]) != engineDeltaHeader:
		return EngineDeltaHeader{}, fmt.Errorf("kvshare: engine delta version %d, header %d", le.Uint32(b[8:]), le.Uint32(b[12:]))
	case SessionHash64(b[:72], 0) != le.Uint64(b[72:]):
		return EngineDeltaHeader{}, errors.New("kvshare: engine delta header checksum mismatch")
	}
	return EngineDeltaHeader{
		Model: le.Uint64(b[16:]), Config: le.Uint64(b[24:]), Payload: le.Uint64(b[32:]),
		Base: le.Uint64(b[40:]), New: le.Uint64(b[48:]), FileSize: le.Uint64(b[56:]),
	}, nil
}

// sessHeaderBytes is a STRSESS v1 header: magic, version, header size, model,
// config, payload length, two reserved words and the hash of bytes 0..55.
func sessHeaderBytes(model, config, payload uint64) []byte {
	h := make([]byte, sessHeader)
	copy(h, sessMagic)
	le := binary.LittleEndian
	le.PutUint32(h[8:], sessVersion)
	le.PutUint32(h[12:], sessHeader)
	le.PutUint64(h[16:], model)
	le.PutUint64(h[24:], config)
	le.PutUint64(h[32:], payload)
	le.PutUint64(h[56:], SessionHash64(h[:56], 0))
	return h
}

// SaveIDString is a save id as the engine's API spells it.
func SaveIDString(id uint64) string { return fmt.Sprintf("%016x", id) }

func isKVSection(name string) bool { return strings.HasPrefix(name, "kv") }

// chunkHashes are the ChunkSize hashes of n bytes of f from 0, reusing prev's
// for the whole chunks below keep.
func chunkHashes(f io.ReaderAt, keep, n int64, prev []uint64) ([]uint64, error) {
	reuse := min(keep/ChunkSize, int64(len(prev)))
	out := append([]uint64(nil), prev[:reuse]...)
	buf := make([]byte, ChunkSize)
	for off := reuse * ChunkSize; off < n; off += ChunkSize {
		c := min(int64(ChunkSize), n-off)
		if _, err := f.ReadAt(buf[:c], off); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		out = append(out, SessionHash64(buf[:c], 0))
	}
	return out, nil
}

// deltaReader reads the delta's payload, hashing and counting it.
type deltaReader struct {
	r *bufio.Reader
	h *SessionHash
	n uint64
}

func (d *deltaReader) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	d.h.Write(p[:n])
	d.n += uint64(n)
	return n, err
}

func (d *deltaReader) u64() (uint64, error) {
	var b [8]byte
	if _, err := io.ReadFull(d, b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b[:]), nil
}

// rebuild is the replica being rebuilt from an engine delta: its items in
// file order and its framing bytes.
type rebuild struct {
	s      *Store
	key    string
	frames bytes.Buffer
	items  []replicaItem
	have   map[string]int64    // the base's section lengths
	chunks map[string][]uint64 // the base's chunk hashes, then the new ones
	keeps  map[string]int64
	tokens int64
	size   int64 // the rebuilt file so far
}

func (b *rebuild) frame(p []byte) {
	if n := len(b.items); n > 0 && b.items[n-1].Section == "" {
		b.items[n-1].Len += int64(len(p))
	} else {
		b.items = append(b.items, replicaItem{Len: int64(len(p)), Frame: int64(b.frames.Len())})
	}
	b.frames.Write(p)
	b.size += int64(len(p))
}

func (b *rebuild) u64Frame(d *deltaReader) (uint64, error) {
	v, err := d.u64()
	if err == nil {
		b.frame(binary.LittleEndian.AppendUint64(nil, v))
	}
	return v, err
}

func (b *rebuild) skip(d *deltaReader, n int) error {
	p := make([]byte, n)
	if _, err := io.ReadFull(d, p); err != nil {
		return err
	}
	b.frame(p)
	return nil
}

// vec is a whole section of count elements of elem bytes, as in STRSESS.
func (b *rebuild) vec(d *deltaReader, name string, elem uint64, limit uint64) error {
	n, err := b.u64Frame(d)
	if err != nil {
		return err
	}
	if elem != 0 && n > limit/elem {
		return fmt.Errorf("kvshare: %s: %d elements in a %d-byte file", name, n, limit)
	}
	if name == "live.ids" {
		b.tokens = int64(n)
	}
	return b.section(d, name, 0, int64(n*elem))
}

// section writes size bytes of name: keep from the base, then the delta's.
func (b *rebuild) section(d *deltaReader, name string, keep, size int64) error {
	if keep > b.have[name] {
		return fmt.Errorf("kvshare: %s: keep %d of a %d-byte base", name, keep, b.have[name])
	}
	if err := b.s.patchSection(b.key, Item{Section: name, Keep: keep, Len: size}, io.LimitReader(d, size-keep)); err != nil {
		return err
	}
	delete(b.have, name)
	b.keeps[name] = keep
	b.items = append(b.items, replicaItem{Section: name, Len: size})
	b.size += size
	return nil
}

// kv is one delta-coded K/V buffer: (size, keep, tail, n, hashes). The
// tail's chunks are checked as written, the kept chunks against the base's.
func (b *rebuild) kv(d *deltaReader, name string, limit uint64) error {
	size, err := d.u64()
	if err != nil {
		return err
	}
	keep, err := d.u64()
	if err != nil {
		return err
	}
	if size > limit || keep > size || keep%ChunkSize != 0 {
		return fmt.Errorf("kvshare: %s: keep %d of %d", name, keep, size)
	}
	b.frame(binary.LittleEndian.AppendUint64(nil, size)) // STRSESS: the byte count
	base := b.chunks[name]
	if int64(keep/ChunkSize) > int64(len(base)) {
		return fmt.Errorf("kvshare: %s: keep %d past the base's %d chunks", name, keep, len(base))
	}
	if err := b.section(d, name, int64(keep), int64(size)); err != nil {
		return err
	}
	n, err := d.u64()
	if err != nil {
		return err
	}
	if want := (size + ChunkSize - 1) / ChunkSize; n != want {
		return fmt.Errorf("kvshare: %s: %d chunk hashes for %d bytes", name, n, size)
	}
	hashes := make([]uint64, n)
	if n > 0 {
		raw := make([]byte, 8*n)
		if _, err := io.ReadFull(d, raw); err != nil {
			return err
		}
		for i := range hashes {
			hashes[i] = binary.LittleEndian.Uint64(raw[8*i:])
		}
	}
	kept := keep / ChunkSize
	for i := range kept {
		if hashes[i] != base[i] {
			return fmt.Errorf("kvshare: %s: chunk %d differs from the base's", name, i)
		}
	}
	path, err := b.s.sectionFile(b.key, name)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	got, err := chunkHashes(f, int64(keep), int64(size), hashes[:kept])
	if err != nil {
		return err
	}
	for i := kept; i < n; i++ {
		if got[i] != hashes[i] {
			return fmt.Errorf("kvshare: %s: chunk %d does not match the engine's hash", name, i)
		}
	}
	b.chunks[name] = hashes
	return nil
}

func (b *rebuild) checkpoint(d *deltaReader, tag string, limit uint64) error {
	if err := b.vec(d, tag+".ids", 4, limit); err != nil {
		return err
	}
	if err := b.vec(d, tag+".imgs", 16, limit); err != nil {
		return err
	}
	for _, part := range []string{"gdn", "ple", "tails", "dead", "block_pos"} {
		if err := b.vec(d, tag+"."+part, 1, limit); err != nil {
			return err
		}
	}
	return b.skip(d, 8) // used
}

func (b *rebuild) payload(d *deltaReader, limit uint64) error {
	if err := b.skip(d, 18*8+3*8); err != nil { // geometry, layer range, cvec
		return err
	}
	if err := b.checkpoint(d, "live", limit); err != nil {
		return err
	}
	nckpt, err := b.u64Frame(d)
	if err != nil {
		return err
	}
	if nckpt > 64 {
		return fmt.Errorf("kvshare: %d checkpoints", nckpt)
	}
	for i := range nckpt {
		if err := b.checkpoint(d, fmt.Sprintf("ckpt%d", i), limit); err != nil {
			return err
		}
	}
	nkv, err := b.u64Frame(d)
	if err != nil {
		return err
	}
	if nkv > 1024 {
		return fmt.Errorf("kvshare: %d K/V layers", nkv)
	}
	for i := range nkv {
		if err := b.skip(d, 7*8); err != nil { // format, cells, heads, head_dim, page_size, pooled_rows, idx_dim
			return err
		}
		for _, part := range []string{"k", "v", "k_scale", "v_scale", "pooled"} {
			if err := b.kv(d, fmt.Sprintf("kv%d.%s", i, part), limit); err != nil {
				return err
			}
		}
	}
	return nil
}

// PutEngineDelta rebuilds the replica under key from an engine delta against
// the save the replica holds. want.Size is the rebuilt file's size and
// want.SaveID the new save's id, both as the request declared them.
//
// A delta against another base, or for another model or configuration, is
// refused with 409 and leaves the replica as it was: the sender's next save
// asks for the right base. Any failure after writing began drops the replica,
// so the next push sends everything.
func (s *Store) PutEngineDelta(_ context.Context, key string, delta io.Reader, want FileMeta) (res PutResult, err error) {
	if !ValidKey(key) {
		return PutResult{}, fmt.Errorf("kvshare: invalid key %q", key)
	}
	unlock := s.lock(key)
	writing := false
	defer func() {
		if err != nil {
			if writing {
				_ = os.RemoveAll(s.keyDir(key))
				s.mu.Lock()
				delete(s.man, key)
				s.mu.Unlock()
			}
			unlock()
		}
	}()
	start := time.Now()
	br := bufio.NewReaderSize(delta, 1<<20)
	h, err := ReadEngineDeltaHeader(br)
	if err != nil {
		return PutResult{}, err
	}
	if int64(h.FileSize) != want.Size || SaveIDString(h.New) != want.SaveID {
		return PutResult{}, errors.New("kvshare: the delta builds another save than the request declared")
	}
	old, err := s.readState(key)
	if err != nil {
		return PutResult{}, err
	}
	switch {
	case old == nil || !old.Complete:
		return PutResult{}, Statusf(http.StatusConflict, "kvshare: no replica to apply a delta to")
	case old.Model != h.Model || old.Config != h.Config:
		return PutResult{}, Statusf(http.StatusConflict, "kvshare: the delta is for another model or configuration")
	case old.SaveID != SaveIDString(h.Base):
		return PutResult{}, Statusf(http.StatusConflict, "kvshare: the replica holds save %s, the delta is against %s", old.SaveID, SaveIDString(h.Base))
	}
	s.mu.Lock()
	prevMan := s.man[key]
	delete(s.man, key)
	s.mu.Unlock()
	b := &rebuild{s: s, key: key, have: map[string]int64{}, chunks: map[string][]uint64{}, keeps: map[string]int64{}}
	for _, it := range old.Items {
		if it.Section != "" {
			b.have[it.Section] = it.Len
		}
	}
	for name, c := range old.Chunks {
		b.chunks[name] = c
	}
	writing = true
	old.Complete = false
	if err := s.writeState(key, old); err != nil {
		return PutResult{}, err
	}
	b.frame(sessHeaderBytes(h.Model, h.Config, 0)) // the payload length is set below
	d := &deltaReader{r: br, h: NewSessionHash(0)}
	if err := b.payload(d, h.FileSize); err != nil {
		return PutResult{}, err
	}
	if d.n != h.Payload {
		return PutResult{}, fmt.Errorf("kvshare: delta payload is %d bytes, the header says %d", d.n, h.Payload)
	}
	var tr [16]byte
	if _, err := io.ReadFull(br, tr[:]); err != nil {
		return PutResult{}, fmt.Errorf("kvshare: delta trailer: %w", err)
	}
	if binary.LittleEndian.Uint64(tr[:8]) != d.h.Sum64() || string(tr[8:]) != engineDeltaEnd {
		return PutResult{}, errors.New("kvshare: delta payload checksum mismatch")
	}
	payload := b.size - sessHeader
	if b.size+sessTrailer != int64(h.FileSize) {
		return PutResult{}, fmt.Errorf("kvshare: the rebuilt file is %d bytes, the delta says %d", b.size+sessTrailer, h.FileSize)
	}
	frames := b.frames.Bytes()
	copy(frames, sessHeaderBytes(h.Model, h.Config, uint64(payload)))
	for gone := range b.have {
		if p, err := s.sectionFile(key, gone); err == nil {
			_ = os.Remove(p)
		}
	}
	if err := writeAtomic(filepath.Join(s.keyDir(key), "frames.bin"), frames); err != nil {
		return PutResult{}, err
	}
	next := &replicaState{Model: h.Model, Config: h.Config, Size: int64(h.FileSize), Items: b.items,
		Tokens: b.tokens, SaveID: SaveIDString(h.New), Chunks: b.chunks, Trailer: true,
		Complete: true, Updated: time.Now().UTC()}
	if err := s.writeState(key, next); err != nil {
		return PutResult{}, err
	}
	res = PutResult{Size: next.Size, ApplyMs: ms(time.Since(start))}
	go s.finish(key, next, b.keeps, prevMan, unlock)
	return res, nil
}
