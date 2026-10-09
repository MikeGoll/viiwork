package top

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/top/stream"
	"github.com/janit/viiwork/v2/meshapi"
)

const Usage = `usage: viiwork top [--node host:port] [--config path] [--host name] [--once]

  --node host:port   the node to watch from (default 127.0.0.1:<api.port> from --config, else 127.0.0.1:8086);
                     any member shows the whole mesh
  --config path      viiwork.yaml, read only for api.port
  --host name        open this host's screen; the name only filters the view
  --once             print one plain-text frame and exit (the default when stdout is not a terminal)

keys: ↑↓ host · enter detail · esc back · m sort models · p pause · q quit
`

const (
	onceTimeout = 10 * time.Second
	onceSettle  = 2 * time.Second        // longest wait for the replay after the first snapshot
	onceQuiet   = 500 * time.Millisecond // a stream this quiet has finished replaying
	onceWidth   = 120
	onceHeight  = 1000 // --once never cuts the frame
	frameEvery  = 100 * time.Millisecond
)

// Screen is a full-screen terminal. term.Terminal implements it.
type Screen interface {
	Size() (w, h int, err error)
	Write(p []byte) (int, error)
	Restore() error
}

// Env is everything the command touches outside itself.
type Env struct {
	Stdout      io.Writer
	Stderr      io.Writer
	Client      *http.Client
	ReadFile    func(string) ([]byte, error)
	LookupEnv   func(string) (string, bool)
	Now         func() time.Time
	Interactive func() bool // stdin and stdout are both terminals
	OpenScreen  func() (Screen, error)
	Keys        io.Reader // raw bytes from the keyboard
}

// Run is `viiwork top`. Exit 0 on success, 1 on failure, 2 on a usage error.
func Run(ctx context.Context, args []string, env Env) int {
	fs := flag.NewFlagSet("top", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	nodeFlag := fs.String("node", "", "")
	configPath := fs.String("config", "", "")
	host := fs.String("host", "", "")
	once := fs.Bool("once", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprint(env.Stderr, Usage)
		return 2
	}
	port := config.Defaults().API.Port
	if *configPath != "" {
		data, err := env.ReadFile(*configPath)
		if err != nil {
			fmt.Fprintf(env.Stderr, "reading %s: %v\n", *configPath, err)
			return 2
		}
		cfg, err := config.Parse(data)
		if err != nil {
			fmt.Fprintf(env.Stderr, "%s: %v\n", *configPath, err)
			return 2
		}
		port = cfg.API.Port
	}
	node := *nodeFlag
	if node == "" {
		node = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	}
	v := View{}
	if *host != "" {
		v.Screen, v.Host = Host, *host
	}
	if *once || !env.Interactive() {
		return runOnce(ctx, env, node, v)
	}
	return runLive(ctx, env, node, v)
}

func follow(ctx context.Context, env Env, node string) (<-chan stream.Message, <-chan error) {
	msgs := make(chan stream.Message, 64)
	errc := make(chan error, 1)
	f := &stream.Follower{Client: env.Client, Node: node}
	go func() { errc <- f.Run(ctx, msgs) }()
	return msgs, errc
}

func apply(s *State, m stream.Message, now time.Time) {
	switch {
	case m.Connected:
		s.Connected, s.Err = true, ""
	case m.Lost != nil:
		s.Connected, s.Err = false, m.Lost.Error()
		s.Reset() // the replay on reconnect rebuilds it
	case m.Cluster != nil:
		s.Apply(*m.Cluster, now)
	case m.Event != nil:
		s.ApplyEvent(*m.Event)
	}
}

func runOnce(ctx context.Context, env Env, node string, v View) int {
	ctx, cancel := context.WithTimeout(ctx, onceTimeout)
	defer cancel()
	msgs, errc := follow(ctx, env, node)
	s := NewState()
	width := onceWidth
	if c, ok := env.LookupEnv("COLUMNS"); ok {
		if n, err := strconv.Atoi(c); err == nil && n > 0 {
			width = n
		}
	}
	// The node starts following the other members only when a viewer
	// connects, so their replayed requests arrive after the first snapshot.
	// Keep reading until the stream has been quiet for onceQuiet, or for at
	// most onceSettle after the first snapshot, then print.
	var settle, quiet <-chan time.Time
	show := func() int {
		fmt.Fprint(env.Stdout, Plain(Render(s, v, width, onceHeight, env.Now())))
		return 0
	}
	for {
		select {
		case m := <-msgs:
			if m.Lost != nil {
				if s.Cluster != nil {
					return show()
				}
				fmt.Fprintf(env.Stderr, "viiwork top: %s: %v\n", node, m.Lost)
				return 1
			}
			apply(s, m, env.Now())
			if m.Cluster != nil && settle == nil {
				settle = time.After(onceSettle)
			}
			if settle != nil {
				quiet = time.After(onceQuiet)
			}
		case <-settle:
			return show()
		case <-quiet:
			return show()
		case err := <-errc:
			fmt.Fprintln(env.Stderr, failure(ctx, env, node, err))
			return 1
		case <-ctx.Done():
			fmt.Fprintf(env.Stderr, "viiwork top: no snapshot from %s within %s\n", node, onceTimeout)
			return 1
		}
	}
}

// failure explains why the stream ended for good. A node without the stream
// is named with its version, so the operator knows which node to upgrade.
func failure(ctx context.Context, env Env, node string, err error) string {
	if !errors.Is(err, stream.ErrNoStream) {
		return fmt.Sprintf("viiwork top: %s: %v", node, err)
	}
	ver := "unknown version"
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+node+meshapi.PathStatus, nil)
	if resp, err := env.Client.Do(req); err == nil {
		var st meshapi.NodeStatus
		if json.NewDecoder(resp.Body).Decode(&st) == nil && st.Ver != "" {
			ver = sanitize(st.Ver) // another node's text, printed on a normal terminal
		}
		resp.Body.Close()
	}
	return fmt.Sprintf("viiwork top: %s (%s) does not serve %s; it needs a newer viiwork", node, ver, meshapi.PathMeshStream)
}

