package kvshare

import (
	"compress/flate"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// PutResult is the receiver's answer to a delta.
type PutResult struct {
	Size    int64   `json:"size"`     // the rebuilt file
	ApplyMs float64 `json:"apply_ms"` // reading and applying the delta
}

// Handler is the receiving side of a transport: it holds snapshot files by key.
type Handler interface {
	// Manifest describes the file held under key; nil (and no error) when there is none.
	Manifest(ctx context.Context, key string) (*Manifest, error)
	// Put rebuilds the file under key from the delta stream and publishes it.
	Put(ctx context.Context, key string, delta io.Reader) (PutResult, error)
}

// Server is a listening transport.
type Server interface {
	Addr() string
	Close() error
}

// Transport carries manifests and deltas between two hosts. Implementations
// own only the bytes on the wire; what a manifest or delta means is this
// package's business, so every transport moves the same deltas and can be
// compared on equal terms.
type Transport interface {
	Name() string
	Listen(addr string, h Handler) (Server, error)
	Manifest(ctx context.Context, addr, key string) (*Manifest, error)
	Put(ctx context.Context, addr, key string, delta io.Reader) (PutResult, error)
}

var (
	regMu    sync.Mutex
	registry = map[string]func() Transport{}
	wrappers = map[string]func(Transport) Transport{}
)

// Register adds a transport under its name.
func Register(name string, f func() Transport) {
	regMu.Lock()
	defer regMu.Unlock()
	registry[name] = f
}

// RegisterWrapper adds a decorator usable as "<name>+<transport>".
func RegisterWrapper(name string, f func(Transport) Transport) {
	regMu.Lock()
	defer regMu.Unlock()
	wrappers[name] = f
}

// New builds a transport from a spec such as "tcp", "http" or "flate+tcp".
func New(spec string) (Transport, error) {
	regMu.Lock()
	defer regMu.Unlock()
	parts := strings.Split(spec, "+")
	f, ok := registry[parts[len(parts)-1]]
	if !ok {
		return nil, fmt.Errorf("kvshare: unknown transport %q (have %s)", parts[len(parts)-1], strings.Join(names(), ", "))
	}
	t := f()
	for i := len(parts) - 2; i >= 0; i-- {
		w, ok := wrappers[parts[i]]
		if !ok {
			return nil, fmt.Errorf("kvshare: unknown transport wrapper %q", parts[i])
		}
		t = w(t)
	}
	return t, nil
}

func names() []string {
	var n []string
	for k := range registry {
		n = append(n, k)
	}
	for k := range wrappers {
		n = append(n, k+"+")
	}
	sort.Strings(n)
	return n
}

// ValidKey reports whether key may name a file: letters, digits, '.', '_' and
// '-', not starting with a dot, at most 128 bytes.
func ValidKey(key string) bool {
	if key == "" || len(key) > 128 || key[0] == '.' {
		return false
	}
	for _, c := range key {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func init() {
	RegisterWrapper("flate", func(t Transport) Transport { return flateTransport{t} })
}

// flateTransport compresses the delta stream at BestSpeed. The state and K/V
// barely compress (measured ~94% with zstd); it exists to measure that on a
// slow link rather than to be used.
type flateTransport struct{ inner Transport }

func (f flateTransport) Name() string { return "flate+" + f.inner.Name() }

func (f flateTransport) Listen(addr string, h Handler) (Server, error) {
	return f.inner.Listen(addr, inflating{h})
}

func (f flateTransport) Manifest(ctx context.Context, addr, key string) (*Manifest, error) {
	return f.inner.Manifest(ctx, addr, key)
}

func (f flateTransport) Put(ctx context.Context, addr, key string, delta io.Reader) (PutResult, error) {
	pr, pw := io.Pipe()
	go func() {
		zw, _ := flate.NewWriter(pw, flate.BestSpeed)
		_, err := io.Copy(zw, delta)
		if err == nil {
			err = zw.Close()
		}
		pw.CloseWithError(err)
	}()
	res, err := f.inner.Put(ctx, addr, key, pr)
	pr.CloseWithError(io.ErrClosedPipe)
	return res, err
}

type inflating struct{ Handler }

func (i inflating) Put(ctx context.Context, key string, delta io.Reader) (PutResult, error) {
	zr := flate.NewReader(delta)
	defer zr.Close()
	return i.Handler.Put(ctx, key, zr)
}
