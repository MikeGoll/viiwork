package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// runThinkStream is streamThinkDisabled's output for sse, with the rename
// fast path on or off.
func runThinkStream(t *testing.T, sse string, fast bool) string {
	t.Helper()
	old := renameFastPath
	renameFastPath = fast
	defer func() { renameFastPath = old }()
	w := httptest.NewRecorder()
	if _, err := streamThinkDisabled(w, strings.NewReader(sse), func() {}); err != nil {
		t.Fatal(err)
	}
	return w.Body.String()
}

// sameFrames compares two SSE outputs line by line, data payloads as JSON
// values: the slow path re-encodes with sorted keys, the fast path keeps the
// engine's bytes, and both must say the same thing.
func sameFrames(t *testing.T, fast, slow string) {
	t.Helper()
	fl, sl := strings.Split(fast, "\n"), strings.Split(slow, "\n")
	if len(fl) != len(sl) {
		t.Fatalf("line count differs:\nfast %q\nslow %q", fast, slow)
	}
	for i := range fl {
		f, s := fl[i], sl[i]
		if !strings.HasPrefix(f, "data: {") || !strings.HasPrefix(s, "data: {") {
			if f != s {
				t.Errorf("line %d: fast %q, slow %q", i, f, s)
			}
			continue
		}
		var fv, sv any
		if err := json.Unmarshal([]byte(f[len("data: "):]), &fv); err != nil {
			t.Fatalf("fast line %d: %v", i, err)
		}
		if err := json.Unmarshal([]byte(s[len("data: "):]), &sv); err != nil {
			t.Fatalf("slow line %d: %v", i, err)
		}
		if !reflect.DeepEqual(fv, sv) {
			t.Errorf("line %d differs:\nfast %s\nslow %s", i, f, s)
		}
	}
}

// The rename fast path must produce what the slow path produces, for the
// chunks it takes and for the ones it must leave to the slow path.
func TestStreamThinkDisabledRenameFastPathMatchesSlowPath(t *testing.T) {
	chunks := []string{
		`{"id":"a","created":1700000000,"choices":[{"index":0,"delta":{"reasoning_content":"plain token"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"role":"assistant","reasoning_content":"with \"quotes\" and \\ backslash"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"</think> stray close outside a block"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"html <b>&amp;</b> stays"}}],"usage":null}`,
		`{"choices":[{"delta":{"reasoning_content":""}}]}`,
		`{"choices":[{"delta":{"reasoning_content" : "spaced key"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"stop here"},"finish_reason":"stop"}]}`,
		// Left to the slow path:
		`{"choices":[{"delta":{"reasoning_content":"ä escaped"}}]}`,
		`{"choices":[{"delta":{"content":"x","reasoning_content":"y"}}]}`,
		`{"choices":[{"delta":{"content":null,"reasoning_content":"null content"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"a"}},{"delta":{"reasoning_content":"b"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"say \"reasoning_content\" twice"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":7}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"<think>opens"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"inside"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"</think>after"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"tail"}}]}`,
	}
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: " + c + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	sse := b.String()
	fast, slow := runThinkStream(t, sse, true), runThinkStream(t, sse, false)
	sameFrames(t, fast, slow)
	if !strings.Contains(fast, `{"id":"a","created":1700000000,"choices":[{"index":0,"delta":{"content":"plain token"},"finish_reason":null}]}`) {
		t.Errorf("the fast path did not keep the engine's bytes: %q", fast)
	}
}

func TestUntaggedReasoningKeyGuards(t *testing.T) {
	takes := `{"choices":[{"delta":{"reasoning_content":"x"}}]}`
	if at, ok := untaggedReasoningKey([]byte(takes)); !ok || !strings.HasPrefix(takes[at:], `"reasoning_content"`) {
		t.Errorf("plain chunk: at=%d ok=%v", at, ok)
	}
	for _, c := range []string{
		`{"choices":[{"delta":{"reasoning_content":"<think>"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"<think>"}}]}`,
		`{"choices":[{"delta":{"content":"a","reasoning_content":"b"}}]}`,
		`{"choices":[{"delta":{"content":null,"reasoning_content":"b"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":null}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"a"}},{"delta":{"reasoning_content":"b"}}]}`,
		`{"choices":[{"delta":null,"message":{"reasoning_content":"a"}}]}`,
		`not json "reasoning_content"`,
	} {
		if _, ok := untaggedReasoningKey([]byte(c)); ok {
			t.Errorf("fast path taken for %s", c)
		}
	}
}
