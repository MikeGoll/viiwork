package proxy

import (
	"fmt"
	"strings"
	"testing"

	"github.com/janit/viiwork/v2/internal/activity"
)

// The early stop in extractOutputText is only lossless while maxOutputChars
// is no smaller than what the prompt store keeps.
func TestOutputCapMatchesThePromptStore(t *testing.T) {
	l := activity.NewLog()
	l.StorePrompt(1, "m", "p")
	l.StoreOutput(1, "m", strings.Repeat("a", maxOutputChars), 0)
	l.StorePrompt(2, "m", "p")
	l.StoreOutput(2, "m", strings.Repeat("a", maxOutputChars+1), 0)
	if e, _ := l.GetPrompt(1); e.Output != strings.Repeat("a", maxOutputChars) {
		t.Errorf("an output of maxOutputChars was cut to %d bytes: the store keeps less than maxOutputChars", len(e.Output))
	}
	if e, _ := l.GetPrompt(2); strings.HasPrefix(e.Output, strings.Repeat("a", maxOutputChars+1)) {
		t.Error("the store keeps more than maxOutputChars: the early stop would drop text it stores")
	}
}

// Stopping early keeps every byte the prompt store keeps, reasoning and
// answer alike.
func TestExtractOutputTextStopsEarlyWithoutLosingStoredText(t *testing.T) {
	var sse, reasoning, answer strings.Builder
	for i := 0; i < 4000; i++ {
		tok := fmt.Sprintf("r%d ", i)
		reasoning.WriteString(tok)
		sse.WriteString(`data: {"choices":[{"delta":{"reasoning_content":"` + tok + `"}}]}` + "\n\n")
	}
	for i := 0; i < 8000; i++ {
		tok := fmt.Sprintf("a%d ", i)
		answer.WriteString(tok)
		sse.WriteString(`data: {"choices":[{"delta":{"content":"` + tok + `"}}]}` + "\n\n")
	}
	sse.WriteString("data: [DONE]\n\n")
	full := "[reasoning]\n" + strings.TrimSpace(reasoning.String()) + "\n\n[answer]\n" + strings.TrimSpace(answer.String())
	if len(full) <= maxOutputChars {
		t.Fatalf("the fixture must exceed the cap: %d", len(full))
	}

	got := extractOutputText([]byte(sse.String()))
	if len(got) < maxOutputChars || got[:maxOutputChars] != full[:maxOutputChars] {
		t.Errorf("the first %d bytes differ from the full extraction (got %d bytes)", maxOutputChars, len(got))
	}
	if len(got) >= len(full) {
		t.Errorf("decoded all %d bytes; want an early stop", len(got))
	}
}
