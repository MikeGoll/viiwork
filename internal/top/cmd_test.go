package top

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

// fakeNode serves fixture()'s cluster on the stream, then holds it open.
func fakeNode(t *testing.T, stream bool) string {
	c := *fixture().Cluster
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == meshapi.PathMeshStream && stream:
			w.Header().Set("Content-Type", "text/event-stream")
			b, _ := json.Marshal(c)
			fmt.Fprintf(w, "event: cluster\ndata: %s\n\n", b)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case r.URL.Path == meshapi.PathStatus:
			json.NewEncoder(w).Encode(meshapi.NodeStatus{Node: "node-a", Ver: "v2.0.0"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// A node's stream hub starts following the other members only when a viewer
// connects, so their replayed requests arrive after the first snapshot. The
// one frame --once prints must include them.
func TestOnceWaitsForTheReplay(t *testing.T) {
	c := *fixture().Cluster
	late := meshapi.MeshEvent{
		Event:  meshapi.Event{Time: t0.Unix() - 3, Type: meshapi.EventRequest, RequestID: 42, Message: meshapi.RequestStarted("m", "m/0"), Replay: true},
		NodeID: "id-node-b", Hostname: "node-b",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(c)
		fmt.Fprintf(w, "event: cluster\ndata: %s\n\n", b)
		w.(http.Flusher).Flush()
		time.Sleep(300 * time.Millisecond)
		e, _ := json.Marshal(late)
		fmt.Fprintf(w, "event: activity\ndata: %s\n\n", e)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	var out bytes.Buffer
	if code := Run(context.Background(), []string{"--once", "--node", strings.TrimPrefix(srv.URL, "http://")}, env(&out, io.Discard, false, nil, nil)); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "node-b") || !strings.Contains(out.String(), "    42") {
		t.Fatalf("the replayed request is missing:\n%s", out.String())
	}
}

type fakeScreen struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	restored  int
	panicSize bool
}

func (f *fakeScreen) Size() (int, int, error) {
	if f.panicSize {
		panic("boom")
	}
	return 120, 40, nil
}
func (f *fakeScreen) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.Write(p)
}
func (f *fakeScreen) Restore() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restored++
	return nil
}
func (f *fakeScreen) contains(s string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Contains(f.buf.String(), s)
}

func env(out, errOut io.Writer, interactive bool, scr Screen, keys io.Reader) Env {
	return Env{
		Stdout: out, Stderr: errOut, Client: &http.Client{},
		ReadFile:    func(string) ([]byte, error) { return nil, fmt.Errorf("no file") },
		LookupEnv:   func(string) (string, bool) { return "", false },
		Now:         func() time.Time { return t0 },
		Interactive: func() bool { return interactive },
		OpenScreen:  func() (Screen, error) { return scr, nil },
		Keys:        keys,
	}
}

func TestOnceWhenNotATerminal(t *testing.T) {
	node := fakeNode(t, true)
	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"--node", node}, env(&out, &errOut, false, nil, nil))
	if code != 0 || !strings.Contains(out.String(), "via node-a") || strings.Contains(out.String(), "\x1b") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
}

func TestOnceHostScreen(t *testing.T) {
	node := fakeNode(t, true)
	var out bytes.Buffer
	if code := Run(context.Background(), []string{"--once", "--node", node, "--host", "node-a"}, env(&out, io.Discard, true, nil, nil)); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "HOST node-a") {
		t.Fatalf("stdout %q", out.String())
	}
}

func TestNoStreamNamesTheVersion(t *testing.T) {
	node := fakeNode(t, false)
	var errOut bytes.Buffer
	code := Run(context.Background(), []string{"--once", "--node", node}, env(io.Discard, &errOut, false, nil, nil))
	if code != 1 || !strings.Contains(errOut.String(), "v2.0.0") || !strings.Contains(errOut.String(), meshapi.PathMeshStream) {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
}

func TestOnceUnreachableFailsFast(t *testing.T) {
	var errOut bytes.Buffer
	start := time.Now()
	code := Run(context.Background(), []string{"--once", "--node", "127.0.0.1:1"}, env(io.Discard, &errOut, false, nil, nil))
	if code != 1 || time.Since(start) > 5*time.Second {
		t.Fatalf("exit %d after %v, stderr %q", code, time.Since(start), errOut.String())
	}
}

func TestUsage(t *testing.T) {
	var errOut bytes.Buffer
	if code := Run(context.Background(), []string{"--frob"}, env(io.Discard, &errOut, false, nil, nil)); code != 2 || !strings.Contains(errOut.String(), "usage: viiwork top") {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
}

func TestInteractiveKeysAndRestore(t *testing.T) {
	node := fakeNode(t, true)
	scr := &fakeScreen{}
	kr, kw := io.Pipe()
	done := make(chan int, 1)
	go func() {
		done <- Run(context.Background(), []string{"--node", node}, env(io.Discard, io.Discard, true, scr, kr))
	}()

	waitFor(t, func() bool { return scr.contains("via node-a") })
	kw.Write([]byte("\x1b[B\r")) // down to node-b, open it
	waitFor(t, func() bool { return scr.contains("HOST node-b") })
	kw.Write([]byte("\x1b")) // back to the fleet
	kw.Write([]byte("q"))
	select {
	case code := <-done:
		if code != 0 || scr.restored == 0 {
			t.Fatalf("exit %d, restored %d", code, scr.restored)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("q did not quit")
	}
}

func TestRestoreOnCancel(t *testing.T) {
	node := fakeNode(t, true)
	scr := &fakeScreen{}
	kr, _ := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- Run(ctx, []string{"--node", node}, env(io.Discard, io.Discard, true, scr, kr)) }()
	waitFor(t, func() bool { return scr.contains("via node-a") })
	cancel() // what SIGTERM does through main's signal context
	<-done
	if scr.restored == 0 {
		t.Fatal("terminal not restored on cancel")
	}
}

func TestRestoreOnPanic(t *testing.T) {
	scr := &fakeScreen{panicSize: true}
	kr, _ := io.Pipe()
	defer func() {
		if recover() == nil {
			t.Fatal("panic swallowed")
		}
		if scr.restored == 0 {
			t.Fatal("terminal not restored on panic")
		}
	}()
	Run(context.Background(), []string{"--node", "127.0.0.1:1"}, env(io.Discard, io.Discard, true, scr, kr))
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
