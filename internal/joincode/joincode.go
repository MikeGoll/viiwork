// Package joincode is the one string a new machine needs to join a mesh: the
// mesh secret (for a secured mesh) and one member's gossip address as a seed,
// with a checksum so a mangled paste fails clearly instead of joining nothing.
// It is a client-side encoding of values the mesh already uses, not a wire
// format between nodes. A code IS the mesh secret: handle it like mesh.env.
package joincode

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"net/netip"
	"strings"
)

const (
	prefix     = "viiwork1-"
	version    = 1
	flagSecure = 1
	secretLen  = 32
)

var (
	ErrVersion = errors.New("unknown join code version")
	ErrDamaged = errors.New("join code damaged — copy it again")
)

// Code is what a join code carries. Secret is nil for an open mesh.
type Code struct {
	Secret []byte
	Seed   string // gossip ip:port
}

// Encode spells c. The seed must be ip:port, as mesh.seeds requires.
func Encode(c Code) (string, error) {
	if _, err := netip.ParseAddrPort(c.Seed); err != nil {
		return "", fmt.Errorf("seed %q must be ip:port", c.Seed)
	}
	if c.Secret != nil && len(c.Secret) != secretLen {
		return "", fmt.Errorf("a mesh secret is %d bytes, not %d", secretLen, len(c.Secret))
	}
	var b bytes.Buffer
	b.WriteByte(version)
	if c.Secret != nil {
		b.WriteByte(flagSecure)
		b.Write(c.Secret)
	} else {
		b.WriteByte(0)
	}
	b.WriteByte(byte(len(c.Seed)))
	b.WriteString(c.Seed)
	binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(b.Bytes()))
	return prefix + base64.RawURLEncoding.EncodeToString(b.Bytes()), nil
}

// Decode reads a code, tolerating whitespace and line breaks around and
// inside it (a paste from a terminal).
func Decode(s string) (Code, error) {
	s = strings.Join(strings.Fields(s), "")
	rest, ok := strings.CutPrefix(s, prefix)
	if !ok {
		return Code{}, ErrVersion
	}
	raw, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil || len(raw) < 1+1+1+4 {
		return Code{}, ErrDamaged
	}
	body, sum := raw[:len(raw)-4], raw[len(raw)-4:]
	if crc32.ChecksumIEEE(body) != binary.BigEndian.Uint32(sum) {
		return Code{}, ErrDamaged
	}
	if body[0] != version {
		return Code{}, ErrVersion
	}
	var c Code
	p := body[2:]
	if body[1]&flagSecure != 0 {
		if len(p) < secretLen {
			return Code{}, ErrDamaged
		}
		c.Secret, p = append([]byte(nil), p[:secretLen]...), p[secretLen:]
	}
	if len(p) < 1 || int(p[0]) != len(p)-1 {
		return Code{}, ErrDamaged
	}
	c.Seed = string(p[1:])
	if _, err := netip.ParseAddrPort(c.Seed); err != nil {
		return Code{}, ErrDamaged
	}
	return c, nil
}
