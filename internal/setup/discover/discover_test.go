package discover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

// serve answers /v1/status on addr. One port on several loopback addresses
// stands in for one API port on several hosts. block makes it hang until the
// request is cancelled.
func serve(t *testing.T, addr string, st meshapi.NodeStatus, block bool) {
	t.Helper()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("cannot listen on %s: %v", addr, err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if block {
			<-r.Context().Done()
			return
		}
		if r.URL.Path != meshapi.PathStatus {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(st)
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestFindListsNodesFromBothSources(t *testing.T) {
	port := freePort(t)
	at := func(ip string) string { return fmt.Sprintf("%s:%d", ip, port) }
	serve(t, at("127.0.0.1"), meshapi.NodeStatus{Node: "node-b", Ver: "v2.6.0", Models: []meshapi.ModelStatus{{Name: "m1"}}}, false)
	serve(t, at("127.0.0.2"), meshapi.NodeStatus{Node: "node-a", Ver: "v2.6.0"}, false)
	serve(t, at("127.0.0.4"), meshapi.NodeStatus{}, true)                               // hangs
	serve(t, at("127.0.0.5"), meshapi.NodeStatus{Node: "node-b", Ver: "v2.6.0"}, false) // the same node by a second address
	s := Sources{
		Tailnet: func(context.Context) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("127.0.0.3"), netip.MustParseAddr("127.0.0.4"), netip.MustParseAddr("127.0.0.5")}, nil
		},
		LAN: func(context.Context) ([]string, error) {
			return []string{"127.0.0.2:7946", "127.0.0.1:7946", "garbage"}, nil
		},
		APIPort: port,
		HTTP:    &http.Client{},
	}
	start := time.Now()
	got := Find(context.Background(), s, 600*time.Millisecond)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v with a 600ms budget", d)
	}
	want := []Node{
		{Name: "node-a", Version: "v2.6.0", API: at("127.0.0.2")},
		{Name: "node-b", Version: "v2.6.0", API: at("127.0.0.1"), Models: []string{"m1"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestFindNothingIsNormal(t *testing.T) {
	s := Sources{
		Tailnet: func(context.Context) ([]netip.Addr, error) { return nil, errors.New("no tailscaled") },
		APIPort: freePort(t),
		HTTP:    &http.Client{},
	}
	if got := Find(context.Background(), s, 200*time.Millisecond); len(got) != 0 {
		t.Errorf("found %+v", got)
	}
}
