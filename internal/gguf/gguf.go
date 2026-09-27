// Package gguf reads the facts the setup wizard plans with from a GGUF model
// file's header: architecture, layer count, trained context, KV heads and head
// size, and the file's size across its shards. Tensor data is never read.
package gguf

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
)

// Value types, from the GGUF specification.
const (
	typeUint8 uint32 = iota
	typeInt8
	typeUint16
	typeInt16
	typeUint32
	typeInt32
	typeFloat32
	typeBool
	typeString
	typeArray
	typeUint64
	typeInt64
	typeFloat64
)

// Limits that stop a corrupt header from allocating or looping without end.
// Real headers are 6-10 MB (a 262k-token vocabulary with its scores and
// merges); maxHeader bounds how much of a file is ever read.
const (
	maxKVs     = 1 << 20
	maxString  = 1 << 20
	maxArray   = 1 << 26
	maxVersion = 3
	maxHeader  = 64 << 20
)

// sizes is each fixed-width value type's size in bytes.
var sizes = map[uint32]int{typeUint8: 1, typeInt8: 1, typeBool: 1, typeUint16: 2, typeInt16: 2,
	typeUint32: 4, typeInt32: 4, typeFloat32: 4, typeUint64: 8, typeInt64: 8, typeFloat64: 8}

// ErrNotGGUF is a file that does not start with the GGUF magic.
var ErrNotGGUF = errors.New("not a GGUF file")

// Info is what the planner needs from a model file.
type Info struct {
	Arch          string
	Layers        int
	ContextLength int
	HeadCountKV   int // the largest per layer
	HeadDim       int
	// KVHeads is the KV heads summed over the layers that keep a cache: a
	// per-layer count's sum (SSM layers carry 0), or the head count times the
	// attention layers — every layer, or one in full_attention_interval.
	KVHeads int
	Size    int64 // bytes, all shards
	Shards  int
}

// KVBytesPerToken is the f16 KV cache one token costs: K and V for every KV
// head of every attention layer. false when the header does not say enough.
func (i Info) KVBytesPerToken() (int64, bool) {
	if i.KVHeads <= 0 || i.HeadDim <= 0 {
		return 0, false
	}
	return 2 * int64(i.KVHeads) * int64(i.HeadDim) * 2, true
}

var shardRe = regexp.MustCompile(`^(.*)-(\d{5})-of-(\d{5})\.gguf$`)

// Read reads path's header and sums the sizes of every shard it belongs to.
func Read(path string) (Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return Info{}, err
	}
	defer f.Close()
	lr := &io.LimitedReader{R: f, N: maxHeader}
	info, err := readHeader(bufio.NewReaderSize(lr, 1<<16))
	if err != nil {
		if lr.N == 0 {
			return Info{}, fmt.Errorf("%s: header larger than %d MiB: not a model this reader trusts", path, maxHeader>>20)
		}
		return Info{}, fmt.Errorf("%s: %w", path, err)
	}
	info.Shards = 1
	if m := shardRe.FindStringSubmatch(filepath.Base(path)); m != nil {
		n, _ := strconv.Atoi(m[3])
		info.Shards = n
		var total int64
		for i := 1; i <= n; i++ {
			p := filepath.Join(filepath.Dir(path), fmt.Sprintf("%s-%05d-of-%05d.gguf", m[1], i, n))
			fi, err := os.Stat(p)
			if err != nil {
				return Info{}, fmt.Errorf("shard %s: %w", filepath.Base(p), err)
			}
			total += fi.Size()
		}
		info.Size = total
		return info, nil
	}
	fi, err := f.Stat()
	if err != nil {
		return Info{}, err
	}
	info.Size = fi.Size()
	return info, nil
}

type reader struct{ r *bufio.Reader }

func (r reader) u32() (uint32, error) {
	var v uint32
	err := binary.Read(r.r, binary.LittleEndian, &v)
	return v, err
}

func (r reader) u64() (uint64, error) {
	var v uint64
	err := binary.Read(r.r, binary.LittleEndian, &v)
	return v, err
}

// skipStr reads past a string without keeping it.
func (r reader) skipStr() error {
	n, err := r.u64()
	if err != nil {
		return err
	}
	if n > maxString {
		return fmt.Errorf("string of %d bytes in the header", n)
	}
	_, err = r.r.Discard(int(n))
	return err
}

