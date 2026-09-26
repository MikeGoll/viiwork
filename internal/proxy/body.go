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
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Prompt string `json:"prompt"`
}

// extractPromptText is best-effort: a body with multimodal content parts (an
// array instead of a plain string) fails to decode into Content for that one
// message, same as elsewhere in this file, and simply yields no text there
// rather than an error the caller has to handle.
func extractPromptText(body []byte) string {
	var p promptExtract
	json.Unmarshal(body, &p)
	for i := len(p.Messages) - 1; i >= 0; i-- {
		if p.Messages[i].Role == "user" && p.Messages[i].Content != "" {
			return p.Messages[i].Content
		}
	}
	return p.Prompt
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
