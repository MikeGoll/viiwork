package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const imageReq = `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"which panel?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]}`

func TestHasImageParts(t *testing.T) {
	for name, body := range map[string]string{
		"a data url part":         imageReq,
		"an image alone":          `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.invalid/a.png"}}]}]}`,
		"in an earlier message":   `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]},{"role":"assistant","content":"a cat"},{"role":"user","content":"and its colour?"}]}`,
		"spaces around the colon": `{"model":"m","messages":[{"role":"user","content":[{"type" : "image_url","image_url":{"url":"x"}}]}]}`,
	} {
		if !hasImageParts([]byte(body)) {
			t.Errorf("%s: want an image request", name)
		}
	}
}

// A coding agent that works on this repository sends prompts full of the
// words image_url. Routing such a request as an image request would keep it
// off every text-only backend.
func TestHasImagePartsIgnoresText(t *testing.T) {
	prompt, _ := json.Marshal(`the part looks like {"type":"image_url","image_url":{"url":"..."}} in the body`)
	for name, body := range map[string]string{
		"plain":                    chatReq,
		"prompt text about it":     `{"model":"m","messages":[{"role":"user","content":` + string(prompt) + `}]}`,
		"a text part about it":     `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":` + string(prompt) + `}]}]}`,
		"a tool schema property":   `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"show","parameters":{"type":"object","properties":{"image_url":{"type":"string"}}}}}]}`,
		"a tool call argument key": `{"model":"m","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"c","function":{"name":"show","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c","content":[{"type":"text","text":"ok"}]}],"image_url":"x"}`,
		"not json":                 `"image_url" {{{`,
		"empty":                    ``,
	} {
		if hasImageParts([]byte(body)) {
			t.Errorf("%s: want a text request", name)
		}
	}
}

// The body scan must cost a text request nothing but the scan.
func BenchmarkHasImagePartsText(b *testing.B) {
	body := chatBody(16384, false)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if hasImageParts(body) {
			b.Fatal("a text body")
		}
	}
}

func BenchmarkHasImagePartsImage(b *testing.B) {
	body := []byte(strings.Replace(imageReq, "iVBORw0KGgo=", strings.Repeat("QUJD", 64<<10), 1))
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !hasImageParts(body) {
			b.Fatal("an image body")
		}
	}
}

func TestHasImagePartsTextAllocatesNothing(t *testing.T) {
	body := chatBody(2048, false)
	if n := testing.AllocsPerRun(50, func() { hasImageParts(body) }); n != 0 {
		t.Errorf("a text request allocated %v times in hasImageParts", n)
	}
}

// When every host that serves the model says it reads no images, the client
// is told so at once, in viiwork's error shape, with the model named.
func TestImageRequestToATextOnlyModelIs400(t *testing.T) {
	no := false
	f := newHandlerFx(t)
	textOnly := peerModel("m", 4, 0)
	textOnly.Images = &no
	f.reports.add("P", "127.0.0.1:1", time.Now(), textOnly)
	f.build()

	start := time.Now()
	rec := f.do(http.MethodPost, "/v1/chat/completions", imageReq)
	if e := errorOf(t, rec); rec.Code != http.StatusBadRequest || e.Type != "invalid_request" || e.Message != `model "m" does not read images on any node` {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("answered after %s: it must not queue", d)
	}
}

// With a pin the 400 is about that host: another node may read images.
func TestImageRequestPinnedToATextOnlyHostNamesTheHost(t *testing.T) {
	yes, no := true, false
	f := newHandlerFx(t)
	blind, seeing := peerModel("m", 4, 0), peerModel("m", 4, 0)
	blind.Images, seeing.Images = &no, &yes
	f.reports.add("blind", "127.0.0.1:1", time.Now(), blind)
	f.reports.add("eyes", "127.0.0.1:2", time.Now(), seeing)
	f.build()

	rec := f.do(http.MethodPost, "/v1/chat/completions?host=blind", imageReq)
	if e := errorOf(t, rec); rec.Code != http.StatusBadRequest || e.Message != `model "m" does not read images on host "blind"` {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
}

// An image request reaches the peer that reads images, body untouched, and
// passes the one that does not, however free it is.
func TestImageRequestIsForwardedToThePeerThatReadsImages(t *testing.T) {
	yes, no := true, false
	var got string
	eyes := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		r.Body.Read(b)
		got = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"the left panel"}}]}`))
	}))
	defer eyes.Close()

	f := newHandlerFx(t)
	blind, seeing := peerModel("m", 8, 0), peerModel("m", 1, 0)
	blind.Images, seeing.Images = &no, &yes
	f.reports.add("blind", "127.0.0.1:1", time.Now(), blind)
	f.reports.add("eyes", strings.TrimPrefix(eyes.URL, "http://"), time.Now(), seeing)
	f.build()

	rec := f.do(http.MethodPost, "/v1/chat/completions", imageReq)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "the left panel") {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
	if got != imageReq {
		t.Errorf("the peer received %q, want the body as sent", got)
	}
}
