package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// maxRequestBodySize limits inference request bodies to 32 MB.
const maxRequestBodySize = 32 << 20

// presizeCap bounds how much readBodyPresized will trust Content-Length for.
// 2 MB comfortably covers a 100K-token prompt; anything larger grows normally.
const presizeCap = 2 << 20

// HeaderTask is a fallback for clients whose SDKs forbid non-standard JSON fields.
const HeaderTask = "X-Viiwork-Task"

// maxTaskIDLen caps the task tag length — the dashboard badge needs to stay readable.
const maxTaskIDLen = 32

// sanitizeTaskID trims whitespace, strips non-printable runes, and truncates to maxTaskIDLen.
func sanitizeTaskID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	b := make([]rune, 0, len(s))
	for _, r := range s {
		if r >= 0x20 && r != 0x7f {
			b = append(b, r)
		}
	}
	if len(b) > maxTaskIDLen {
		b = b[:maxTaskIDLen]
	}
	return strings.TrimSpace(string(b))
}

// maxHostLen bounds the pin; a DNS name is at most 253 octets.
const maxHostLen = 253

// sanitizeHost validates the ?host= pin. It returns ("", true) when there is
// no pin — absent, blank, or the literal "mesh", so the default is spelled the
// same way in a URL as in the chat page's selector — and (host, true) for a
// well-formed hostname or IP literal. Anything else is ("", false), and the
// caller answers 400 rather than routing as if no pin were given: a pin that
// quietly does not hold defeats the comparison the feature exists for. The
// value is only ever compared against known hostnames and never dialled, so
// this is about a clear answer, not about safety.
func sanitizeHost(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "mesh") {
		return "", true
	}
	if len(s) > maxHostLen {
		return "", false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == ':', c == '_', c == '-', c == '[', c == ']':
		default:
			return "", false
		}
	}
	return s, true
}

// maxPrefer bounds the ?prefer= list. A fleet is a dozen machines and a list
// names the few a client would rather have.
const maxPrefer = 8

// parsePrefer validates the ?prefer= list (or its header): node names
// separated by commas, in order of preference. Blank entries and the literal
// "mesh" are dropped, so "gb1,mesh" means gb1 and then anywhere, which is
// what the list means anyway. A malformed name or more than maxPrefer names
// is (nil, false) and the caller answers 400, for the reason sanitizeHost
// gives. Like the pin, a name is only ever compared and never dialled.
func parsePrefer(s string) ([]string, bool) {
	var out []string
	for s != "" {
		var name string
		name, s, _ = strings.Cut(s, ",")
		name, ok := sanitizeHost(name)
		if !ok {
			return nil, false
		}
		if name == "" {
			continue
		}
		if len(out) == maxPrefer {
			return nil, false
		}
		out = append(out, name)
	}
	return out, true
}

