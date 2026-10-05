package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/janit/viiwork/v2/internal/release"
)

func runCmd(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestKeygenSignVerify(t *testing.T) {
	dir := t.TempDir()
	key, pub := filepath.Join(dir, "cfg", "release.key"), filepath.Join(dir, "release.pub")
	if code, _, e := runCmd("keygen", "--key", key, "--pub", pub); code != 0 {
		t.Fatalf("keygen: %d %s", code, e)
	}
	if fi, err := os.Stat(key); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode = %v, %v", fi.Mode(), err)
	}
	if code, _, e := runCmd("keygen", "--key", key, "--pub", pub); code != 1 || !strings.Contains(e, "refusing") {
		t.Fatalf("second keygen: %d %s", code, e)
	}

	sums := filepath.Join(dir, "SHA256SUMS")
	os.WriteFile(sums, releaseSums("v2.6.0", "a"), 0o644)
	if code, _, e := runCmd("sign", "--key", key, sums); code != 2 {
		t.Fatalf("sign without --version: %d %s", code, e)
	}
	if code, _, e := runCmd("sign", "--key", key, "--version", "v2.6.0", sums); code != 0 {
		t.Fatalf("sign: %d %s", code, e)
	}
	if code, _, e := runCmd("verify", "--pub", pub, "--version", "v2.6.0", sums, sums+".sig"); code != 0 {
		t.Fatalf("verify: %d %s", code, e)
	}
	if code, _, e := runCmd("verify", "--pub", pub, "--version", "v2.6.1", sums, sums+".sig"); code != 1 {
		t.Fatalf("verify under another version: %d %s", code, e)
	}
	os.WriteFile(sums, releaseSums("v2.6.0", "b"), 0o644)
	if code, _, e := runCmd("verify", "--pub", pub, "--version", "v2.6.0", sums, sums+".sig"); code != 1 {
		t.Fatalf("verify of tampered sums: %d %s", code, e)
	}
	// A SHA256SUMS that smuggles in another version's archive is never signed.
	smuggled := append(releaseSums("v2.6.0", "a"), []byte(strings.Repeat("c", 64)+"  viiwork_v9.9.9_linux_amd64.tar.gz\n")...)
	os.WriteFile(sums, smuggled, 0o644)
	if code, _, e := runCmd("sign", "--key", key, "--version", "v2.6.0", sums); code != 1 || !strings.Contains(e, "v9.9.9") {
		t.Fatalf("signed a smuggled archive: %d %s", code, e)
	}
}

func TestSignRefusesAnExposedKey(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "release.key")
	if code, _, e := runCmd("keygen", "--key", key, "--pub", filepath.Join(dir, "p.pub")); code != 0 {
		t.Fatal(e)
	}
	os.Chmod(key, 0o644)
	sums := filepath.Join(dir, "SHA256SUMS")
	os.WriteFile(sums, releaseSums("v2.6.0", "a"), 0o644)
	if code, _, e := runCmd("sign", "--key", key, "--version", "v2.6.0", sums); code != 1 || !strings.Contains(e, "chmod 600") {
		t.Fatalf("sign with a 0644 key: %d %s", code, e)
	}
}

func TestSignRefusesMalformedSums(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "release.key")
	runCmd("keygen", "--key", key, "--pub", filepath.Join(dir, "p.pub"))
	sums := filepath.Join(dir, "SHA256SUMS")
	os.WriteFile(sums, []byte("not a sums file\n"), 0o644)
	if code, _, _ := runCmd("sign", "--key", key, "--version", "v2.6.0", sums); code != 1 {
		t.Fatalf("signed a malformed SHA256SUMS: %d", code)
	}
}

// releaseSums is a well-formed SHA256SUMS for every target of version; fill
// varies the digests.
func releaseSums(version, fill string) []byte {
	var b strings.Builder
	for _, t := range release.Targets {
		b.WriteString(strings.Repeat(fill, 64) + "  " + release.ArchiveName(version, t) + "\n")
	}
	return []byte(b.String())
}

type entry struct {
	name, body string
	typ        byte
	mode       int64
}

