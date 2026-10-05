package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
)

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "top/", Typeflag: tar.TypeDir, Mode: 0o755})
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: "top/" + name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestReadArchive(t *testing.T) {
	files, err := ReadArchive(bytes.NewReader(tarGz(t, map[string]string{"viiwork": "BIN", "LICENSE": "MIT"})), "top", 1<<20)
	if err != nil || string(files["viiwork"]) != "BIN" || len(files) != 2 {
		t.Fatalf("ReadArchive = %v, %v", files, err)
	}
	if _, err := ReadArchive(bytes.NewReader(tarGz(t, map[string]string{"viiwork": "BIN"})), "other", 1<<20); err == nil {
		t.Error("a foreign top directory was accepted")
	}
	big := tarGz(t, map[string]string{"viiwork": strings.Repeat("x", 2048)})
	if _, err := ReadArchive(bytes.NewReader(big), "top", 1024); err == nil || !strings.Contains(err.Error(), "larger") {
		t.Errorf("size cap: %v", err)
	}
	if _, err := ReadArchive(strings.NewReader("not gzip"), "top", 1024); err == nil {
		t.Error("garbage accepted")
	}
}
