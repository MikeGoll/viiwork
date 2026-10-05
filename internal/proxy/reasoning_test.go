package proxy

import (
	"github.com/janit/viiwork/v2/internal/activity"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseOf joins payloads the way a backend sends them.
func sseOf(payloads ...string) string {
	var b strings.Builder
	for _, p := range payloads {
		b.WriteString("data: " + p + "\n\n")
	}
	return b.String()
}

// Chunks as Strata v0.1.39 writes them (captured on gb3): a space after every
// colon, reasoning as delta.reasoning_content with no <think> tags, then the
// answer as delta.content, then a finish chunk that also carries usage.
const (
	strataRole   = `{"id": "c1", "object": "chat.completion.chunk", "model": "m", "choices": [{"index": 0, "delta": {"role": "assistant", "content": ""}, "finish_reason": null}]}`
	strataThink1 = `{"id": "c1", "object": "chat.completion.chunk", "model": "m", "choices": [{"index": 0, "delta": {"reasoning_content": "We need 17*23."}, "finish_reason": null}]}`
	strataThink2 = `{"id": "c1", "object": "chat.completion.chunk", "model": "m", "choices": [{"index": 0, "delta": {"reasoning_content": " That is 391."}, "finish_reason": null}]}`
	strataAnswer = `{"id": "c1", "object": "chat.completion.chunk", "model": "m", "choices": [{"index": 0, "delta": {"content": "391"}, "finish_reason": null}]}`
	strataStop   = `{"id": "c1", "object": "chat.completion.chunk", "model": "m", "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}], "usage": {"prompt_tokens": 69, "completion_tokens": 54}}`
	strataLength = `{"id": "c1", "object": "chat.completion.chunk", "model": "m", "choices": [{"index": 0, "delta": {}, "finish_reason": "length"}]}`
)

// Through the handler: which think-off stream a request gets depends on what
// the model's engine declares, and on nothing else.
func TestThinkOffStreamFollowsTheEngine(t *testing.T) {
	stream := sseOf(strataRole, strataThink1, strataThink2, strataAnswer, strataStop, "[DONE]")
	for _, tc := range []struct {
		name     string
		separate bool
		body     string
		want     string // must be in the reply
		wantNot  string // must not be
	}{
		// An engine that keeps reasoning in its own field is passed through
		// byte for byte: a client that shows reasoning_content shows the
		// thinking as it happens, and one that does not ignores the field.
		{"engine separates reasoning", true, `{"model":"m","stream":true,"messages":[]}`, `"reasoning_content": "We need 17*23."`, `"content": "We need`},
		// The rule for every other engine is unchanged: untagged reasoning is
		// renamed to content, because llama-server may answer through it.
		{"engine does not", false, `{"model":"m","stream":true,"messages":[]}`, "We need", "reasoning_content"},
		// think: true is passthrough whatever the engine says.
		{"think on", true, `{"model":"m","stream":true,"think":true,"messages":[]}`, `"reasoning_content": "We need 17*23."`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, stream)
			}))
			defer eng.Close()
			f := newHandlerFx(t)
			f.local.add("m", eng.Listener.Addr().String())
			f.separate = func(string) bool { return tc.separate }
			f.build()
			rec := f.do(http.MethodPost, "/v1/chat/completions", tc.body)
			got := rec.Body.String()
			if rec.Code != 200 || !strings.Contains(got, tc.want) {
				t.Errorf("status %d, want %q in\n%s", rec.Code, tc.want, got)
			}
			if tc.wantNot != "" && strings.Contains(got, tc.wantNot) {
				t.Errorf("%q must not reach the client:\n%s", tc.wantNot, got)
			}
		})
	}
}

// The prompt history keeps what the /prompt page needs for an average rate:
// the tokens the reply was, and how long generation took once it started.
func TestPromptEntryCarriesTokensAndGenerationTime(t *testing.T) {
	eng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseOf(strataRole, strataAnswer))
		w.(http.Flusher).Flush()
		time.Sleep(30 * time.Millisecond) // generation takes time after the first token
		_, _ = io.WriteString(w, sseOf(strataStop, "[DONE]"))
	}))
	defer eng.Close()
	f := newHandlerFx(t)
	f.local.add("m", eng.Listener.Addr().String())
	f.build()
	if rec := f.do(http.MethodPost, "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	evs := f.requestEvents()
	entry, ok := f.log.GetPrompt(evs[0].RequestID)
	if !ok || entry.OutputTokens != 54 {
		t.Errorf("output_tokens = %d, want the reply's completion_tokens 54 (%+v)", entry.OutputTokens, entry)
	}
	if entry.GenMS < 25 || entry.GenMS > entry.ElapsedMS {
		t.Errorf("gen_ms = %d: want the time from the first token to the end (elapsed %d)", entry.GenMS, entry.ElapsedMS)
	}
}

