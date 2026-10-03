package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// maxCaptureBytes bounds the raw response bytes retained per request for the
// dashboard's output panel.
//
// The number looks generous because SSE is verbose: llama-server wraps every
// few characters of text in a ~200-byte JSON envelope, so the ratio of stream
// bytes to answer text runs around 50:1. 2 MB of raw stream is therefore only
// tens of thousands of characters of answer — roughly where activity's own
// maxPromptChars truncates anyway. Live memory is this times the number of
// requests in flight, which backpressure already bounds.
const maxCaptureBytes = 2 << 20

// captureTailBytes is how much of the newest output is kept past the cap. A
// long stream's final usage chunk lies beyond maxCaptureBytes, and without the
// tail tokens_total would silently undercount exactly the longest requests
// (P4 Decision 13).
const captureTailBytes = 16 << 10

// maxOutputChars is how much output text the prompt history keeps: activity's
// maxPromptChars, which truncates everything past it. Output stops decoding
// there (TestOutputCapMatchesThePromptStore pins the two together).
const maxOutputChars = 50000

// captureWriter tees a proxied response into a bounded buffer on its way to
// the client, so the finished text can be recorded for the prompt history.
//
// It captures raw bytes and parses them once, after the response completes,
// rather than decoding each SSE chunk as it passes. That ordering is
// deliberate: the streaming loop is the per-token hot path, and the think
// rewriter goes out of its way to avoid decoding chunks it does not have to
// (see streamThinkDisabled's fast path). Adding a JSON decode per token here
// would give that back. An append into a byte slice is a memcpy, and once the
// cap is reached even that stops.
//
// Wrapping the outer ResponseWriter also means what gets captured is what the
// client actually received — after think-block rewriting, not before.
type captureWriter struct {
	http.ResponseWriter
	buf      []byte
	status   int
	overflow bool
	tail     []byte // the newest bytes past the cap, at most captureTailBytes once trimmed

	usageDone  bool // usage has been decoded; usageCache is valid even when it found nothing
	usageCache decodedUsage
}

func (c *captureWriter) WriteHeader(status int) {
	c.status = status
	c.ResponseWriter.WriteHeader(status)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if !c.overflow {
		if room := maxCaptureBytes - len(c.buf); len(b) <= room {
			c.grow(len(b))
			c.buf = append(c.buf, b...)
		} else {
			c.grow(room)
			c.buf = append(c.buf, b[:room]...)
			c.overflow = true
			c.keepTail(b[room:])
		}
	} else {
		c.keepTail(b)
	}
	return c.ResponseWriter.Write(b)
}

// grow makes room for n more bytes by doubling, up to the cap. append alone
// grows a large slice by about 1.25x, which allocates about five times the
// cap on the way to it; doubling allocates about twice.
func (c *captureWriter) grow(n int) {
	if cap(c.buf)-len(c.buf) >= n {
		return
	}
	next := min(max(2*cap(c.buf), len(c.buf)+n, 4<<10), maxCaptureBytes)
	buf := make([]byte, len(c.buf), next)
	copy(buf, c.buf)
	c.buf = buf
}

// keepTail appends past-the-cap bytes to a rolling tail. Nothing here runs
// below the cap, so the common response pays nothing for it.
func (c *captureWriter) keepTail(b []byte) {
	if len(b) >= captureTailBytes {
		c.tail = append(c.tail[:0], b[len(b)-captureTailBytes:]...)
		return
	}
	if c.tail == nil {
		c.tail = make([]byte, 0, 2*captureTailBytes)
	}
	if len(c.tail)+len(b) > cap(c.tail) {
		keep := captureTailBytes - len(b)
		copy(c.tail, c.tail[len(c.tail)-keep:])
		c.tail = c.tail[:keep]
	}
	c.tail = append(c.tail, b...)
}

// usageTail is the bytes CompletionTokens and PromptUsage both scan for usage:
// the whole capture below the cap, or past it, the tail with its first,
// partial line dropped.
func (c *captureWriter) usageTail() []byte {
	if !c.overflow {
		return c.buf
	}
	tail := c.tail
	if i := bytes.IndexByte(tail, '\n'); i >= 0 {
		tail = tail[i+1:]
	}
	return tail
}

