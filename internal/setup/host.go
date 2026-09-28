package setup

import (
	"context"
	"crypto/rand"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"os/user"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/janit/viiwork/v2/internal/accept"
	"github.com/janit/viiwork/v2/internal/gpu"
	"github.com/janit/viiwork/v2/internal/setup/discover"
	"github.com/janit/viiwork/v2/internal/setup/install"
	"github.com/janit/viiwork/v2/mesh"
)

// DefaultHost is the machine this process runs on.
func DefaultHost(version, llamaPin string, out io.Writer) Host {
	exe, _ := os.Executable()
	home, _ := os.UserHomeDir()
	if name := os.Getenv("SUDO_USER"); name != "" {
		if u, err := user.Lookup(name); err == nil {
			home = u.HomeDir
		}
	}
	env := accept.DefaultEnv()
	return Host{
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Version: version, Home: home, Euid: os.Geteuid(),
		Hostname: os.Hostname, Run: gpu.ExecRunner, Exec: install.ExecCommand, Sys: os.DirFS("/sys"),
		PortFree: portFree,
		Discover: func(ctx context.Context) []discover.Node {
			return discover.Find(ctx, discover.Sources{Tailnet: tailnetPeers, LAN: lanPeers, APIPort: 8086, HTTP: env.HTTP}, 3*time.Second)
		},
		Tailnet: func(ctx context.Context) bool {
			st, err := mesh.ReadTailnetStatus(ctx, mesh.DefaultTailnetSocket)
			if err != nil {
				return false
			}
			_, ok := st.SelfIPv4()
			return ok
		},
		Rand: rand.Reader, Executable: exe, HTTP: env.HTTP, Accept: env,
		NodeAPI: "127.0.0.1:8086", PeerAPIPort: 8086, Out: out, ReadyTimeout: 45 * time.Minute, UpTimeout: 2 * time.Minute, Poll: 2 * time.Second,
		UID: os.Getuid(), LlamaPin: llamaPin, LlamaAPI: install.LlamaReleaseAPI,
		// Downloads from GitHub honour HTTPS_PROXY; the node-facing client
		// above never uses a proxy.
		Download: downloadClient(),
		Path:     os.Getenv("PATH"),
		Signals: func(ctx context.Context) (context.Context, context.CancelFunc) {
			return signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		},
	}
}

func portFree(network string, port int) bool {
	addr := ":" + strconv.Itoa(port)
	if network == "udp" {
		c, err := net.ListenPacket("udp", addr)
		if err != nil {
			return false
		}
		c.Close()
		return true
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	l.Close()
	return true
}

func tailnetPeers(ctx context.Context) ([]netip.Addr, error) {
	st, err := mesh.ReadTailnetStatus(ctx, mesh.DefaultTailnetSocket)
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, p := range st.Peers {
		if !p.Online {
			continue
		}
		for _, ip := range p.IPs {
			if ip.Is4() {
				out = append(out, ip)
				break
			}
		}
	}
	return out, nil
}

func lanPeers(ctx context.Context) ([]string, error) {
	self, err := mesh.LANAddress()
	if err != nil {
		return nil, err
	}
	f, err := mesh.MDNSFeeder(self, log.New(io.Discard, "", 0))
	if err != nil {
		return nil, err
	}
	return f.Candidates(ctx)
}

// downloadClient fetches from GitHub: through HTTPS_PROXY when one is set,
// and never waiting forever for a server that accepted the connection but
// sends nothing (install.Fetch watches the body the same way).
func downloadClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = time.Minute
	return &http.Client{Transport: tr}
}
