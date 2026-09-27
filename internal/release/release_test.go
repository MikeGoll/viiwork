package release

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
)

func TestNames(t *testing.T) {
	if got := ArchiveName("v2.6.0", Target{"linux", "amd64"}); got != "viiwork_v2.6.0_linux_amd64.tar.gz" {
		t.Errorf("ArchiveName = %q", got)
	}
	if len(Targets) != 3 {
		t.Errorf("targets = %v", Targets)
	}
	for v, want := range map[string]bool{
		"v2.6.0": true, "v2.6.0-rc.1": true, "v2.0.0-beta1": true,
		"2.6.0": false, "v2.6": false, "v2.6.0-g1a2b3c": true, "v2.6.0 ": false, "v2.6.0/x": false, "": false,
	} {
		if ValidVersion(v) != want {
			t.Errorf("ValidVersion(%q) = %v", v, !want)
		}
	}
}

func sumOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestParseSums(t *testing.T) {
	good := sumOf("a") + "  viiwork_v2.6.0_linux_amd64.tar.gz\n" + sumOf("b") + "  viiwork_v2.6.0_darwin_arm64.tar.gz\n"
	s, err := ParseSums([]byte(good))
	if err != nil || len(s) != 2 {
		t.Fatalf("ParseSums(good) = %v, %v", s, err)
	}
	if err := s.Check("viiwork_v2.6.0_linux_amd64.tar.gz", strings.NewReader("a")); err != nil {
		t.Errorf("Check(match) = %v", err)
	}
	if err := s.Check("viiwork_v2.6.0_linux_amd64.tar.gz", strings.NewReader("tampered")); err == nil {
		t.Error("Check(mismatch) = nil")
	}
	if err := s.Check("other.tar.gz", strings.NewReader("a")); !errors.Is(err, ErrNotListed) {
		t.Errorf("Check(unlisted) = %v", err)
	}

	h := sumOf("a")
	bad := map[string]string{
		"empty":             "",
		"no final newline":  h + "  x.tar.gz",
		"crlf":              h + "  x.tar.gz\r\n",
		"uppercase hex":     strings.ToUpper(h) + "  x.tar.gz\n",
		"binary marker":     h + " *x.tar.gz\n",
		"one space":         h + " x.tar.gz\n",
		"short digest":      h[:60] + "  x.tar.gz\n",
		"duplicate":         h + "  x.tar.gz\n" + h + "  x.tar.gz\n",
		"slash in name":     h + "  a/x.tar.gz\n",
		"dot name":          h + "  ..\n",
		"hidden name":       h + "  .x\n",
		"blank line inside": h + "  x.tar.gz\n\n" + h + "  y.tar.gz\n",
	}
	for name, in := range bad {
		if _, err := ParseSums([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ParseSums([]byte(h + "  x.tar.gz\n" + "junk\n")); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error does not name the line: %v", err)
	}
}

func TestSignVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	sums := []byte(sumOf("a") + "  x.tar.gz\n")
	sig := Sign(priv, "v2.6.0", sums)
	if !bytes.HasSuffix(sig, []byte("\n")) {
		t.Error("signature file is not newline-terminated")
	}
	if err := Verify([]ed25519.PublicKey{pub}, "v2.6.0", sums, sig); err != nil {
		t.Fatalf("Verify = %v", err)
	}
	// Rotation: an old and a new key are both accepted.
	if err := Verify([]ed25519.PublicKey{other, pub}, "v2.6.0", sums, sig); err != nil {
		t.Errorf("Verify with two keys = %v", err)
	}
	tampered := bytes.Replace(sums, []byte("x.tar"), []byte("y.tar"), 1)
	if err := Verify([]ed25519.PublicKey{pub}, "v2.6.0", tampered, sig); !errors.Is(err, ErrBadSignature) {
		t.Errorf("tampered sums: %v", err)
	}
	if err := Verify([]ed25519.PublicKey{other}, "v2.6.0", sums, sig); !errors.Is(err, ErrBadSignature) {
		t.Errorf("wrong key: %v", err)
	}
	if err := Verify([]ed25519.PublicKey{pub}, "v2.6.0", sums, []byte("not base64!\n")); err == nil || errors.Is(err, ErrBadSignature) {
		t.Errorf("garbage signature: %v", err)
	}
	if err := Verify(nil, "v2.6.0", sums, sig); err == nil || !strings.Contains(err.Error(), "no release public key") {
		t.Errorf("no keys: %v", err)
	}
	// A plain ed25519 signature over the bare file does not verify: the
	// signed message carries a context string, so a signature made for
	// anything else with this key cannot be replayed as a release signature.
	bare := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, sums)) + "\n"
	if err := Verify([]ed25519.PublicKey{pub}, "v2.6.0", sums, []byte(bare)); !errors.Is(err, ErrBadSignature) {
		t.Errorf("a context-free signature: %v; want ErrBadSignature", err)
	}
	// The version is signed too: a signature for one release cannot be
	// replayed as another release's, even with identical SHA256SUMS bytes.
	if err := Verify([]ed25519.PublicKey{pub}, "v9.9.9", sums, sig); !errors.Is(err, ErrBadSignature) {
		t.Errorf("replayed under another version: %v; want ErrBadSignature", err)
	}
}