func (r reader) str() (string, error) {
	n, err := r.u64()
	if err != nil {
		return "", err
	}
	if n > maxString {
		return "", fmt.Errorf("string of %d bytes in the header", n)
	}
	b := make([]byte, n)
	_, err = io.ReadFull(r.r, b)
	return string(b), err
}

// scalar reads one value of type t, returning integers as uint64 (ok) and
// skipping everything else.
func (r reader) scalar(t uint32) (uint64, bool, error) {
	switch t {
	case typeString:
		return 0, false, r.skipStr()
	case typeArray:
		return 0, false, errors.New("nested array")
	}
	n, ok := sizes[t]
	if !ok {
		return 0, false, fmt.Errorf("unknown value type %d", t)
	}
	var b [8]byte
	if _, err := io.ReadFull(r.r, b[:n]); err != nil {
		return 0, false, err
	}
	switch t {
	case typeUint8, typeUint16, typeUint32, typeUint64, typeInt8, typeInt16, typeInt32, typeInt64:
		return binary.LittleEndian.Uint64(b[:]), true, nil
	}
	return 0, false, nil
}

// value reads a value, returning an integer — or, for an integer array, its
// largest element and its sum — when it is one.
func (r reader) value(t uint32) (max, sum uint64, isInt bool, err error) {
	if t != typeArray {
		v, ok, err := r.scalar(t)
		return v, v, ok, err
	}
	et, err := r.u32()
	if err != nil {
		return 0, 0, false, err
	}
	n, err := r.u64()
	if err != nil {
		return 0, 0, false, err
	}
	if n > maxArray {
		return 0, 0, false, fmt.Errorf("array of %d elements in the header", n)
	}
	for i := uint64(0); i < n; i++ {
		v, ok, err := r.scalar(et)
		if err != nil {
			return 0, 0, false, err
		}
		if ok {
			isInt = true
			sum += v
			if v > max {
				max = v
			}
		}
	}
	return max, sum, isInt, nil
}

func readHeader(br *bufio.Reader) (Info, error) {
	r := reader{br}
	magic := make([]byte, 4)
	if _, err := io.ReadFull(br, magic); err != nil || string(magic) != "GGUF" {
		return Info{}, ErrNotGGUF
	}
	version, err := r.u32()
	if err != nil {
		return Info{}, err
	}
	if version < 2 || version > maxVersion {
		return Info{}, fmt.Errorf("GGUF version %d is not supported", version)
	}
	if _, err := r.u64(); err != nil { // tensor count
		return Info{}, err
	}
	n, err := r.u64()
	if err != nil {
		return Info{}, err
	}
	if n > maxKVs {
		return Info{}, fmt.Errorf("%d metadata entries in the header", n)
	}
	ints, sums := map[string]uint64{}, map[string]uint64{}
	arrays := map[string]bool{}
	var info Info
	for i := uint64(0); i < n; i++ {
		key, err := r.str()
		if err != nil {
			return Info{}, err
		}
		t, err := r.u32()
		if err != nil {
			return Info{}, err
		}
		if key == "general.architecture" && t == typeString {
			if info.Arch, err = r.str(); err != nil {
				return Info{}, err
			}
			continue
		}
		v, sum, ok, err := r.value(t)
		if err != nil {
			return Info{}, fmt.Errorf("%s: %w", key, err)
		}
		if ok {
			ints[key], sums[key], arrays[key] = v, sum, t == typeArray
		}
	}
	get := func(k string) int { return int(ints[info.Arch+"."+k]) }
	info.Layers = get("block_count")
	info.ContextLength = get("context_length")
	info.HeadCountKV = get("attention.head_count_kv")
	kvKey := info.Arch + ".attention.head_count_kv"
	switch {
	case arrays[kvKey]:
		info.KVHeads = int(sums[kvKey])
	case info.HeadCountKV > 0:
		attention := info.Layers
		if every := get("full_attention_interval"); every > 0 {
			attention = (info.Layers + every - 1) / every
		}
		info.KVHeads = info.HeadCountKV * attention
	}
	info.HeadDim = get("attention.key_length")
	if info.HeadDim == 0 {
		if heads := get("attention.head_count"); heads > 0 {
			info.HeadDim = get("embedding_length") / heads
		}
	}
	return info, nil
}
