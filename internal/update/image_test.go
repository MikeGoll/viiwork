package update

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeHelper answers the node's next image request the way the host's
// engine helper does, after first leaving a stale answer for another request.
func fakeHelper(t *testing.T, dir string, answer func(ImageRequest) ImageResult) {
	t.Helper()
	os.Remove(filepath.Join(dir, ImageRequestFile))
	os.WriteFile(filepath.Join(dir, ImageResultFile), []byte(`{"version":"v2.6.0","id":"00000000000000000000000000000000","ok":true}`), 0o644)
	go func() {
		for i := 0; i < 500; i++ {
			b, err := os.ReadFile(filepath.Join(dir, ImageRequestFile))
			var req ImageRequest
			if err == nil && json.Unmarshal(b, &req) == nil {
				out, _ := json.Marshal(answer(req))
				os.WriteFile(filepath.Join(dir, ImageResultFile), out, 0o644)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
}

func TestImageHelperPrepare(t *testing.T) {
	dir := t.TempDir()
	h := ImageHelper{Dir: dir, Poll: 5 * time.Millisecond, Wait: 5 * time.Second}
	fakeHelper(t, dir, func(r ImageRequest) ImageResult {
		if !ValidRequestID(r.ID) {
			t.Errorf("request id %q", r.ID)
		}
		return ImageResult{Version: r.Version, ID: r.ID, OK: true, Engines: map[string]string{"llamacpp": "b10438"}}
	})
	engines, err := h.Prepare(context.Background(), "v2.6.0")
	if err != nil || engines["llamacpp"] != "b10438" {
		t.Fatalf("Prepare = %v, %v", engines, err)
	}

	fakeHelper(t, dir, func(r ImageRequest) ImageResult {
		return ImageResult{Version: r.Version, ID: r.ID, Error: "the image's viiwork differs from the signed release"}
	})
	if _, err := h.Prepare(context.Background(), "v2.6.0"); err == nil || !strings.Contains(err.Error(), "differs from the signed") {
		t.Errorf("helper error: %v", err)
	}

	// An answer for another version is not an answer.
	fakeHelper(t, dir, func(r ImageRequest) ImageResult { return ImageResult{Version: "v2.6.1", ID: r.ID, OK: true} })
	h.Wait = 200 * time.Millisecond
	if _, err := h.Prepare(context.Background(), "v2.6.0"); err == nil || !strings.Contains(err.Error(), "viiwork-engine") {
		t.Errorf("no answer: %v", err)
	}
}
