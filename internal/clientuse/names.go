// Package clientuse names the client a request came from and measures how
// long each client held a slot. The energy store uses both to split a
// model's energy between the clients it served.
//
// A client is named by the address its connection came from and by nothing
// else: a header from a client is dropped, so a client cannot claim to be
// another. See docs/superpowers/specs/2026-10-09-client-energy-design.md.
package clientuse

import (
	"fmt"
	"net/netip"
	"sort"
)

// Other is the client of a request whose address matches no configured name,
// and of a request with no label on a node that is counting. It is reserved:
// no client can be configured under it.
const Other = "other"

const maxNameLen = 32

// ValidName reports whether s matches [a-z0-9][a-z0-9._-]{0,31}.
func ValidName(s string) bool {
	if len(s) == 0 || len(s) > maxNameLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case i > 0 && (c == '.' || c == '_' || c == '-'):
		default:
			return false
		}
	}
	return true
}

type entry struct {
	prefix netip.Prefix
	name   string
}

// Table maps a source address to a client name. A nil *Table names nobody:
// this node has no clients section.
type Table struct {
	entries []entry // most specific first
	names   int
}

// ParseTable builds the table of a clients section. Errors name the key the
// operator wrote. An empty section gives a nil table.
func ParseTable(clients map[string][]string) (*Table, error) {
	if len(clients) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(clients))
	for name := range clients {
		names = append(names, name)
	}
	sort.Strings(names) // the same error every time when two names collide
	owner := map[netip.Prefix]string{}
	t := &Table{names: len(names)}
	for _, name := range names {
		if name == Other {
			return nil, fmt.Errorf("clients.%s: the name %q is reserved", name, Other)
		}
		if !ValidName(name) {
			return nil, fmt.Errorf("clients.%s: a name must match [a-z0-9][a-z0-9._-]{0,31}", name)
		}
		if len(clients[name]) == 0 {
			return nil, fmt.Errorf("clients.%s: needs at least one address or prefix", name)
		}
		for i, raw := range clients[name] {
			p, err := parsePrefix(raw)
			if err != nil {
				return nil, fmt.Errorf("clients.%s[%d] %q must be an address or a prefix: %w", name, i, raw, err)
			}
			if prev, ok := owner[p]; ok {
				if prev != name {
					return nil, fmt.Errorf("clients.%s[%d]: %s is already listed under %s", name, i, p, prev)
				}
				continue
			}
			owner[p] = name
			t.entries = append(t.entries, entry{prefix: p, name: name})
		}
	}
	sort.SliceStable(t.entries, func(i, j int) bool { return t.entries[i].prefix.Bits() > t.entries[j].prefix.Bits() })
	return t, nil
}

// parsePrefix reads one address or prefix. An address is its own full-length
// prefix; a prefix written with host bits (100.64.1.9/24) is masked, so two
// spellings of one network compare equal.
func parsePrefix(raw string) (netip.Prefix, error) {
	if addr, err := netip.ParseAddr(raw); err == nil {
		addr = addr.Unmap()
		return netip.PrefixFrom(addr, addr.BitLen()), nil
	}
	p, err := netip.ParsePrefix(raw)
	if err != nil {
		return netip.Prefix{}, err
	}
	return p.Masked(), nil
}

// Lookup names the client at src: the most specific prefix that contains it,
// else Other. On a nil table it is "", which means "no label", not Other.
func (t *Table) Lookup(src netip.Addr) string {
	if t == nil {
		return ""
	}
	src = src.Unmap()
	for _, e := range t.entries {
		if e.prefix.Contains(src) {
			return e.name
		}
	}
	return Other
}

// Names is how many clients the table names.
func (t *Table) Names() int {
	if t == nil {
		return 0
	}
	return t.names
}
