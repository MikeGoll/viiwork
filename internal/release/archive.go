package release

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// ReadArchive reads a release archive — a gzip'd tar with one top directory
// named top — into memory: name (relative to top) to content. It refuses
// anything a release never contains: another top directory, a path that
// escapes, a non-regular file, a name listed twice, mode bits beyond rwx, or
// more than maxBytes of content in all. Both viiwork-release compare and a
// node's staging read archives through it, so what is compared is exactly
// what is run.
func ReadArchive(r io.Reader, top string, maxBytes int64) (map[string][]byte, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		name := h.Name
		if path.IsAbs(name) || name != path.Clean(name) && name != path.Clean(name)+"/" || strings.Contains(name, "..") {
			return nil, fmt.Errorf("unsafe path %q", name)
		}
		dir, rest, _ := strings.Cut(strings.TrimSuffix(name, "/"), "/")
		if dir != top {
			return nil, fmt.Errorf("%q is outside the top directory %q", name, top)
		}
		if h.Mode&^0o777 != 0 {
			return nil, fmt.Errorf("%s: mode %o has bits beyond rwx", name, h.Mode)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if rest != "" {
				return nil, fmt.Errorf("unexpected directory %q", name)
			}
		case tar.TypeReg:
			if rest == "" {
				return nil, fmt.Errorf("file %q at the top level", name)
			}
			if _, dup := files[rest]; dup {
				return nil, fmt.Errorf("%s is in the archive twice", rest)
			}
			b, err := io.ReadAll(io.LimitReader(tr, maxBytes-total+1))
			if err != nil {
				return nil, err
			}
			total += int64(len(b))
			if total > maxBytes {
				return nil, fmt.Errorf("archive content is larger than %d bytes", maxBytes)
			}
			files[rest] = b
		default:
			return nil, fmt.Errorf("%s: not a regular file (type %q)", rest, h.Typeflag)
		}
	}
}
