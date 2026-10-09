package gguf

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writer builds a GGUF v3 header for tests.
type writer struct{ b bytes.Buffer }

func (w *writer) u32(v uint32) { binary.Write(&w.b, binary.LittleEndian, v) }
func (w *writer) u64(v uint64) { binary.Write(&w.b, binary.LittleEndian, v) }
func (w *writer) str(s string) { w.u64(uint64(len(s))); w.b.WriteString(s) }
func (w *writer) kvU32(k string, v uint32) {
	w.str(k)
	w.u32(typeUint32)
	w.u32(v)
}
func (w *writer) kvStr(k, v string) {
	w.str(k)
	w.u32(typeString)
	w.str(v)
}

func header(kvs func(w *writer), n int) []byte {
	w := &writer{}
	w.b.WriteString("GGUF")
	w.u32(3)
	w.u64(0)         // tensors
	w.u64(uint64(n)) // kv count
	kvs(w)
	return w.b.Bytes()
}

func write(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func llama(w *writer) {
	w.kvStr("general.architecture", "llama")
	// An array the reader must skip: a tokenizer's token list.
	w.str("tokenizer.ggml.tokens")
	w.u32(typeArray)
	w.u32(typeString)
	w.u64(3)
	w.str("a")
	w.str("bb")
	w.str("ccc")
	w.kvU32("llama.block_count", 32)
	w.kvU32("llama.context_length", 131072)
	w.kvU32("llama.embedding_length", 4096)
	w.kvU32("llama.attention.head_count", 32)
	w.kvU32("llama.attention.head_count_kv", 8)
}

func TestRead(t *testing.T) {
	p := write(t, "m.gguf", append(header(llama, 7), make([]byte, 1000)...))
	info, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Arch != "llama" || info.Layers != 32 || info.ContextLength != 131072 || info.HeadCountKV != 8 || info.HeadDim != 128 || info.Shards != 1 {
		t.Fatalf("info = %+v", info)
	}
	fi, _ := os.Stat(p)
	if info.Size != fi.Size() {
		t.Errorf("size %d, want %d", info.Size, fi.Size())
	}
	// 2 (K and V) × 32 layers × 8 heads × 128 × 2 bytes (f16)
	if kv, ok := info.KVBytesPerToken(); !ok || kv != 2*32*8*128*2 {
		t.Errorf("KVBytesPerToken = %d, %v", kv, ok)
	}
}

func TestExplicitKeyLengthAndPerLayerHeads(t *testing.T) {
	p := write(t, "q.gguf", header(func(w *writer) {
		w.kvStr("general.architecture", "qwen")
		w.kvU32("qwen.block_count", 4)
		w.kvU32("qwen.context_length", 32768)
		w.kvU32("qwen.attention.key_length", 256)
		// Per-layer KV heads (hybrid architectures): the largest counts.
		w.str("qwen.attention.head_count_kv")
		w.u32(typeArray)
		w.u32(typeUint32)
		w.u64(4)
		for _, v := range []uint32{0, 4, 0, 2} {
			w.u32(v)
		}
	}, 5))
	info, err := Read(p)
	if err != nil || info.HeadDim != 256 || info.HeadCountKV != 4 {
		t.Fatalf("info = %+v, %v", info, err)
	}
}

func TestMissingFieldsAreUncertain(t *testing.T) {
	p := write(t, "x.gguf", header(func(w *writer) {
		w.kvStr("general.architecture", "mystery")
		w.kvU32("mystery.block_count", 10)
	}, 2))
	info, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := info.KVBytesPerToken(); ok {
		t.Error("a KV size was invented without head counts")
	}
}

func TestShardsSum(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "big-00001-of-00002.gguf")
	os.WriteFile(first, append(header(llama, 7), make([]byte, 100)...), 0o644)
	os.WriteFile(filepath.Join(dir, "big-00002-of-00002.gguf"), make([]byte, 5000), 0o644)
	info, err := Read(first)
	if err != nil || info.Shards != 2 {
		t.Fatalf("info = %+v, %v", info, err)
	}
	a, _ := os.Stat(first)
	if info.Size != a.Size()+5000 {
		t.Errorf("size %d, want both shards", info.Size)
	}
	os.Remove(filepath.Join(dir, "big-00002-of-00002.gguf"))
	if _, err := Read(first); err == nil || !strings.Contains(err.Error(), "00002") {
		t.Errorf("a missing shard: %v", err)
	}
}

