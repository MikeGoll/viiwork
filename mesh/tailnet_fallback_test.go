package mesh

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func darwinCLIFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/tailnet-status-darwin-cli.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// The App Store Tailscale's CLI prints the same ipnstate.Status the LocalAPI
// returns, so both readers decode it unchanged.
func TestDecodeDarwinCLIStatus(t *testing.T) {
	st, err := decodeTailnetStatus(darwinCLIFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if ip, ok := st.SelfIPv4(); !ok || ip != netip.MustParseAddr("100.64.0.200") || st.Self.HostName != "mac-a" {
		t.Errorf("self = %+v", st.Self)
	}
	if len(st.Peers) != 2 || st.Peers[0].HostName != "node-a" || !st.Peers[0].Online || st.Peers[1].Online {
		t.Errorf("peers = %+v", st.Peers)
	}
	// MagicDNSSuffix is at the top level and under CurrentTailnet; the top
	// level is read first.
	if got, err := parseMagicDNSSuffix(darwinCLIFixture(t)); err != nil || got != "example.ts.net" {
		t.Errorf("suffix = %q, %v", got, err)
	}
}

func withStatusFallback(t *testing.T, fb func(context.Context) ([]byte, error)) *int {
	t.Helper()
	calls := 0
	old := statusFallback
	statusFallback = func(ctx context.Context) ([]byte, error) { calls++; return fb(ctx) }
	t.Cleanup(func() { statusFallback = old })
	return &calls
}

func TestStatusFallbackOnlyForTheDefaultSocket(t *testing.T) {
	cli := darwinCLIFixture(t)
	calls := withStatusFallback(t, func(context.Context) ([]byte, error) { return cli, nil })
	dead := filepath.Join(t.TempDir(), "absent.sock")

	// An explicitly configured socket is never second-guessed.
	if _, err := readStatusDocument(context.Background(), dead, false); err == nil || *calls != 0 {
		t.Errorf("explicit socket: err=%v, fallback calls=%d", err, *calls)
	}
	// The default one falls back when nothing listens on it.
	body, err := readStatusDocument(context.Background(), dead, true)
	if err != nil || *calls != 1 || string(body) != string(cli) {
		t.Errorf("default socket: err=%v, fallback calls=%d", err, *calls)
	}
	// A daemon that answers, even with an error, is not replaced by another.
	api := startLocalAPI(t, 500, "boom")
	if _, err := readStatusDocument(context.Background(), api.socket, true); err == nil || *calls != 1 {
		t.Errorf("answering daemon: err=%v, fallback calls=%d", err, *calls)
	}
}

func TestStatusFallbackFailureKeepsBothReasons(t *testing.T) {
	withStatusFallback(t, func(context.Context) ([]byte, error) { return nil, errors.New("no tailscale CLI") })
	_, err := readStatusDocument(context.Background(), filepath.Join(t.TempDir(), "absent.sock"), true)
	if err == nil || !strings.Contains(err.Error(), "absent.sock") || !strings.Contains(err.Error(), "no tailscale CLI") {
		t.Errorf("err = %v", err)
	}
	withStatusFallback(t, func(context.Context) ([]byte, error) { return nil, errNoStatusFallback })
	_, err = readStatusDocument(context.Background(), filepath.Join(t.TempDir(), "absent.sock"), true)
	if err == nil || strings.Contains(err.Error(), errNoStatusFallback.Error()) {
		t.Errorf("a platform without a fallback must report only the socket: %v", err)
	}
}

// The darwin chain: Homebrew's tailscaled socket, then a tailscale CLI on
// PATH, then the one inside the app bundle (the App Store build has no socket
// and puts nothing on PATH).
func TestDarwinStatusChain(t *testing.T) {
	cli := darwinCLIFixture(t)
	type step struct{ sockets, runs []string }
	cases := []struct {
		name     string
		socketOK bool
		onPath   bool
		appOK    bool
		want     step
		wantErr  bool
	}{
		{name: "homebrew socket", socketOK: true, want: step{sockets: []string{darwinBrewSocket}}},
		{name: "cli on path", onPath: true, want: step{sockets: []string{darwinBrewSocket}, runs: []string{"/opt/homebrew/bin/tailscale"}}},
		{name: "app store", appOK: true, want: step{sockets: []string{darwinBrewSocket}, runs: []string{darwinAppCLI}}},
		{name: "nothing", wantErr: true, want: step{sockets: []string{darwinBrewSocket}, runs: []string{darwinAppCLI}}},
	}
	for _, tc := range cases {
		var got step
		src := darwinStatusSource{
			fetch: func(_ context.Context, socket string) ([]byte, error) {
				got.sockets = append(got.sockets, socket)
				if tc.socketOK {
					return cli, nil
				}
				return nil, errors.New("no socket")
			},
			lookPath: func(string) (string, error) {
				if tc.onPath {
					return "/opt/homebrew/bin/tailscale", nil
				}
				return "", errors.New("not found")
			},
			run: func(_ context.Context, name string, args ...string) ([]byte, error) {
				got.runs = append(got.runs, name)
				if !slices.Equal(args, []string{"status", "--json"}) {
					t.Errorf("%s: args %v", tc.name, args)
				}
				if tc.onPath || (tc.appOK && name == darwinAppCLI) {
					return cli, nil
				}
				return nil, errors.New("no such file")
			},
		}
		body, err := src.read(context.Background())
		if (err != nil) != tc.wantErr || (!tc.wantErr && string(body) != string(cli)) {
			t.Errorf("%s: err=%v", tc.name, err)
		}
		if !slices.Equal(got.sockets, tc.want.sockets) || !slices.Equal(got.runs, tc.want.runs) {
			t.Errorf("%s: tried sockets %v runs %v, want %+v", tc.name, got.sockets, got.runs, tc.want)
		}
	}
}
