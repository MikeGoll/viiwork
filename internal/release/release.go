// Package release is viiwork's signed release format: the archive names, the
// SHA256SUMS file and its ed25519 signature. The signing key never leaves the
// publisher's machine; everything else verifies against the public keys
// compiled in from keys/.
package release

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"strings"
)

// Target is one platform a release ships for.
type Target struct{ OS, Arch string }

// Targets is every platform a release ships for.
var Targets = []Target{{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "arm64"}}

var versionRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`)

// ValidVersion reports whether v names a release (vX.Y.Z with an optional
// pre-release suffix). A version is only ever a name: it never becomes a URL
// or a path without passing this.
func ValidVersion(v string) bool { return versionRe.MatchString(v) }

// ArchiveName is the release asset holding t's binaries.
func ArchiveName(version string, t Target) string {
	return fmt.Sprintf("viiwork_%s_%s_%s.tar.gz", version, t.OS, t.Arch)
}

// Sums is a parsed SHA256SUMS: asset name to sha256.
type Sums map[string][sha256.Size]byte

var sumLine = regexp.MustCompile(`^([0-9a-f]{64})  ([A-Za-z0-9._-]+)$`)

// ErrNotListed is an asset SHA256SUMS does not name.
var ErrNotListed = errors.New("not listed in SHA256SUMS")

// ParseSums reads a SHA256SUMS file in exactly the form `sha256sum` writes in
// text mode. Anything else is refused with its line number rather than
// half-read: this file decides what code a node will run.
func ParseSums(b []byte) (Sums, error) {
	if len(b) == 0 {
		return nil, errors.New("SHA256SUMS is empty")
	}
	if !bytes.HasSuffix(b, []byte("\n")) {
		return nil, errors.New("SHA256SUMS: the last line has no newline")
	}
	s := Sums{}
	for i, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		m := sumLine.FindStringSubmatch(line)
		if m == nil || strings.HasPrefix(m[2], ".") {
			return nil, fmt.Errorf("SHA256SUMS line %d: want \"<64 lowercase hex>  <name>\", got %q", i+1, line)
		}
		if _, dup := s[m[2]]; dup {
			return nil, fmt.Errorf("SHA256SUMS line %d: %s is listed twice", i+1, m[2])
		}
		var d [sha256.Size]byte
		if _, err := hex.Decode(d[:], []byte(m[1])); err != nil {
			return nil, fmt.Errorf("SHA256SUMS line %d: %v", i+1, err)
		}
		s[m[2]] = d
	}
	return s, nil
}

// Check reads r to the end and compares its sha256 with name's line.
func (s Sums) Check(name string, r io.Reader) error {
	want, ok := s[name]
	if !ok {
		return fmt.Errorf("%s: %w", name, ErrNotListed)
	}
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if !bytes.Equal(h.Sum(nil), want[:]) {
		return fmt.Errorf("%s: sha256 does not match SHA256SUMS", name)
	}
	return nil
}

// sigContext is prefixed to the signed message, so a signature this key made
// over anything else can never be passed off as a release signature.
const sigContext = "viiwork release SHA256SUMS v1\n"

// message is what is signed: the context, the version, then SHA256SUMS. The
// version is bound in so a signature for one release can never be replayed as
// another's, even over identical bytes.
func message(version string, sums []byte) []byte {
	return append([]byte(sigContext+version+"\n"), sums...)
}

// Sign is the content of SHA256SUMS.sig for version's sums.
func Sign(key ed25519.PrivateKey, version string, sums []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(key, message(version, sums))) + "\n")
}

// CheckSumsFor requires s to list exactly version's archives, one per target:
// no other version's name and no extra asset, because anything listed in a
// signed SHA256SUMS becomes something a node will accept.
func CheckSumsFor(version string, s Sums) error {
	if !ValidVersion(version) {
		return fmt.Errorf("not a release version: %q", version)
	}
	want := map[string]bool{}
	for _, t := range Targets {
		want[ArchiveName(version, t)] = true
	}
	for name := range s {
		if !want[name] {
			return fmt.Errorf("SHA256SUMS lists %s, which is not an archive of %s", name, version)
		}
	}
	for name := range want {
		if _, ok := s[name]; !ok {
			return fmt.Errorf("SHA256SUMS does not list %s", name)
		}
	}
	return nil
}

// ErrBadSignature is a well-formed signature that no trusted key made.
var ErrBadSignature = errors.New("SHA256SUMS signature does not verify against any release key")

// Verify checks sig over version's sums against every key; one match is
// enough, so a key can be rotated with an overlap.
func Verify(keys []ed25519.PublicKey, version string, sums, sig []byte) error {
	if len(keys) == 0 {
		return errors.New("no release public key is compiled in: every release is refused")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || len(raw) != ed25519.SignatureSize {
		return errors.New("SHA256SUMS.sig is not a base64 ed25519 signature")
	}
	for _, k := range keys {
		if ed25519.Verify(k, message(version, sums), raw) {
			return nil
		}
	}
	return ErrBadSignature
}

//go:embed keys
var keyFS embed.FS

// Keys is every public key compiled in from keys/*.pub.
func Keys() ([]ed25519.PublicKey, error) { return loadKeys(keyFS, "keys") }

func loadKeys(fsys fs.FS, dir string) ([]ed25519.PublicKey, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var keys []ed25519.PublicKey
	for _, e := range entries {
		if e.IsDir() || path.Ext(e.Name()) != ".pub" {
			continue
		}
		b, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		k, err := ParsePublicKey(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		keys = append(keys, k)
	}
	return keys, nil
}

// MarshalPublicKey is a .pub file: a comment line and the base64 key.
func MarshalPublicKey(k ed25519.PublicKey) []byte {
	return []byte("# viiwork release public key (ed25519)\n" + base64.StdEncoding.EncodeToString(k) + "\n")
}

// ParsePublicKey reads the first line of b that is neither blank nor a comment.
func ParsePublicKey(b []byte) (ed25519.PublicKey, error) {
	raw, err := keyLine(b, ed25519.PublicKeySize)
	if err != nil {
		return nil, err
	}
	return ed25519.PublicKey(raw), nil
}

// MarshalPrivateKey is a private key file: the base64 seed, nothing else.
func MarshalPrivateKey(k ed25519.PrivateKey) []byte {
	return []byte(base64.StdEncoding.EncodeToString(k.Seed()) + "\n")
}

// ParsePrivateKey reads a file written by MarshalPrivateKey.
func ParsePrivateKey(b []byte) (ed25519.PrivateKey, error) {
	seed, err := keyLine(b, ed25519.SeedSize)
	if err != nil {
		return nil, err
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func keyLine(b []byte, size int) ([]byte, error) {
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(line)
		if err != nil || len(raw) != size {
			return nil, fmt.Errorf("want a base64 %d-byte key", size)
		}
		return raw, nil
	}
	return nil, errors.New("no key in file")
}