// readBodyPresized buffers a request body, sizing the destination from
// Content-Length when the client supplied a usable one.
//
// io.ReadAll starts at 512 bytes and grows by repeated append, so a large chat
// completion body is reallocated and copied ~a dozen times on the way in. Chat
// clients always send Content-Length (the body is a fully-built JSON document,
// not a stream), so the size is known up front in practice.
//
// The length is treated as a HINT, never as truth: it is ignored when absent
// (-1), when implausible, and it does not bound how much is read. The caller
// has already wrapped the body in http.MaxBytesReader, which remains the only
// thing enforcing the size limit. A lying Content-Length therefore costs at
// most one wasted allocation, never a truncated or over-large read.
func readBodyPresized(r io.Reader, contentLength int64) ([]byte, error) {
	if contentLength <= 0 || contentLength > maxRequestBodySize {
		return io.ReadAll(r)
	}
	// Content-Length is CLIENT-CONTROLLED, so it must not size an allocation
	// without a bound. Sending "Content-Length: 32MB" with a one-byte body
	// would otherwise force a 32 MB allocation per request — cheap for the
	// attacker, and multiplied by concurrency an easy way to push a 62 GB host
	// into swap. io.ReadAll never had this exposure because it only ever
	// allocated what it actually read.
	//
	// Capping costs almost nothing: real chat bodies sit far below this, and a
	// genuinely larger one just grows from the cap in a few doublings instead
	// of from 512 bytes in a dozen.
	if contentLength > presizeCap {
		contentLength = presizeCap
	}
	// The headroom is bytes.MinRead, not +1: Buffer.ReadFrom asks grow() for
	// MinRead free bytes before EVERY read, including the final one that just
	// returns io.EOF. Sizing to exactly Content-Length therefore triggers one
	// last doubling and allocates more than io.ReadAll did — measured, not
	// theorised (251 KB/op vs 202 KB before this line was corrected).
	buf := bytes.NewBuffer(make([]byte, 0, contentLength+bytes.MinRead))
	if _, err := buf.ReadFrom(r); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Byte-level keys used to gate the fast field extraction below.
var (
	keyModelJSON = []byte(`"model"`)
	keyThinkJSON = []byte(`"think"`)
	keyTaskJSON  = []byte(`"task"`)
	escapePrefix = []byte(`\u`)
)

// promptExtract pulls just enough of a chat/completions body to recover the
// user-facing prompt text for the dashboard's prompt history. It mirrors the
// same last-user-message convention handlePipeline already uses for
// sourceText, plus the legacy /v1/completions "prompt" string field.
type promptExtract struct {
	Messages []promptMessage `json:"messages"`
	Prompt   string          `json:"prompt"`
}

type promptMessage struct {
	Role       string      `json:"role"`
	Content    messageText `json:"content"`
	ToolCallID string      `json:"tool_call_id"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tool_calls"`
}

// messageText is a message's content as text: a plain string, or the text
// parts of a content-part array joined by newlines. Agent clients such as pi
// send every user message as parts, so reading strings alone recorded no
// prompt at all for them. Non-text parts (images, audio) contribute nothing,
// and anything else decodes to empty rather than failing the body.
type messageText string

func (m *messageText) UnmarshalJSON(b []byte) error {
	switch {
	case len(b) > 1 && b[0] == '"' && bytes.IndexByte(b, '\\') < 0:
		// No escapes: the bytes between the quotes are the text, so skip a
		// second decode — this runs for every message of every request.
		*m = messageText(b[1 : len(b)-1])
	case len(b) > 0 && b[0] == '"':
		var s string
		if json.Unmarshal(b, &s) == nil {
			*m = messageText(s)
		}
	case len(b) > 0 && b[0] == '[':
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(b, &parts) != nil {
			return nil
		}
		var sb strings.Builder
		for _, p := range parts {
			if (p.Type == "text" || p.Type == "") && p.Text != "" {
				if sb.Len() > 0 {
					sb.WriteByte('\n')
				}
				sb.WriteString(p.Text)
			}
		}
		*m = messageText(sb.String())
	}
	return nil
}

// extractPromptText is what a request adds to its conversation: the messages
// after the last assistant message. For a chat turn that is the user's
// message, as before. For a coding agent's turn it is usually tool results,
// each labelled with the tool the assistant called; the last user message
// there is the task, the same on every turn, so showing it told nothing about
// the turn. With nothing after the assistant it falls back to the last user
// message, then to the legacy /v1/completions "prompt".
//
// Best-effort: a body that does not decode yields whatever was read before the
// error, and no error the caller has to handle.
func extractPromptText(body []byte) string {
	var p promptExtract
	json.Unmarshal(body, &p)
	msgs := p.Messages
	start := len(msgs)
	for start > 0 && msgs[start-1].Role != "assistant" {
		start--
	}
	if text := turnInput(msgs, start); text != "" {
		return text
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" && msgs[i].Content != "" {
			return string(msgs[i].Content)
		}
	}
	return p.Prompt
}