func writeArchive(t *testing.T, path string, entries []entry) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o755
		}
		h := &tar.Header{Name: e.name, Mode: mode, Size: int64(len(e.body)), Typeflag: typ}
		if typ == tar.TypeDir || typ == tar.TypeSymlink {
			h.Size = 0
			h.Linkname = e.body
		}
		tw.WriteHeader(h)
		if typ == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	gz.Close()
	os.WriteFile(path, buf.Bytes(), 0o644)
}

func TestCompare(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "viiwork_v2.6.0_linux_amd64")
	os.MkdirAll(local, 0o755)
	for n, b := range map[string]string{"viiwork": "BIN", "viiwork-accept": "ACC", "LICENSE": "MIT"} {
		os.WriteFile(filepath.Join(local, n), []byte(b), 0o644)
	}
	top := "viiwork_v2.6.0_linux_amd64/"
	same := []entry{{name: top, typ: tar.TypeDir}, {name: top + "LICENSE", body: "MIT"}, {name: top + "viiwork", body: "BIN"}, {name: top + "viiwork-accept", body: "ACC"}}
	arch := filepath.Join(dir, "a.tar.gz")

	writeArchive(t, arch, same)
	if code, _, e := runCmd("compare", arch, local); code != 0 {
		t.Fatalf("identical: %d %s", code, e)
	}

	flipped := append([]entry{}, same...)
	flipped[2] = entry{name: top + "viiwork", body: "BIX"}
	writeArchive(t, arch, flipped)
	if code, _, e := runCmd("compare", arch, local); code != 1 || !strings.Contains(e, "viiwork: differs") {
		t.Fatalf("flipped byte: %d %s", code, e)
	}

	writeArchive(t, arch, append(append([]entry{}, same...), entry{name: top + "extra", body: "x"}))
	if code, _, e := runCmd("compare", arch, local); code != 1 || !strings.Contains(e, "extra") {
		t.Fatalf("extra file: %d %s", code, e)
	}

	writeArchive(t, arch, same[:3])
	if code, _, e := runCmd("compare", arch, local); code != 1 || !strings.Contains(e, "missing from archive: viiwork-accept") {
		t.Fatalf("missing file: %d %s", code, e)
	}

	writeArchive(t, arch, append(append([]entry{}, same...), entry{name: top + "link", body: "/etc/passwd", typ: tar.TypeSymlink}))
	if code, _, e := runCmd("compare", arch, local); code != 1 || !strings.Contains(e, "link") {
		t.Fatalf("symlink: %d %s", code, e)
	}

	writeArchive(t, arch, append(append([]entry{}, same...), entry{name: top + "../escape", body: "x"}))
	if code, _, _ := runCmd("compare", arch, local); code != 1 {
		t.Fatalf("path escaping the top directory accepted")
	}

	// A second viiwork entry: an extractor that stops at the first match would
	// run bytes compare never looked at.
	writeArchive(t, arch, append([]entry{{name: top + "viiwork", body: "EVIL"}}, same...))
	if code, _, e := runCmd("compare", arch, local); code != 1 || !strings.Contains(e, "twice") {
		t.Fatalf("duplicate entry: %d %s", code, e)
	}

	// The top directory is the release's own name, not anything.
	other := []entry{{name: "anything/", typ: tar.TypeDir}, {name: "anything/LICENSE", body: "MIT"}, {name: "anything/viiwork", body: "BIN"}, {name: "anything/viiwork-accept", body: "ACC"}}
	writeArchive(t, arch, other)
	if code, _, e := runCmd("compare", arch, local); code != 1 || !strings.Contains(e, "anything") {
		t.Fatalf("wrong top directory: %d %s", code, e)
	}

	// setuid, setgid and sticky bits have no place in a release.
	setuid := append([]entry{}, same...)
	setuid[2] = entry{name: top + "viiwork", body: "BIN", mode: 0o4755}
	writeArchive(t, arch, setuid)
	if code, _, e := runCmd("compare", arch, local); code != 1 || !strings.Contains(e, "mode") {
		t.Fatalf("setuid binary: %d %s", code, e)
	}
}

func TestUsage(t *testing.T) {
	if code, _, e := runCmd("frobnicate"); code != 2 || !strings.Contains(e, "usage: viiwork-release") {
		t.Fatalf("%d %s", code, e)
	}
}
