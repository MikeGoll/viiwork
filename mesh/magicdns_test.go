package mesh

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestReadMagicDNSSuffix(t *testing.T) {
	f := startLocalAPI(t, 200, `{"BackendState":"Running","MagicDNSSuffix":"tail1234.ts.net.","CurrentTailnet":{"MagicDNSSuffix":"other.ts.net"}}`)
	got, err := ReadMagicDNSSuffix(context.Background(), f.socket)
	if err != nil || got != "tail1234.ts.net" {
		t.Fatalf("ReadMagicDNSSuffix = %q, %v; want tail1234.ts.net (trailing dot trimmed)", got, err)
	}
}

func TestParseMagicDNSSuffix(t *testing.T) {
	cases := []struct {
		body, want string
		ok         bool
	}{
		{`{"MagicDNSSuffix":"tail1234.ts.net"}`, "tail1234.ts.net", true},
		{`{"CurrentTailnet":{"MagicDNSSuffix":"Tail-Abc.TS.net"}}`, "tail-abc.ts.net", true},
		{`{"MagicDNSSuffix":""}`, "", false},
		{`{}`, "", false},
		// Shared parents would let every tailnet's pages in: never ours.
		{`{"MagicDNSSuffix":"ts.net"}`, "", false},
		{`{"MagicDNSSuffix":"beta.tailscale.net"}`, "", false},
		{`{"MagicDNSSuffix":"localhost"}`, "", false},
		{`{"MagicDNSSuffix":"*.evil.test"}`, "", false},
	}
	for _, tc := range cases {
		got, err := parseMagicDNSSuffix([]byte(tc.body))
		if tc.ok && (err != nil || got != tc.want) {
			t.Errorf("%s: %q, %v; want %q", tc.body, got, err, tc.want)
		}
		if !tc.ok && !errors.Is(err, ErrNoMagicDNSSuffix) {
			t.Errorf("%s: %q, %v; want ErrNoMagicDNSSuffix", tc.body, got, err)
		}
	}
}

func TestReadMagicDNSSuffixNoDaemon(t *testing.T) {
	if _, err := ReadMagicDNSSuffix(context.Background(), filepath.Join(t.TempDir(), "absent.sock")); err == nil {
		t.Fatal("no tailscaled, no error")
	}
}