// usage lazily decodes this response's usage exactly once, so that a caller
// asking for both CompletionTokens and PromptUsage on the same captureWriter
// — Task 5's wiring calls both per request — shares one scan of the buffer
// and one JSON decode per usage-bearing payload, not two.
func (c *captureWriter) usage() decodedUsage {
	if !c.usageDone {
		completion, completionOK, prompt, promptOK := extractUsage(c.usageTail())
		c.usageCache = decodedUsage{completion: completion, completionOK: completionOK, prompt: prompt, promptOK: promptOK}
		c.usageDone = true
	}
	return c.usageCache
}

// CompletionTokens is usage.completion_tokens from the finished response, read
// once after the response completes and never per token. Past the cap it reads
// the tail, dropping its first, partial line.
func (c *captureWriter) CompletionTokens() (int64, bool) {
	u := c.usage()
	return u.completion, u.completionOK
}

// Unwrap lets http.ResponseController reach the real writer, should anything
// downstream ever need it.
func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// flushCaptureWriter is captureWriter for an underlying writer that flushes.
// The split is not cosmetic: both proxyRequest and streamThinkDisabled branch
// on w.(http.Flusher), and streamThinkDisabled degrades to a plain io.Copy
// when the assertion fails. A wrapper that always advertised Flush would make
// a non-flushing writer look flushable; one that never did would silently turn
// streaming responses into buffered ones.
type flushCaptureWriter struct {
	*captureWriter
	f http.Flusher
}

func (c *flushCaptureWriter) Flush() { c.f.Flush() }

// newCaptureWriter wraps w, preserving whether it can flush.
func newCaptureWriter(w http.ResponseWriter) (http.ResponseWriter, *captureWriter) {
	c := &captureWriter{ResponseWriter: w, status: http.StatusOK}
	if f, ok := w.(http.Flusher); ok {
		return &flushCaptureWriter{captureWriter: c, f: f}, c
	}
	return c, c
}

// Output returns the assistant text the response carried, or — for a response
// that failed — the error body itself, which is the more useful thing to see
// in the dashboard when a request went wrong.
func (c *captureWriter) Output() string {
	out := extractOutputText(c.buf)
	if out == "" && c.status >= 400 {
		return strings.TrimSpace(string(c.buf))
	}
	if c.overflow && out != "" {
		return out + "\n\n... [output truncated: response exceeded the capture limit]"
	}
	return out
}

// completionShape covers every response body this proxy forwards, in both
// their streaming and non-streaming spellings: chat completions carry the text
// under delta (streaming) or message (whole), plain completions carry it under
// text. Unused fields simply stay empty, so one struct decodes all of them.
type completionShape struct {
	Choices []struct {
		Delta struct {
			Content          string     `json:"content"`
			ReasoningContent string     `json:"reasoning_content"`
			ToolCalls        []toolCall `json:"tool_calls"`
		} `json:"delta"`
		Message struct {
			Content          string     `json:"content"`
			ReasoningContent string     `json:"reasoning_content"`
			ToolCalls        []toolCall `json:"tool_calls"`
		} `json:"message"`
		Text string `json:"text"`
	} `json:"choices"`
}

