package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"
)

// Performance measurement on the executing node (performance-routing spec
// §1, §4): ask the engine for usage, strip the usage chunk again for a client
// that did not ask, and note when the first content chunk left for the
// client. Everything here runs per request, or per write until the first
// token; after that a write costs one bool test (strip off) or one
// append-and-scan into a reused buffer (strip on). No allocation per token.

var keyStreamJSON = []byte(`"stream"`)

const includeUsageMember = `"stream_options":{"include_usage":true},`

// injectIncludeUsage sets stream_options.include_usage on a streamed request
// that did not ask for usage. injected reports that the caller must strip the
// usage chunk from the response, because the client never asked for it.
func injectIncludeUsage(body []byte) ([]byte, bool) {
	if !bytes.Contains(body, keyStreamJSON) {
		return body, false
	}
	var s struct {
		Stream        bool                       `json:"stream"`
		StreamOptions map[string]json.RawMessage `json:"stream_options"`
	}
	if json.Unmarshal(body, &s) != nil || !s.Stream {
		return body, false
	}
	if v, ok := s.StreamOptions["include_usage"]; ok && bytes.Equal(bytes.TrimSpace(v), []byte("true")) {
		return body, false
	}
	if s.StreamOptions != nil {
		// Rare: stream_options without include_usage:true. Rebuild the object.
		var generic map[string]json.RawMessage
		if json.Unmarshal(body, &generic) != nil {
			return body, false
		}
		s.StreamOptions["include_usage"] = json.RawMessage("true")
		so, err := json.Marshal(s.StreamOptions)
		if err != nil {
			return body, false
		}
		generic["stream_options"] = so
		out, err := json.Marshal(generic)
		if err != nil {
			return body, false
		}
		return out, true
	}
	i := bytes.IndexByte(body, '{')
	if i < 0 {
		return body, false
	}
	out := make([]byte, 0, len(body)+len(includeUsageMember))
	out = append(out, body[:i+1]...)
	out = append(out, includeUsageMember...)
	return append(out, body[i+1:]...), true
}

// measureWriter sits between a local backend's stream and the client.
type measureWriter struct {
	http.ResponseWriter
	now      func() time.Time
	strip    bool
	carry    []byte    // strip mode: bytes of an event not yet complete
	firstAt  time.Time // when the first content chunk was written; zero = never
	stripped []byte    // the usage-only event removed for the client, if any
}

type flushMeasureWriter struct {
	*measureWriter
	f http.Flusher
}

func (m *flushMeasureWriter) Flush() { m.f.Flush() }

// newMeasureWriter wraps w, preserving whether it can flush (see
// flushCaptureWriter for why that matters).
func newMeasureWriter(w http.ResponseWriter, strip bool, now func() time.Time) (http.ResponseWriter, *measureWriter) {
	m := &measureWriter{ResponseWriter: w, strip: strip, now: now}
	if f, ok := w.(http.Flusher); ok {
		return &flushMeasureWriter{measureWriter: m, f: f}, m
	}
	return m, m
}

func (m *measureWriter) Unwrap() http.ResponseWriter { return m.ResponseWriter }

func (m *measureWriter) Write(p []byte) (int, error) {
	if m.firstAt.IsZero() && hasTokenText(p) {
		m.firstAt = m.now()
	}
	if !m.strip {
		return m.ResponseWriter.Write(p)
	}
	m.carry = append(m.carry, p...)
	start := 0
	for {
		i := bytes.Index(m.carry[start:], sseEventEnd)
		if i < 0 {
			break
		}
		end := start + i + len(sseEventEnd)
		ev := m.carry[start:end]
		if isUsageOnlyEvent(ev) {
			m.stripped = append(m.stripped[:0], ev...)
		} else if _, err := m.ResponseWriter.Write(ev); err != nil {
			return 0, err
		}
		start = end
	}
	n := copy(m.carry, m.carry[start:])
	m.carry = m.carry[:n]
	return len(p), nil
}

// finish writes an incomplete final event, so stripping never loses bytes.
func (m *measureWriter) finish() error {
	if !m.strip || len(m.carry) == 0 {
		return nil
	}
	_, err := m.ResponseWriter.Write(m.carry)
	m.carry = m.carry[:0]
	return err
}

var (
	sseEventEnd   = []byte("\n\n")
	emptyChoices  = []byte(`"choices":[]`)
	tokenTextKeys = [][]byte{[]byte(`"content":"`), []byte(`"reasoning_content":"`), []byte(`"text":"`)}
	toolCallsKey  = []byte(`"tool_calls":[`)
)

func isUsageOnlyEvent(ev []byte) bool {
	ev = bytes.TrimSpace(ev)
	return bytes.HasPrefix(ev, sseDataPrefix) && bytes.Contains(ev, emptyChoices) && bytes.Contains(ev, usageKey)
}

// hasTokenText reports whether p carries generated text: a non-empty content,
// reasoning or completion text value, or a non-empty tool_calls array. A role-only opening
// chunk, which vLLM sends before any prefill, does not count.
func hasTokenText(p []byte) bool {
	for rest := p; ; {
		i := bytes.Index(rest, toolCallsKey)
		if i < 0 {
			break
		}
		rest = bytes.TrimLeft(rest[i+len(toolCallsKey):], " \t\r\n")
		// An empty array is no call: counting it would measure a TTFT of
		// about zero and make the host look free.
		if len(rest) > 0 && rest[0] != ']' {
			return true
		}
	}
	for _, k := range tokenTextKeys {
		for rest := p; ; {
			i := bytes.Index(rest, k)
			if i < 0 {
				break
			}
			rest = rest[i+len(k):]
			if len(rest) > 0 && rest[0] != '"' {
				return true
			}
		}
	}
	return false
}
