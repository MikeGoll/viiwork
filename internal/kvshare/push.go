package kvshare

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"
)

// PushResult times one push.
type PushResult struct {
	Plan    *Plan
	Put     PutResult
	DiffMs  float64 // parsing, hashing and planning on the sender
	WireMs  float64 // streaming the delta until the receiver answered
	TotalMs float64
}

// Push sends the session file at path to the receiver under key, as a delta
// against m (the receiver's manifest; nil sends the whole file).
func Push(ctx context.Context, t Transport, addr, key, path string, m *Manifest) (PushResult, error) {
	return PushSave(ctx, t, addr, key, path, m, "")
}

// PushSave is Push for a file the engine saved under saveID, which the
// receiver records so that the engine's next save can be a delta against it.
func PushSave(ctx context.Context, t Transport, addr, key, path string, m *Manifest, saveID string) (PushResult, error) {
	start := time.Now()
	f, err := os.Open(path)
	if err != nil {
		return PushResult{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return PushResult{}, err
	}
	l, err := ParseLayout(f, st.Size())
	if err != nil {
		return PushResult{}, err
	}
	p, err := Diff(f, l, m)
	if err != nil {
		return PushResult{}, err
	}
	res := PushResult{Plan: p, DiffMs: ms(time.Since(start))}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(WriteDelta(pw, f, p)) }()
	wire := time.Now()
	if mp, ok := t.(MetaPutter); ok {
		res.Put, err = mp.PutFile(ctx, addr, key, FileMeta{Size: p.Size, Digest: p.Digest, SaveID: saveID}, pr)
	} else {
		res.Put, err = t.Put(ctx, addr, key, pr)
	}
	pr.CloseWithError(io.ErrClosedPipe)
	res.WireMs = ms(time.Since(wire))
	res.TotalMs = ms(time.Since(start))
	return res, err
}

// PushEngineDelta sends an engine delta file (STRSDLT) as it is: the
// receiver applies it to the replica of its base save. The transport must
// carry the file's meta (MetaPutter). Plan reports the delta's bytes as Data
// and the rebuilt file as Size.
func PushEngineDelta(ctx context.Context, t Transport, addr, key, path string) (PushResult, error) {
	start := time.Now()
	mp, ok := t.(MetaPutter)
	if !ok {
		return PushResult{}, fmt.Errorf("kvshare: transport %s cannot carry an engine delta", t.Name())
	}
	f, err := os.Open(path)
	if err != nil {
		return PushResult{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return PushResult{}, err
	}
	h, err := ReadEngineDeltaHeader(f)
	if err != nil {
		return PushResult{}, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return PushResult{}, err
	}
	res := PushResult{Plan: &Plan{Size: int64(h.FileSize), Data: st.Size(), Model: h.Model, Config: h.Config}}
	wire := time.Now()
	res.Put, err = mp.PutFile(ctx, addr, key, FileMeta{Size: int64(h.FileSize), SaveID: SaveIDString(h.New), Format: FormatEngineDelta}, f)
	res.WireMs = ms(time.Since(wire))
	res.TotalMs = ms(time.Since(start))
	return res, err
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
