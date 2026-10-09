package update

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/janit/viiwork/v2/internal/durable"
)

// On a Docker install made by `viiwork init`, a root-owned helper on the host
// (`viiwork engine-sync`) makes the compose file's image follow current.
// The node only ever asks: through these two files beside state.json.
const (
	// ImageRequestFile asks the helper to pull and verify one release's
	// image: an ImageRequest, nothing else. It is all the helper reads from
	// the node's files besides state.json's current.
	ImageRequestFile = "engine-request.json"
	// ImageResultFile is the helper's answer, written by root, readable here.
	ImageResultFile = "engine-result.json"
)

// ImageRequest names a release and the request, so that an answer to an
// earlier request is never taken for this one.
type ImageRequest struct {
	Version string `json:"version"`
	ID      string `json:"id"`
}

// ImageResult is the helper's answer. Engines are the engine versions the
// image carries, by engine name, as that engine's Versioner spells them.
type ImageResult struct {
	Version string            `json:"version"`
	ID      string            `json:"id"`
	OK      bool              `json:"ok"`
	Error   string            `json:"error,omitempty"`
	Image   string            `json:"image,omitempty"` // repo@digest, as pulled
	Engines map[string]string `json:"engines,omitempty"`
}

var requestIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidRequestID reports whether id is one ImageHelper would write.
func ValidRequestID(id string) bool { return requestIDRe.MatchString(id) }

// ImageHelper is the node's side of the exchange.
type ImageHelper struct {
	Dir  string        // the releases directory
	Poll time.Duration // 0 = 1 s
	// Wait bounds the answer; 0 = 12 minutes, which leaves the stage's
	// download inside its 15. A first pull of an engine image is gigabytes.
	Wait time.Duration
}

// Prepare asks the helper to pull and verify version's image and returns the
// engine versions it carries. The helper is this host's own root, so its
// answer is taken as it is; the request is only ever a version name.
func (h ImageHelper) Prepare(ctx context.Context, version string) (map[string]string, error) {
	poll, wait := h.Poll, h.Wait
	if poll <= 0 {
		poll = time.Second
	}
	if wait <= 0 {
		wait = 12 * time.Minute
	}
	var raw [16]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		return nil, err
	}
	req := ImageRequest{Version: version, ID: hex.EncodeToString(raw[:])}
	b, _ := json.Marshal(req)
	if err := durable.WriteFile(h.Dir, ImageRequestFile, append(b, '\n')); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		if b, err := os.ReadFile(filepath.Join(h.Dir, ImageResultFile)); err == nil {
			var res ImageResult
			if json.Unmarshal(b, &res) == nil && res.ID == req.ID && res.Version == version {
				if !res.OK {
					return nil, fmt.Errorf("the host's engine helper could not prepare the %s image: %s", version, res.Error)
				}
				return res.Engines, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("the host's engine helper did not prepare the %s image within %s: is viiwork-engine.path enabled on the host (sudo viiwork init adds it)?", version, wait)
		case <-t.C:
		}
	}
}

// ImagePrepared reports whether the helper's last answer is a successful
// prepare of version: its image is on this host, verified. Activation on a
// managed install requires it, so that the swap never waits on a pull that
// could outlast the wait for the helper.
func ImagePrepared(dir, version string) error {
	b, err := os.ReadFile(filepath.Join(dir, ImageResultFile))
	var res ImageResult
	if err != nil || json.Unmarshal(b, &res) != nil || !res.OK || res.Version != version {
		return fmt.Errorf("the image for %s is not prepared on this host: stage it first", version)
	}
	return nil
}