// toolCall is one tool call in either spelling: whole in a message, or as
// deltas in a stream, where the name arrives once and the arguments in
// fragments, all tagged with the call's index.
type toolCall struct {
	Index    int `json:"index"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// maxToolCalls bounds the calls one response may record; an index past it is
// dropped, so a malformed index cannot grow the slice without limit.
const maxToolCalls = 64

// toolCallText gathers a response's tool calls by index. A coding agent's turn
// usually answers with tool calls and no text, so without them most of its
// turns would leave nothing in the prompt history.
type toolCallText struct {
	names, args [][]byte
	size        int
}

func (t *toolCallText) add(i int, c toolCall) {
	if i < 0 || i >= maxToolCalls {
		return
	}
	for len(t.names) <= i {
		t.names = append(t.names, nil)
		t.args = append(t.args, nil)
	}
	t.names[i] = append(t.names[i], c.Function.Name...)
	t.args[i] = append(t.args[i], c.Function.Arguments...)
	t.size += len(c.Function.Name) + len(c.Function.Arguments)
}

// String renders one call per line as "name arguments".
func (t *toolCallText) String() string {
	var sb strings.Builder
	for i := range t.names {
		name, args := t.names[i], bytes.TrimSpace(t.args[i])
		if len(name) == 0 && len(args) == 0 {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.Write(name)
		if len(args) > 0 {
			sb.WriteByte(' ')
			sb.Write(args)
		}
	}
	return sb.String()
}

var sseDataPrefix = []byte("data:")

// extractOutputText reassembles the answer from a captured response body,
// which is either an SSE stream of deltas or a single JSON object.
//
// Reasoning is kept, labelled and separated from the answer rather than
// concatenated into it. A thinking model with think enabled puts everything in
// reasoning_content and leaves content empty, so dropping reasoning would show
// a blank output for exactly the requests most worth inspecting; merging the
// two silently would misrepresent what the client received.
//
// Decoding stops once the text gathered exceeds maxOutputChars: the prompt
// history keeps no more than that, and the text past it would be decoded only
// to be thrown away — up to maxCaptureBytes of JSON, while the handler waits.
// What is kept is unchanged: the first maxOutputChars bytes of the result are
// the same with or without the early stop, save for a response ending in more
// whitespace than that.
func extractOutputText(raw []byte) string {
	var content, reasoning strings.Builder
	var tools toolCallText

	if bytes.HasPrefix(bytes.TrimLeft(raw, " \r\n"), sseDataPrefix) {
		rest := raw
		for len(rest) > 0 && content.Len()+reasoning.Len()+tools.size <= maxOutputChars {
			line := rest
			if i := bytes.IndexByte(rest, '\n'); i >= 0 {
				line, rest = rest[:i], rest[i+1:]
			} else {
				rest = nil
			}
			line = bytes.TrimSpace(line)
			if !bytes.HasPrefix(line, sseDataPrefix) {
				continue
			}
			payload := bytes.TrimSpace(line[len(sseDataPrefix):])
			if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
				continue
			}
			appendChoiceText(payload, &content, &reasoning, &tools)
		}
	} else {
		appendChoiceText(raw, &content, &reasoning, &tools)
	}

	answer := strings.TrimSpace(content.String())
	think := strings.TrimSpace(reasoning.String())
	calls := tools.String()
	if think == "" && calls == "" {
		return answer
	}
	var sb strings.Builder
	for _, sec := range [...]struct{ label, text string }{
		{"[reasoning]", think}, {"[answer]", answer}, {"[tool calls]", calls},
	} {
		if sec.text == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(sec.label + "\n" + sec.text)
	}
	return sb.String()
}

// appendChoiceText decodes one JSON body or SSE payload and appends whatever
// text it carries. A payload that does not decode is skipped rather than
// reported: this runs off the request path on best-effort telemetry, and a
// backend emitting something unexpected should not cost anything visible.
func appendChoiceText(payload []byte, content, reasoning *strings.Builder, tools *toolCallText) {
	var c completionShape
	if json.Unmarshal(payload, &c) != nil {
		return
	}
	for _, ch := range c.Choices {
		content.WriteString(ch.Delta.Content)
		content.WriteString(ch.Message.Content)
		content.WriteString(ch.Text)
		reasoning.WriteString(ch.Delta.ReasoningContent)
		reasoning.WriteString(ch.Message.ReasoningContent)
		for _, tc := range ch.Delta.ToolCalls {
			tools.add(tc.Index, tc)
		}
		// A whole message lists its calls in order and may omit index.
		for i, tc := range ch.Message.ToolCalls {
			tools.add(i, tc)
		}
	}
}

// usageShape covers a finished response's usage object: completion count,
// prompt count and how much of the prompt was cached. It appears either in a
// single JSON object or in the last usage-bearing SSE event of a stream.
type usageShape struct {
	Usage *struct {
		CompletionTokens    *float64 `json:"completion_tokens"`
		PromptTokens        *float64 `json:"prompt_tokens"`
		PromptTokensDetails *struct {
			CachedTokens *float64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

var usageKey = []byte(`"usage"`)

// promptUsage is a finished response's prompt token counts, for performance
// measurement. CachedPresent distinguishes a reported 0 from an absent field.
type promptUsage struct {
	Prompt, Cached int64
	CachedPresent  bool
}

// decodedUsage is one payload's usage object, decoded once. completionOK and
// promptOK are independent: a payload can carry either count without the
// other (or neither), and extractUsage tracks each independently across a
// stream's payloads for exactly that reason.
type decodedUsage struct {
	completion   int64
	completionOK bool
	prompt       promptUsage
	promptOK     bool
}

// decodeUsage decodes one JSON payload's usage object, if it has one. This is
// the one place completion and prompt counts are ever read out of a payload,
// so a caller wanting both never decodes the same JSON twice.
func decodeUsage(payload []byte) decodedUsage {
	var d decodedUsage
	if len(payload) == 0 || payload[0] != '{' {
		return d
	}
	var u usageShape
	if json.Unmarshal(payload, &u) != nil || u.Usage == nil {
		return d
	}
	if u.Usage.CompletionTokens != nil {
		d.completion, d.completionOK = int64(*u.Usage.CompletionTokens), true
	}
	if u.Usage.PromptTokens != nil {
		d.prompt.Prompt, d.promptOK = int64(*u.Usage.PromptTokens), true
		if pd := u.Usage.PromptTokensDetails; pd != nil && pd.CachedTokens != nil {
			d.prompt.Cached, d.prompt.CachedPresent = int64(*pd.CachedTokens), true
		}
	}
	return d
}

func promptUsageOf(payload []byte) (promptUsage, bool) {
	d := decodeUsage(payload)
	return d.prompt, d.promptOK
}

// promptUsageFromEvent reads one SSE event ("data: {...}\n\n").
func promptUsageFromEvent(ev []byte) (promptUsage, bool) {
	ev = bytes.TrimSpace(ev)
	if !bytes.HasPrefix(ev, sseDataPrefix) {
		return promptUsage{}, false
	}
	return promptUsageOf(bytes.TrimSpace(ev[len(sseDataPrefix):]))
}

// completionTokensFromEvent reads usage.completion_tokens from one SSE event
// ("data: {...}\n\n"), such as the usage chunk stripped for a client that did
// not ask for usage.
func completionTokensFromEvent(ev []byte) (int64, bool) {
	ev = bytes.TrimSpace(ev)
	if !bytes.HasPrefix(ev, sseDataPrefix) {
		return 0, false
	}
	d := decodeUsage(bytes.TrimSpace(ev[len(sseDataPrefix):]))
	return d.completion, d.completionOK
}

// extractUsage finds usage in a finished response: a single JSON object, or
// else an SSE stream, where the last data payload carrying a numeric value
// wins — independently for completion and prompt tokens, since in principle
// the two counts need not be reported in the same payload. Absent gives
// (0, false): a streaming client that did not ask for usage is counted as a
// request with no tokens.
//
// extractCompletionTokens and extractPromptUsage are both thin views onto
// this: each usage-bearing payload is decoded once here for both fields
// together, rather than once per field as two independent scans would. A
// caller wanting both counts from the same buffer — captureWriter.usage()
// does, so that CompletionTokens and PromptUsage share one call here — pays
// for exactly one pass.
func extractUsage(raw []byte) (completion int64, completionOK bool, prompt promptUsage, promptOK bool) {
	if d := decodeUsage(bytes.TrimSpace(raw)); d.completionOK || d.promptOK {
		return d.completion, d.completionOK, d.prompt, d.promptOK
	}
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, sseDataPrefix) || !bytes.Contains(line, usageKey) {
			continue // only chunks that mention usage are decoded
		}
		d := decodeUsage(bytes.TrimSpace(line[len(sseDataPrefix):]))
		if d.completionOK {
			completion, completionOK = d.completion, true
		}
		if d.promptOK {
			prompt, promptOK = d.prompt, true
		}
	}
	return
}

// extractCompletionTokens finds usage.completion_tokens in a finished
// response. See extractUsage.
func extractCompletionTokens(raw []byte) (int64, bool) {
	completion, completionOK, _, _ := extractUsage(raw)
	return completion, completionOK
}

// extractPromptUsage is extractCompletionTokens for prompt counts. See
// extractUsage.
func extractPromptUsage(raw []byte) (promptUsage, bool) {
	_, _, prompt, promptOK := extractUsage(raw)
	return prompt, promptOK
}

// PromptUsage is CompletionTokens for the prompt counts.
func (c *captureWriter) PromptUsage() (promptUsage, bool) {
	u := c.usage()
	return u.prompt, u.promptOK
}
