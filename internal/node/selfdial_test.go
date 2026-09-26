package node

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/mesh/meshtest"
)

func TestSelfDialAddr(t *testing.T) {
	cases := []struct {
		bound *net.TCPAddr
		want  string
	}{
		{&net.TCPAddr{IP: net.IPv4zero, Port: 8086}, "127.0.0.1:8086"},
		{&net.TCPAddr{IP: net.IPv6unspecified, Port: 8086}, "[::1]:8086"},
		{&net.TCPAddr{IP: net.ParseIP("100.101.7.3"), Port: 8086}, "100.101.7.3:8086"},
		{&net.TCPAddr{IP: net.ParseIP("192.168.1.10"), Port: 18086}, "192.168.1.10:18086"},
		{&net.TCPAddr{IP: net.ParseIP("fd7a:115c:a1e0::5"), Port: 8086}, "[fd7a:115c:a1e0::5]:8086"},
	}
	for _, tc := range cases {
		if got := selfDialAddr(tc.bound); got != tc.want {
			t.Errorf("selfDialAddr(%v) = %q, want %q", tc.bound, got, tc.want)
		}
	}
}

func TestWildcardHost(t *testing.T) {
	for host, want := range map[string]bool{
		"": true, "0.0.0.0": true, "::": true, "[::]": false, // net.JoinHostPort brackets it; config holds "::"
		"127.0.0.1": false, "100.101.7.3": false, "gb1.local": false,
	} {
		if got := wildcardHost(host); got != want {
			t.Errorf("wildcardHost(%q) = %v, want %v", host, got, want)
		}
	}
}

// keepHostListen binds the configured host on an ephemeral port.
func keepHostListen(network, addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	return net.Listen(network, net.JoinHostPort(host, "0"))
}

// Pipelines call back into their own node. They used to dial a hard-coded
// 127.0.0.1:<port>, which refuses the connection when api.host is a specific
// address. 127.0.0.2 stands in for a tailnet IP: a real, non-127.0.0.1
// address the test can bind on Linux.
func TestPipelineSelfCallFollowsAPIHost(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skipf("cannot bind 127.0.0.2 here: %v", err)
	}
	probe.Close()

	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "viiwork.yaml")
	yaml := nodeConfig("pipe", state, nil, "pipelines:\n  tr:\n    locales:\n      fi:\n        language: Finnish\n    steps: []\n")
	yaml = strings.Replace(yaml, "host: 127.0.0.1", "host: 127.0.0.2", 1)
	writeFile(t, path, yaml)
	cfg, err := config.Load(path, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	n, err := New(cfg, Options{ConfigPath: path, Log: &logs, LookupEnv: noEnv, Listen: keepHostListen,
		MeshTune: meshTune(meshtest.NewNetwork(), "pipe", new(netip.AddrPort))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("node did not stop")
		}
	})
	select {
	case <-n.Ready():
	case err := <-done:
		t.Fatalf("Run: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("not ready")
	}

	if !strings.HasPrefix(n.APIAddr(), "127.0.0.2:") {
		t.Fatalf("bound %s, want 127.0.0.2", n.APIAddr())
	}
	_, port, _ := net.SplitHostPort(n.APIAddr())
	if c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second); err == nil {
		c.Close()
		t.Skip("127.0.0.1 reaches a 127.0.0.2 listener here; the test proves nothing on this host")
	}

	resp, err := n.selfClient().Get("http://" + selfHost + "/health")
	if err != nil {
		t.Fatalf("the pipeline client cannot reach its own node: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"node":"pipe"`) {
		t.Errorf("self call: %d %s", resp.StatusCode, body)
	}
	if strings.Contains(logs.String(), "every network interface") {
		t.Error("a specific api.host was warned about as a wildcard")
	}
}

// The warning is the only mitigation for the default bind: it must name the
// risk and the knob.
func TestWildcardBindWarns(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "viiwork.yaml")
	writeFile(t, path, strings.Replace(nodeConfig("wild", state, nil, ""), "host: 127.0.0.1", "host: 0.0.0.0", 1))
	cfg, err := config.Load(path, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	n, err := New(cfg, Options{ConfigPath: path, Log: &logs, LookupEnv: noEnv, Listen: loopbackListen,
		MeshTune: meshTune(meshtest.NewNetwork(), "wild", new(netip.AddrPort))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx) }()
	select {
	case <-n.Ready():
	case err := <-done:
		t.Fatalf("Run: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("not ready")
	}
	cancel()
	<-done
	log := logs.String()
	if !strings.Contains(log, "every network interface") || !strings.Contains(log, "authenticates nothing") || !strings.Contains(log, "api.host") {
		t.Errorf("no clear wildcard warning: %q", log)
	}
}
