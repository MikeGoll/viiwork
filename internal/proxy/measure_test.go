package proxy

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestInjectIncludeUsage(t *testing.T) {
	cases := []struct {
		name, in string
		injected bool
	}{
		{"streamed, not asked", `{"model":"m","stream":true,"messages":[]}`, true},
		{"not streamed", `{"model":"m","messages":[]}`, false},
		{"stream false", `{"model":"m","stream":false,"messages":[]}`, false},
		{"already asked", `{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":[]}`, false},
		{"asked false", `{"model":"m","stream":true,"stream_options":{"include_usage":false},"messages":[]}`, true},
		{"other option", `{"model":"m","stream":true,"stream_options":{"continuous_usage_stats":false},"messages":[]}`, true},
		{"stream only in prompt text", `{"model":"m","messages":[{"role":"user","content":"\"stream\":true"}]}`, false},
	}
	for _, tc := range cases {
		out, injected := injectIncludeUsage([]byte(tc.in))
		if injected != tc.injected {
			t.Errorf("%s: injected = %v, want %v", tc.name, injected, tc.injected)
			continue
		}
		if !injected {
			if string(out) != tc.in {
				t.Errorf("%s: body changed without injection", tc.name)
			}
			continue
		}
		var got struct {
			StreamOptions map[string]any `json:"stream_options"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("%s: injected body is not JSON: %v\n%s", tc.name, err, out)
		}
		if got.StreamOptions["include_usage"] != true {
			t.Errorf("%s: include_usage not true in %s", tc.name, out)
		}
	}
}

const usageEvent = "data: {\"id\":\"x\",\"choices\":[],\"usage\":{\"prompt_tokens\":4000,\"completion_tokens\":9,\"prompt_tokens_details\":{\"cached_tokens\":1000}}}\n\n"

func TestStripKeepsTheClientsBytesIdentical(t *testing.T) {
	events := []string{
		"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
		usageEvent,
		"data: [DONE]\n\n",
	}
	want := events[0] + events[1] + events[3]
	stream := strings.Join(events, "")
	// Every split point, so an event straddling two reads is covered (Review Focus 1).
	for cut := 1; cut < len(stream); cut++ {
		rec := httptest.NewRecorder()
		w, m := newMeasureWriter(rec, true, time.Now)
		w.Write([]byte(stream[:cut]))
		w.Write([]byte(stream[cut:]))
		if err := m.finish(); err != nil {
			t.Fatal(err)
		}
		if rec.Body.String() != want {
			t.Fatalf("cut %d: client got\n%q\nwant\n%q", cut, rec.Body.String(), want)
		}
		if string(m.stripped) != usageEvent {
			t.Fatalf("cut %d: stripped = %q", cut, m.stripped)
		}
	}
}

func TestNoStripPassesEverything(t *testing.T) {
	rec := httptest.NewRecorder()
	w, m := newMeasureWriter(rec, false, time.Now)
	w.Write([]byte(usageEvent))
	m.finish()
	if rec.Body.String() != usageEvent || m.stripped != nil {
		t.Fatal("a client that asked for usage must receive it")
	}
}

func TestFirstTokenDetection(t *testing.T) {
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		chunk string
		want  bool
	}{
		{`data: {"choices":[{"delta":{"role":"assistant","content":""}}]}`, false},
		{`data: {"choices":[{"delta":{"role":"assistant"}}]}`, false},
		{`data: {"choices":[{"delta":{"content":"hi"}}]}`, true},
		{`data: {"choices":[{"delta":{"reasoning_content":"hm"}}]}`, true},
		{`data: {"choices":[{"text":"hi"}]}`, true},
		{`data: {"choices":[{"delta":{"tool_calls":[{"index":0}]}}]}`, true}, // Review Focus 2
		{`data: {"choices":[{"delta":{"tool_calls":[ {"index":0}]}}]}`, true},
		{`data: {"choices":[{"delta":{"tool_calls":[]}}]}`, false},
		{`data: {"choices":[{"delta":{"tool_calls":[ ]}}]}`, false},
		{`data: {"choices":[{"delta":{"tool_calls":[],"content":"hi"}}]}`, true},
		{`data: {"choices":[{"delta":{"content":null}}]}`, false},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		w, m := newMeasureWriter(rec, false, func() time.Time { return at })
		w.Write([]byte(tc.chunk + "\n\n"))
		if got := !m.firstAt.IsZero(); got != tc.want {
			t.Errorf("%s: first token = %v, want %v", tc.chunk, got, tc.want)
		}
	}
}

func TestPromptUsageFromEvent(t *testing.T) {
	u, ok := promptUsageFromEvent([]byte(usageEvent))
	if !ok || u.Prompt != 4000 || u.Cached != 1000 || !u.CachedPresent {
		t.Fatalf("got %+v %v", u, ok)
	}
	u, ok = promptUsageFromEvent([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10}}\n\n"))
	if !ok || u.Prompt != 10 || u.CachedPresent {
		t.Fatalf("no details: got %+v %v", u, ok)
	}
	if _, ok := promptUsageFromEvent([]byte("data: {\"choices\":[]}\n\n")); ok {
		t.Fatal("no usage must report false")
	}
}

func TestCapturePromptUsage(t *testing.T) {
	rec := httptest.NewRecorder()
	w, c := newCaptureWriter(rec)
	w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" + usageEvent + "data: [DONE]\n\n"))
	u, ok := c.PromptUsage()
	if !ok || u.Prompt != 4000 || u.Cached != 1000 {
		t.Fatalf("got %+v %v", u, ok)
	}
}

// A key split across two writes still marks the first token. The think-off
// stream's rename writes `"content"` and the value as separate pieces, and a
// per-write scan never saw a token on llama.cpp models that answer through
// reasoning_content (granite 4.2 on n100 and gb1, found on the fleet).
func TestFirstTokenAcrossWrites(t *testing.T) {
	cases := []struct {
		name   string
		writes []string
		want   bool
	}{
		{"key then value", []string{`data: {"choices":[{"delta":{`, `"content"`, `:"Okay"}}]}`, "\n\n"}, true},
		{"one byte at a time", strings.Split(`data: {"choices":[{"delta":{"content":"Okay"}}]}`, ""), true},
		{"empty value split", []string{`data: {"choices":[{"delta":{"content"`, `:""}}]}`}, false},
		{"tool call split", []string{`{"delta":{"tool_calls"`, `:[{"index":0}]}}`}, true},
		{"empty tool calls split", []string{`{"delta":{"tool_calls"`, `:[]}}`}, false},
	}
	for _, tc := range cases {
		w, m := newMeasureWriter(httptest.NewRecorder(), false, time.Now)
		for _, s := range tc.writes {
			w.Write([]byte(s))
		}
		if got := !m.firstAt.IsZero(); got != tc.want {
			t.Errorf("%s: first token = %v, want %v", tc.name, got, tc.want)
		}
	}
}

type piecesReader struct{ parts []string }

func (p *piecesReader) Read(b []byte) (int, error) {
	if len(p.parts) == 0 {
		return 0, io.EOF
	}
	n := copy(b, p.parts[0])
	if p.parts[0] = p.parts[0][n:]; p.parts[0] == "" {
		p.parts = p.parts[1:]
	}
	return n, nil
}

// The think-off stream renames reasoning_content to content; the measurement
// behind it must still see the first token and the usage chunk.
func TestThinkOffRenamedReasoningIsMeasured(t *testing.T) {
	stream := []string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":null}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":"Okay"}}]}` + "\n\n",
		`data: {"choices":[],"usage":{"completion_tokens":4,"prompt_tokens":1716,"prompt_tokens_details":{"cached_tokens":8}}}` + "\n\n",
		"data: [DONE]\n\n",
	}
	rec := httptest.NewRecorder()
	w, m := newMeasureWriter(rec, true, time.Now)
	streamThinkDisabled(w, &piecesReader{parts: stream}, func() {})
	if err := m.finish(); err != nil {
		t.Fatal(err)
	}
	if m.firstAt.IsZero() {
		t.Fatal("no first token: the renamed content chunk was not seen")
	}
	if u, ok := promptUsageFromEvent(m.stripped); !ok || u.Prompt-u.Cached != 1708 {
		t.Fatalf("usage = %+v %v", u, ok)
	}
	if strings.Contains(rec.Body.String(), "usage") || !strings.Contains(rec.Body.String(), `"content":"Okay"`) {
		t.Fatalf("client body %q", rec.Body.String())
	}
}
