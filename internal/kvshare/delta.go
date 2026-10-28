package kvshare

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
)

// BlockSize is the manifest's granularity inside a section. A turn rewrites a
// K/V part from its reuse point on, so each of the ~65 parts wastes at most one
// block: 4 MiB at 64 KiB, where 1 MiB blocks wasted ~35 MB of a 237 MB delta.
const BlockSize = 64 << 10

// digestChunk is the granularity of a file digest: SHA-256 over the chunks'
// SHA-256s, so both sides hash in parallel.
const digestChunk = 4 << 20

// Manifest describes the snapshot a receiver holds for a key: its identity and
// the hash of every whole block of every section.
type Manifest struct {
	Model    uint64            `json:"model"`
	Config   uint64            `json:"config"`
	Size     int64             `json:"size"`
	Sections []SectionManifest `json:"sections"`
	// SaveID is the engine's id of the save the receiver holds, "" when
	// unknown: what a sender asks the engine for a delta against.
	SaveID string `json:"save_id,omitempty"`
}

type SectionManifest struct {
	Name   string `json:"name"`
	Len    int64  `json:"len"`
	Blocks []byte `json:"blocks"` // 16 bytes (truncated SHA-256) per whole block
}

func (s *SectionManifest) count() int64 { return int64(len(s.Blocks) / 16) }

func (s *SectionManifest) block(i int64) []byte { return s.Blocks[i*16 : i*16+16] }

// hashBlocks hashes the whole blocks of n bytes at off.
func hashBlocks(r io.ReaderAt, off, n int64) ([]byte, error) {
	out := make([]byte, n/BlockSize*16)
	err := parallel(int(n/BlockSize), BlockSize, func(b int, buf []byte) error {
		if _, err := r.ReadAt(buf, off+int64(b)*BlockSize); err != nil {
			return err
		}
		s := sha256.Sum256(buf)
		copy(out[b*16:], s[:16])
		return nil
	})
	return out, err
}

// FileManifest builds a manifest from a whole session file, as a receiver
// holding that file would.
func FileManifest(r io.ReaderAt, l *Layout) (*Manifest, error) {
	m := &Manifest{Model: l.Model, Config: l.Config, Size: l.Size}
	for _, s := range l.Sections {
		b, err := hashBlocks(r, s.Off, s.Len)
		if err != nil {
			return nil, err
		}
		m.Sections = append(m.Sections, SectionManifest{Name: s.Name, Len: s.Len, Blocks: b})
	}
	return m, nil
}

// FileDigest is SHA-256 over the SHA-256 of every digestChunk of the file.
func FileDigest(r io.ReaderAt, size int64) ([32]byte, error) {
	n := int((size + digestChunk - 1) / digestChunk)
	sums := make([]byte, n*32)
	err := parallel(n, digestChunk, func(i int, buf []byte) error {
		off := int64(i) * digestChunk
		c := min(digestChunk, size-off)
		if _, err := r.ReadAt(buf[:c], off); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		s := sha256.Sum256(buf[:c])
		copy(sums[i*32:], s[:])
		return nil
	})
	return sha256.Sum256(sums), err
}

// parallel runs fn(0..n-1) on GOMAXPROCS workers, each with its own buffer.
func parallel(n, bufSize int, fn func(i int, buf []byte) error) error {
	workers := min(runtime.GOMAXPROCS(0), max(n, 1))
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		next int
		ferr error
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var buf []byte
			for {
				mu.Lock()
				i := next
				next++
				stop := ferr != nil
				mu.Unlock()
				if i >= n || stop {
					return
				}
				if buf == nil {
					buf = make([]byte, bufSize)
				}
				if err := fn(i, buf); err != nil {
					mu.Lock()
					ferr = err
					mu.Unlock()
					return
				}
			}
		}()
	}
	wg.Wait()
	return ferr
}

// Item is one piece of a session file in order: framing bytes, which always
// travel, or a section, of which the receiver keeps the first Keep bytes of
// its own copy and receives the rest.
type Item struct {
	Section string // "" for framing
	Off     int64  // in the new file
	Len     int64
	Keep    int64
}

// Plan is a delta's structure without its data, and what it saves.
type Plan struct {
	Items  []Item
	Size   int64
	Data   int64    // bytes that travel
	Digest [32]byte // FileDigest of the new file
	Model  uint64
	Config uint64
}