// A plain reply has no first-token moment: the count is kept, the time is not.
func TestPromptEntryOfAPlainReplyHasNoGenerationTime(t *testing.T) {
	eng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"391"}}],"usage":{"prompt_tokens":5,"completion_tokens":7}}`)
	}))
	defer eng.Close()
	f := newHandlerFx(t)
	f.local.add("m", eng.Listener.Addr().String())
	f.build()
	f.do(http.MethodPost, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	entry, _ := f.log.GetPrompt(f.requestEvents()[0].RequestID)
	if entry.OutputTokens != 7 || entry.GenMS != 0 {
		t.Errorf("want 7 tokens and no generation time: %+v", entry)
	}
}

// Through the handler: gen_ms covers the reasoning the tokens were spent on.
func TestPromptEntryGenerationTimeIncludesReasoning(t *testing.T) {
	eng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, sseOf(strataRole, strataThink1))
		fl.Flush()
		time.Sleep(80 * time.Millisecond) // thinking
		_, _ = io.WriteString(w, sseOf(strataAnswer))
		fl.Flush()
		time.Sleep(20 * time.Millisecond)
		_, _ = io.WriteString(w, sseOf(strataStop, "[DONE]"))
	}))
	defer eng.Close()
	f := newHandlerFx(t)
	f.local.add("m", eng.Listener.Addr().String())
	f.separate = func(string) bool { return true }
	f.build()
	f.do(http.MethodPost, "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	entry, _ := f.log.GetPrompt(f.requestEvents()[0].RequestID)
	if entry.OutputTokens != 54 || entry.GenMS < 90 {
		t.Errorf("gen_ms = %d for 54 tokens: it must cover the ~100 ms of reasoning and answer, or the page shows several times the real rate", entry.GenMS)
	}
}

// The same for a llama-server whose <think> block is suppressed: the tokens
// counted include the thinking, so the time must too. The first-token moment
// used for routing is left where it was for that path.
func TestSuppressedThinkBlockStartsGenerationTime(t *testing.T) {
	stream := []string{
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":"<think>"}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":"hm"}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":"</think>"}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"content":"4"}}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	clock := time.Unix(1000, 0)
	w, m := newMeasureWriter(httptest.NewRecorder(), false, func() time.Time { clock = clock.Add(time.Second); return clock })
	streamThinkDisabled(w, &piecesReader{parts: stream}, func() {})
	if m.genAt.IsZero() || m.firstAt.IsZero() || !m.genAt.Before(m.firstAt) {
		t.Errorf("generation must start at the suppressed thinking (genAt %v) and the first token stay at the answer (firstAt %v)", m.genAt, m.firstAt)
	}
}

// The plain reply's rewrite and the stream must agree on a tool call too:
// reasoning in front of one is dropped, not moved into content.
func TestPlainToolCallReplyDropsReasoning(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","content":null,"reasoning_content":"We need the tool.","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`
	out := string(rewriteThinkResponse([]byte(body)))
	if strings.Contains(out, "We need the tool") || !strings.Contains(out, `"tool_calls"`) {
		t.Errorf("want the tool call without the reasoning:\n%s", out)
	}
}

// A request the client gave up on before any output has nothing to show a
// rate for: no stray generation time on an entry with no output and no
// elapsed time.
func TestPromptEntryOfAnAbandonedRequestHasNoRate(t *testing.T) {
	p := activity.NewLog()
	p.StorePrompt(1, "m", "hi")
	recordReply(p, 1, "m", "", 500*time.Millisecond, 0, 58005)
	if e, _ := p.GetPrompt(1); e.GenMS != 0 || e.ElapsedMS != 0 {
		t.Errorf("an entry with no output must carry no times: %+v", e)
	}
	recordReply(p, 1, "m", "391", 500*time.Millisecond, 54, 120)
	if e, _ := p.GetPrompt(1); e.GenMS != 120 || e.OutputTokens != 54 || e.ElapsedMS != 500 {
		t.Errorf("an entry with output carries all three: %+v", e)
	}
}