// A release's SHA256SUMS lists exactly its own archives: one per target, named
// for its version. Anything else — another version's name, an extra asset, a
// missing target — must never be signed or accepted.
func TestSumsForVersion(t *testing.T) {
	line := func(name string) string { return sumOf(name) + "  " + name + "\n" }
	var exact string
	for _, tg := range Targets {
		exact += line(ArchiveName("v2.6.0", tg))
	}
	parse := func(in string) Sums {
		s, err := ParseSums([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if err := CheckSumsFor("v2.6.0", parse(exact)); err != nil {
		t.Fatalf("exact set: %v", err)
	}
	cases := map[string]string{
		"another version's archive": exact + line(ArchiveName("v9.9.9", Targets[0])),
		"extra asset":               exact + line("notes.txt"),
		"missing target":            line(ArchiveName("v2.6.0", Targets[0])) + line(ArchiveName("v2.6.0", Targets[1])),
		"wrong version throughout":  strings.ReplaceAll(exact, "v2.6.0", "v2.6.1"),
	}
	for name, in := range cases {
		if err := CheckSumsFor("v2.6.0", parse(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := CheckSumsFor("2.6.0", parse(exact)); err == nil {
		t.Error("an invalid version was accepted")
	}
}

func TestKeyFiles(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	p, err := ParsePublicKey(MarshalPublicKey(pub))
	if err != nil || !p.Equal(pub) {
		t.Fatalf("public round trip: %v", err)
	}
	k, err := ParsePrivateKey(MarshalPrivateKey(priv))
	if err != nil || !k.Equal(priv) {
		t.Fatalf("private round trip: %v", err)
	}
	if _, err := ParsePublicKey([]byte("# only a comment\n")); err == nil {
		t.Error("empty public key accepted")
	}
	if _, err := ParsePublicKey([]byte("AAAA\n")); err == nil {
		t.Error("short public key accepted")
	}
	if _, err := ParsePrivateKey([]byte("AAAA\n")); err == nil {
		t.Error("short private key accepted")
	}
}

func TestLoadKeys(t *testing.T) {
	a, _, _ := ed25519.GenerateKey(rand.Reader)
	b, _, _ := ed25519.GenerateKey(rand.Reader)
	fsys := fstest.MapFS{
		"keys/README.md":  {Data: []byte("not a key")},
		"keys/2026.pub":   {Data: MarshalPublicKey(a)},
		"keys/2027.pub":   {Data: MarshalPublicKey(b)},
		"keys/broken.txt": {Data: []byte("ignored")},
	}
	keys, err := loadKeys(fsys, "keys")
	if err != nil || len(keys) != 2 {
		t.Fatalf("loadKeys = %d keys, %v", len(keys), err)
	}
	fsys["keys/bad.pub"] = &fstest.MapFile{Data: []byte("AAAA\n")}
	if _, err := loadKeys(fsys, "keys"); err == nil || !strings.Contains(err.Error(), "bad.pub") {
		t.Errorf("a broken .pub must fail and be named: %v", err)
	}
	if _, err := Keys(); err != nil {
		t.Errorf("embedded keys: %v", err)
	}
}