// Diff plans a new session file against the receiver's manifest (nil: no
// base). Per section the receiver keeps the longest run of leading whole
// blocks it already holds: the K/V only grows from its reuse point and the
// running state is rewritten whole, so a prefix is all there is to find. A
// manifest with another identity is ignored.
func Diff(r io.ReaderAt, l *Layout, base *Manifest) (*Plan, error) {
	p := &Plan{Size: l.Size, Model: l.Model, Config: l.Config}
	baseSec := map[string]*SectionManifest{}
	if base != nil && base.Model == l.Model && base.Config == l.Config {
		for i := range base.Sections {
			baseSec[base.Sections[i].Name] = &base.Sections[i]
		}
	}
	keep := make([]int64, len(l.Sections))
	// per section, compare leading blocks until the first mismatch: a rewritten
	// state stops at block 0, a grown K/V part hashes what it keeps
	err := parallel(len(l.Sections), BlockSize, func(i int, buf []byte) error {
		s := l.Sections[i]
		bs := baseSec[s.Name]
		if bs == nil {
			return nil
		}
		for b := int64(0); b < bs.count() && (b+1)*BlockSize <= s.Len; b++ {
			if _, err := r.ReadAt(buf, s.Off+b*BlockSize); err != nil {
				return err
			}
			h := sha256.Sum256(buf)
			if string(h[:16]) != string(bs.block(b)) {
				break
			}
			keep[i] = (b + 1) * BlockSize
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	pos := int64(0)
	for i, s := range l.Sections {
		if s.Off > pos {
			p.Items = append(p.Items, Item{Off: pos, Len: s.Off - pos})
		}
		p.Items = append(p.Items, Item{Section: s.Name, Off: s.Off, Len: s.Len, Keep: keep[i]})
		pos = s.Off + s.Len
	}
	if pos < l.Size {
		p.Items = append(p.Items, Item{Off: pos, Len: l.Size - pos})
	}
	for _, it := range p.Items {
		p.Data += it.Len - it.Keep
	}
	if p.Digest, err = FileDigest(r, l.Size); err != nil {
		return nil, err
	}
	return p, nil
}

// Wire format of a delta (little-endian):
//
//	"KVDELTA2" | size u64 | digest [32] | model u64 | config u64 | items u64
//	item: name len u16 | name | len u64 | keep u64 | (len - keep) bytes
//
// Framing is an item with an empty name and keep 0.
const deltaMagic = "KVDELTA2"

// WriteDelta streams the plan with the new file's bytes past each item's keep.
func WriteDelta(w io.Writer, r io.ReaderAt, p *Plan) error {
	bw := bufio.NewWriterSize(w, 4<<20)
	hdr := make([]byte, 0, 72)
	hdr = append(hdr, deltaMagic...)
	hdr = binary.LittleEndian.AppendUint64(hdr, uint64(p.Size))
	hdr = append(hdr, p.Digest[:]...)
	hdr = binary.LittleEndian.AppendUint64(hdr, p.Model)
	hdr = binary.LittleEndian.AppendUint64(hdr, p.Config)
	hdr = binary.LittleEndian.AppendUint64(hdr, uint64(len(p.Items)))
	if _, err := bw.Write(hdr); err != nil {
		return err
	}
	for _, it := range p.Items {
		rec := binary.LittleEndian.AppendUint16(nil, uint16(len(it.Section)))
		rec = append(rec, it.Section...)
		rec = binary.LittleEndian.AppendUint64(rec, uint64(it.Len))
		rec = binary.LittleEndian.AppendUint64(rec, uint64(it.Keep))
		if _, err := bw.Write(rec); err != nil {
			return err
		}
		if _, err := io.Copy(bw, io.NewSectionReader(r, it.Off+it.Keep, it.Len-it.Keep)); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// DeltaHeader is what a delta stream says about the file it builds.
type DeltaHeader struct {
	Size          int64
	Digest        [32]byte
	Model, Config uint64
	Items         uint64
}

// DeltaReader walks a delta stream item by item.
type DeltaReader struct {
	br     *bufio.Reader
	Header DeltaHeader
	left   uint64
	built  int64
}

func NewDeltaReader(r io.Reader) (*DeltaReader, error) {
	br := bufio.NewReaderSize(r, 4<<20)
	var hdr [72]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return nil, fmt.Errorf("kvshare: delta header: %w", err)
	}
	if string(hdr[:8]) != deltaMagic {
		return nil, errors.New("kvshare: not a delta stream")
	}
	d := &DeltaReader{br: br}
	d.Header.Size = int64(binary.LittleEndian.Uint64(hdr[8:]))
	copy(d.Header.Digest[:], hdr[16:48])
	d.Header.Model = binary.LittleEndian.Uint64(hdr[48:])
	d.Header.Config = binary.LittleEndian.Uint64(hdr[56:])
	d.Header.Items = binary.LittleEndian.Uint64(hdr[64:])
	if d.Header.Size < 0 || d.Header.Items > 1<<20 {
		return nil, fmt.Errorf("kvshare: delta of %d bytes in %d items", d.Header.Size, d.Header.Items)
	}
	d.left = d.Header.Items
	return d, nil
}

// Next returns the next item and a reader of its len-keep new bytes, which
// must be consumed before the next call; io.EOF after the last item.
func (d *DeltaReader) Next() (Item, io.Reader, error) {
	if d.left == 0 {
		if d.built != d.Header.Size {
			return Item{}, nil, fmt.Errorf("kvshare: delta built %d of %d bytes", d.built, d.Header.Size)
		}
		return Item{}, nil, io.EOF
	}
	d.left--
	var nl [2]byte
	if _, err := io.ReadFull(d.br, nl[:]); err != nil {
		return Item{}, nil, err
	}
	name := make([]byte, binary.LittleEndian.Uint16(nl[:]))
	if _, err := io.ReadFull(d.br, name); err != nil {
		return Item{}, nil, err
	}
	var lk [16]byte
	if _, err := io.ReadFull(d.br, lk[:]); err != nil {
		return Item{}, nil, err
	}
	it := Item{Section: string(name), Off: d.built,
		Len: int64(binary.LittleEndian.Uint64(lk[:])), Keep: int64(binary.LittleEndian.Uint64(lk[8:]))}
	if it.Len < 0 || it.Keep < 0 || it.Keep > it.Len || it.Len > d.Header.Size-d.built ||
		(it.Section == "" && it.Keep != 0) {
		return Item{}, nil, fmt.Errorf("kvshare: delta item %q len %d keep %d at %d of %d", it.Section, it.Len, it.Keep, d.built, d.Header.Size)
	}
	d.built += it.Len
	return it, io.LimitReader(d.br, it.Len-it.Keep), nil
}