func runLive(ctx context.Context, env Env, node string, v View) (code int) {
	scr, err := env.OpenScreen()
	if err != nil {
		fmt.Fprintf(env.Stderr, "viiwork top: %v\n", err)
		return 1
	}
	// Deferred calls run during a panic too, so this restores the terminal on
	// q, on cancel (SIGTERM) and on a crash alike. Restore is idempotent.
	defer scr.Restore()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	msgs, errc := follow(ctx, env, node)
	keys := make(chan []Key, 8)
	go readKeys(env.Keys, keys)
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	frames := time.NewTicker(frameEvery)
	defer frames.Stop()
	second := time.NewTicker(time.Second)
	defer second.Stop()

	s := NewState()
	color := ColorEnabled(env.LookupEnv)
	draw := func() {
		w, h, err := scr.Size()
		if err != nil {
			w, h = 80, 24
		}
		if v.Screen == Fleet {
			v.Cursor = min(v.Cursor, max(len(HostNames(s))-1, 0))
		}
		scr.Write(Paint(Render(s, v, w, h, env.Now()), color))
	}
	draw()
	dirty := false
	for {
		select {
		case <-ctx.Done():
			return 0
		case err := <-errc:
			scr.Restore()
			fmt.Fprintln(env.Stderr, failure(ctx, env, node, err))
			return 1
		case m := <-msgs:
			apply(s, m, env.Now())
			dirty = true
		case ks, ok := <-keys:
			if !ok {
				keys = nil // stdin closed: keep watching, stop reading keys
				continue
			}
			for _, k := range ks {
				if k == KeyQuit {
					return 0
				}
				v = press(v, k, HostNames(s))
			}
			draw() // a key press is answered at once, paused or not
			dirty = false
		case <-winch:
			dirty = true
		case <-second.C:
			s.Tick(env.Now())
			dirty = true // ages and rates move with the clock
		case <-frames.C:
			if dirty && !v.Paused {
				draw()
				dirty = false
			}
		}
	}
}

// press applies one key to the view.
func press(v View, k Key, names []string) View {
	switch k {
	case KeyUp:
		if v.Screen == Fleet && v.Cursor > 0 {
			v.Cursor--
		}
	case KeyDown:
		if v.Screen == Fleet && v.Cursor < len(names)-1 {
			v.Cursor++
		}
	case KeyEnter:
		if v.Screen == Fleet && v.Cursor < len(names) {
			v.Screen, v.Host = Host, names[v.Cursor]
		}
	case KeyEsc:
		v.Screen = Fleet
	case KeySort:
		v.Sort = v.Sort.Next()
	case KeyPause:
		v.Paused = !v.Paused
	}
	return v
}

func readKeys(r io.Reader, out chan<- []Key) {
	defer close(out)
	buf := make([]byte, 64)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if ks := ParseKeys(buf[:n]); len(ks) > 0 {
				out <- ks
			}
		}
		if err != nil {
			return
		}
	}
}
