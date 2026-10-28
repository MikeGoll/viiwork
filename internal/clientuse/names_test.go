package clientuse

import (
	"net/netip"
	"strings"
	"testing"
)

func TestValidName(t *testing.T) {
	for _, ok := range []string{"a", "factory-a", "batch.b_2", "0x", strings.Repeat("a", 32)} {
		if !ValidName(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "-a", ".a", "Factory", "a b", "a/b", "é", strings.Repeat("a", 33)} {
		if ValidName(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestLookup(t *testing.T) {
	table, err := ParseTable(map[string][]string{
		"factory-a": {"100.64.0.10"},
		"batch-b":   {"100.64.0.20", "100.64.1.0/24", "fd7a::/64"},
		"lab":       {"100.64.1.5", "100.64.3.77/24"}, // host bits are masked off
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"100.64.0.10":        "factory-a",
		"100.64.0.20":        "batch-b",
		"100.64.1.9":         "batch-b",
		"100.64.1.5":         "lab", // the more specific of two prefixes
		"100.64.3.9":         "lab",
		"::ffff:100.64.0.10": "factory-a",
		"fd7a::1":            "batch-b",
		"100.64.2.1":         Other,
		"fd7b::1":            Other,
	}
	for addr, want := range cases {
		if got := table.Lookup(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Lookup(%s) = %q, want %q", addr, got, want)
		}
	}
	if table.Names() != 3 {
		t.Errorf("Names() = %d, want 3", table.Names())
	}
}

func TestNoTableNamesNobody(t *testing.T) {
	for _, in := range []map[string][]string{nil, {}} {
		table, err := ParseTable(in)
		if err != nil || table != nil {
			t.Fatalf("ParseTable(%v) = %v, %v; want nil, nil", in, table, err)
		}
	}
	var table *Table
	if got := table.Lookup(netip.MustParseAddr("100.64.0.10")); got != "" {
		t.Errorf("a nil table named %q", got)
	}
	if table.Names() != 0 {
		t.Error("a nil table has names")
	}
}

func TestParseTableErrors(t *testing.T) {
	cases := []struct {
		name string
		in   map[string][]string
		want string
	}{
		{"reserved", map[string][]string{"other": {"100.64.0.1"}}, `clients.other: the name "other" is reserved`},
		{"bad name", map[string][]string{"Factory": {"100.64.0.1"}}, "clients.Factory: a name must match"},
		{"no address", map[string][]string{"a": {}}, "clients.a: needs at least one address or prefix"},
		{"not an address", map[string][]string{"a": {"gb1"}}, `clients.a[0] "gb1" must be an address or a prefix`},
		{"same address twice", map[string][]string{"a": {"100.64.0.1"}, "b": {"100.64.0.1"}}, "clients.b[0]: 100.64.0.1/32 is already listed under a"},
		{"same prefix after masking", map[string][]string{"a": {"100.64.1.0/24"}, "b": {"100.64.1.9/24"}}, "clients.b[0]: 100.64.1.0/24 is already listed under a"},
	}
	for _, tc := range cases {
		_, err := ParseTable(tc.in)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to contain %q", tc.name, err, tc.want)
		}
	}
	// The same prefix twice under one name is a repeat, not a conflict.
	if _, err := ParseTable(map[string][]string{"a": {"100.64.0.1", "100.64.0.1/32"}}); err != nil {
		t.Errorf("a repeat under one name: %v", err)
	}
}