// turnInput renders msgs[start:], the turn's new messages. Without a tool
// result it is the last user message alone, unlabelled; with one, every
// tool result and user message in order, each under a label.
func turnInput(msgs []promptMessage, start int) string {
	tools, last := false, ""
	for _, m := range msgs[start:] {
		switch {
		case m.Role == "tool":
			tools = true
		case m.Role == "user" && m.Content != "":
			last = string(m.Content)
		}
	}
	if !tools {
		return last
	}
	var sb strings.Builder
	for _, m := range msgs[start:] {
		var label string
		switch m.Role {
		case "tool":
			label = "[tool result"
			if name := toolName(msgs, start, m.ToolCallID); name != "" {
				label += ": " + name
			}
			label += "]"
		case "user":
			label = "[user]"
		default:
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(label)
		sb.WriteByte('\n')
		sb.WriteString(string(m.Content))
	}
	return sb.String()
}

// toolName finds the name of the call a tool result answers, in the assistant
// message just before the turn.
func toolName(msgs []promptMessage, start int, id string) string {
	if start == 0 || id == "" {
		return ""
	}
	for _, c := range msgs[start-1].ToolCalls {
		if c.ID == id {
			return c.Function.Name
		}
	}
	return ""
}

// modelValueSpan returns the byte offsets of the top-level "model" key's string
// value, quotes included, without parsing the rest of the body. It reports
// false whenever it cannot be sure, and the caller falls back to decoding.
//
// It shares extractModelFast's reasoning. "model" must appear exactly once
// and no \u escape may appear anywhere, so the key cannot be spelled another
// way and there is no second model key for a full decode to prefer. The scan
// stops at the first nested value, so model has to come before messages —
// which every client library writes.
func modelValueSpan(body []byte) (start, end int, ok bool) {
	if bytes.Contains(body, escapePrefix) || bytes.Count(body, keyModelJSON) != 1 {
		return 0, 0, false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return 0, 0, false
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return 0, 0, false
		}
		keyEnd := int(dec.InputOffset()) // just past the key's closing quote
		valTok, err := dec.Token()
		if err != nil {
			return 0, 0, false
		}
		if _, nested := valTok.(json.Delim); nested {
			return 0, 0, false
		}
		if key, _ := keyTok.(string); key != "model" {
			continue
		}
		if _, isString := valTok.(string); !isString {
			return 0, 0, false
		}
		end = int(dec.InputOffset()) // just past the value's closing quote
		// Between the key and the value there is only whitespace and a colon,
		// so the first quote is the value's opening one.
		i := bytes.IndexByte(body[keyEnd:end], '"')
		if i < 0 {
			return 0, 0, false
		}
		return keyEnd + i, end, true
	}
	return 0, 0, false
}

// extractModelFast returns the value of a top-level "model" key without parsing
// the rest of the body, reporting false when it cannot do so safely.
//
// The motivation: handleProxy needs three small scalars, but json.Unmarshal must
// lex the entire document to produce them — including a prompt that can run to
// megabytes. Routing a 16K-token request cost ~762us of pure lexing before this.
//
// Three guards keep it honest, and any of them failing means the caller falls
// back to the full unmarshal:
//
//  1. "think" and "task" must be absent from the raw bytes. They are viiwork
//     extensions and almost never present; if either string appears anywhere,
//     even inside prompt text, we take the slow path rather than guess.
//  2. "model" must appear exactly once. json.Unmarshal resolves duplicate keys
//     to the LAST occurrence while an early-stopping scan would take the first,
//     so a body with two "model" keys must not use this path.
//  3. Scanning stops at the first non-scalar value. Skipping over a nested
//     array with the decoder would cost what we are trying to avoid, so if
//     "model" does not appear before "messages" there is nothing to win.
//
// Correctness rests on encoding/json's own lexer — this does not hand-roll JSON
// parsing, it just stops reading early.
func extractModelFast(body []byte) (string, bool) {
	if bytes.Contains(body, keyThinkJSON) || bytes.Contains(body, keyTaskJSON) {
		return "", false
	}
	// JSON permits unicode escapes in KEYS, so {"\u0074hink":true} is a valid
	// spelling of "think" that the byte scan above cannot see. Early-stopping
	// cannot rule out a later key either — by the time the decoder reaches an
	// escaped "think" we have already returned on "model". The byte scan is
	// therefore the only thing proving absence, and it must not be defeatable,
	// so any escape sequence anywhere disqualifies the fast path.
	//
	// Cost of being this strict: Python's json.dumps defaults to
	// ensure_ascii=True and escapes every non-ASCII character, so clients
	// sending non-English prompts fall back to the full unmarshal. That is the
	// pre-existing behaviour and always correct — just not faster.
	if bytes.Contains(body, escapePrefix) {
		return "", false
	}
	if bytes.Count(body, keyModelJSON) != 1 {
		return "", false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return "", false
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return "", false
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return "", false
		}
		key, _ := keyTok.(string)
		// The byte-level guards above cannot see keys written with JSON unicode
		// escapes — {"\u0074hink":true} is a valid spelling of "think" that
		// bytes.Contains will miss, and taking the fast path there would drop a
		// think/task the client really sent. dec.Token() has already decoded the
		// escape, so re-checking the decoded key closes the hole for free.
		if key == "think" || key == "task" {
			return "", false
		}
		valTok, err := dec.Token()
		if err != nil {
			return "", false
		}
		if d, isDelim := valTok.(json.Delim); isDelim {
			// Nested object or array: skipping it is the expense we are avoiding.
			_ = d
			return "", false
		}
		if key == "model" {
			s, ok := valTok.(string)
			return s, ok
		}
	}
	return "", false
}