// Only layers with attention keep a KV cache. A per-layer head count sums
// (SSM layers carry 0), and full_attention_interval says one layer in N is
// full attention (Qwen3.5/3.8 hybrids: 1 in 4). Counting every layer at the
// largest head count overstated Qwen3.8's cache about four times.
func TestKVCountsOnlyAttentionLayers(t *testing.T) {
	perLayer := write(t, "h.gguf", header(func(w *writer) {
		w.kvStr("general.architecture", "hyb")
		w.kvU32("hyb.block_count", 4)
		w.kvU32("hyb.attention.key_length", 128)
		w.str("hyb.attention.head_count_kv")
		w.u32(typeArray)
		w.u32(typeUint32)
		w.u64(4)
		for _, v := range []uint32{0, 8, 0, 8} {
			w.u32(v)
		}
	}, 4))
	info, err := Read(perLayer)
	if err != nil {
		t.Fatal(err)
	}
	if kv, _ := info.KVBytesPerToken(); kv != 2*(8+8)*128*2 {
		t.Errorf("per-layer: %d bytes/token, want the sum over layers", kv)
	}
	interval := write(t, "q.gguf", header(func(w *writer) {
		w.kvStr("general.architecture", "qwen35")
		w.kvU32("qwen35.block_count", 65)
		w.kvU32("qwen35.attention.head_count_kv", 4)
		w.kvU32("qwen35.attention.key_length", 256)
		w.kvU32("qwen35.full_attention_interval", 4)
	}, 5))
	info, err = Read(interval)
	if err != nil {
		t.Fatal(err)
	}
	// ceil(65 / 4) = 17 attention layers × 4 heads × 256 × 2 (K, V) × 2 bytes
	if kv, _ := info.KVBytesPerToken(); kv != 2*17*4*256*2 {
		t.Errorf("interval: %d bytes/token, want 17 attention layers", kv)
	}
}

// A corrupt count cannot make the reader walk a whole multi-gigabyte file:
// the header is read from a bounded prefix.
func TestHeaderIsBounded(t *testing.T) {
	w := &writer{}
	w.b.WriteString("GGUF")
	w.u32(3)
	w.u64(0)
	w.u64(1)
	w.str("tokenizer.ggml.tokens")
	w.u32(typeArray)
	w.u32(typeString)
	w.u64(1 << 25) // a corrupt count, under the array cap
	p := write(t, "bogus.gguf", w.b.Bytes())
	f, _ := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	// 300 MiB of zero-length strings (8 zero bytes each) after the count.
	zeros := make([]byte, 1<<20)
	for i := 0; i < 300; i++ {
		f.Write(zeros)
	}
	f.Close()
	if _, err := Read(p); err == nil || !strings.Contains(err.Error(), "header") {
		t.Errorf("a header past the bound: %v", err)
	}
}

func TestRejects(t *testing.T) {
	cases := map[string][]byte{
		"not gguf":    []byte("PK\x03\x04 this is a zip"),
		"truncated":   header(llama, 7)[:40],
		"version 9":   append([]byte("GGUF\x09\x00\x00\x00"), make([]byte, 16)...),
		"huge count":  append(append([]byte("GGUF\x03\x00\x00\x00"), make([]byte, 8)...), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x0f),
		"huge string": header(func(w *writer) { w.u64(1 << 40); w.b.WriteString("k") }, 1),
	}
	for name, b := range cases {
		_, err := Read(write(t, "bad.gguf", b))
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
		if name == "not gguf" && !errors.Is(err, ErrNotGGUF) {
			t.Errorf("not gguf: %v", err)
		}
	}
}
