package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/engine"
	"github.com/janit/viiwork/v2/internal/perf"
	"github.com/janit/viiwork/v2/internal/route"
	"github.com/janit/viiwork/v2/mesh/capacity"
)

type fakePerf struct {
	model    string
	uncached int64
	ttft     time.Duration
	calls    int
	score    perf.Score
	has      bool
}

func (f *fakePerf) Record(model string, _ time.Time, uncached int64, ttft time.Duration) {
	f.model, f.uncached, f.ttft = model, uncached, ttft
	f.calls++
}
func (f *fakePerf) Score(string) (perf.Score, bool) { return f.score, f.has }

func capturedWith(t *testing.T, body string) *captureWriter {
	t.Helper()
	w, c := newCaptureWriter(httptest.NewRecorder())
	w.Write([]byte(body))
	return c
}

func newTestRouter(t *testing.T) *route.Router {
	t.Helper()
	return route.New(route.Config{Self: "a", Local: noLocal{}, Remote: noReports{}, StaleAfter: time.Second, QueueMax: 1, QueueTimeout: time.Second})
}

type noLocal struct{}

func (noLocal) Backends(string) ([]route.LocalBackend, bool, bool) { return nil, false, false }

type noReports struct{}

func (noReports) Reports() []capacity.Report { return nil }

func TestRecordPerf(t *testing.T) {
	granted := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	first := granted.Add(1500 * time.Millisecond)
	ok := execResult{Outcome: outcomeServed, Status: http.StatusOK, Granted: granted, FirstToken: first}

	cases := []struct {
		name     string
		res      execResult
		captured string
		usage    engine.UsageReporting
		wantCall bool
		wantUnc  int64
	}{
		{"stripped usage", func() execResult { r := ok; r.StrippedUsage = []byte(usageEvent); return r }(), "", engine.UsageReporting{CachedTokens: true}, true, 3000},
		{"usage from capture", ok, usageEvent, engine.UsageReporting{CachedTokens: true}, true, 3000},
		{"no first token", func() execResult { r := ok; r.FirstToken = time.Time{}; return r }(), usageEvent, engine.UsageReporting{CachedTokens: true}, false, 0},
		{"no grant", func() execResult { r := ok; r.Granted = time.Time{}; return r }(), usageEvent, engine.UsageReporting{CachedTokens: true}, false, 0},
		{"error status", func() execResult { r := ok; r.Status = 500; return r }(), usageEvent, engine.UsageReporting{CachedTokens: true}, false, 0},
		{"no usage at all", ok, "data: [DONE]\n\n", engine.UsageReporting{CachedTokens: true}, false, 0},
		{"cached absent, engine cannot say", ok, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10}}\n\n", engine.UsageReporting{}, false, 0},
		{"cached absent means zero", ok, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10}}\n\n", engine.UsageReporting{CachedTokens: true}, true, 10},
		// Review Focus 4: an engine quirk must not record a negative sample.
		{"cached exceeds prompt", ok, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"prompt_tokens_details\":{\"cached_tokens\":50}}}\n\n", engine.UsageReporting{CachedTokens: true}, false, 0},
	}
	for _, tc := range cases {
		fp := &fakePerf{}
		u := tc.usage
		h := NewHandler(Deps{Perf: fp, Usage: func(string) engine.UsageReporting { return u }})
		h.recordPerf("m", tc.res, capturedWith(t, tc.captured))
		if (fp.calls == 1) != tc.wantCall {
			t.Errorf("%s: recorded = %v, want %v", tc.name, fp.calls == 1, tc.wantCall)
			continue
		}
		if tc.wantCall && (fp.model != "m" || fp.uncached != tc.wantUnc || fp.ttft != 1500*time.Millisecond) {
			t.Errorf("%s: recorded %q uncached %d ttft %v", tc.name, fp.model, fp.uncached, fp.ttft)
		}
	}
}

func TestRecordPerfWithoutRecorder(t *testing.T) {
	h := NewHandler(Deps{})
	ok := execResult{Outcome: outcomeServed, Status: http.StatusOK, Granted: time.Now(), FirstToken: time.Now()}
	h.recordPerf("m", ok, capturedWith(t, usageEvent)) // must not panic
}

func TestCapacityPublishesScoreOnlyWhenEnabled(t *testing.T) {
	fp := &fakePerf{score: perf.Score{OverheadMs: 400, MsPer1k: 5100, Samples: 12}, has: true}
	for _, publish := range []bool{true, false} {
		h := NewHandler(Deps{Self: "a", Router: newTestRouter(t), Local: fakeCapacity{{Name: "m", Slots: 2}}, Perf: fp, PublishPerf: publish})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/capacity", nil))
		got := rec.Body.String()
		has := strings.Contains(got, `"prefill_ms_per_1k":5100`)
		if has != publish {
			t.Errorf("publish=%v: body %s", publish, got)
		}
	}
}

func TestCompletionTokensFromStrippedEvent(t *testing.T) {
	if n, ok := completionTokensFromEvent([]byte(usageEvent)); !ok || n != 9 {
		t.Fatalf("got %d %v; tokens_total must still count a request whose usage chunk was stripped", n, ok)
	}
	if _, ok := completionTokensFromEvent(nil); ok {
		t.Fatal("no event, no tokens")
	}
}

// Ruling 2: include_usage is injected only when measuring and the engine both
// needs asking and reports cached tokens; any other engine would never yield a
// sample, so its body stays as the client sent it.
func TestLocalBodyForGate(t *testing.T) {
	const body = `{"model":"m","stream":true}`
	cases := []struct {
		name      string
		perf      bool
		usage     *engine.UsageReporting // nil = Deps.Usage unset
		wantStrip bool
	}{
		{"not measuring", false, &engine.UsageReporting{CachedTokens: true}, false},
		{"measuring, cached tokens reported", true, &engine.UsageReporting{CachedTokens: true}, true},
		{"measuring, zero value", true, &engine.UsageReporting{}, false},
		{"measuring, Usage unset", true, nil, false},
		{"measuring, reports unasked", true, &engine.UsageReporting{Unasked: true, CachedTokens: true}, false},
	}
	for _, tc := range cases {
		d := Deps{}
		if tc.perf {
			d.Perf = &fakePerf{}
		}
		if tc.usage != nil {
			u := *tc.usage
			d.Usage = func(string) engine.UsageReporting { return u }
		}
		h := NewHandler(d)
		disp := &dispatch{model: "m", body: []byte(body)}
		got, strip := h.localBodyFor(disp)
		if strip != tc.wantStrip {
			t.Errorf("%s: strip = %v, want %v", tc.name, strip, tc.wantStrip)
		}
		injected := strings.Contains(string(got), `"include_usage":true`)
		if injected != tc.wantStrip {
			t.Errorf("%s: body %s", tc.name, got)
		}
		if !tc.wantStrip && string(got) != body {
			t.Errorf("%s: body changed: %s", tc.name, got)
		}
		// Built once per request: a second attempt reuses it.
		again, strip2 := h.localBodyFor(disp)
		if strip2 != strip || string(again) != string(got) {
			t.Errorf("%s: second call differs", tc.name)
		}
	}
}

// A streamed local serve with strip on: the client never sees the usage
// chunk it did not ask for, and the result carries the measurement.
func TestServeLocalMeasuresAndStrips(t *testing.T) {
	const content = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
	eng := engineServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, content+usageEvent+"data: [DONE]\n\n")
	})
	rec := httptest.NewRecorder()
	res := serveLocal(rec, chatRequest("/v1/chat/completions", "{}"), []byte("{}"), backendFor("m/0", eng), "m", "self", false, true)
	if res.Outcome != outcomeServed || res.Status != 200 {
		t.Fatalf("res=%+v", res)
	}
	if got := rec.Body.String(); got != content+"data: [DONE]\n\n" {
		t.Errorf("client body %q", got)
	}
	if res.FirstToken.IsZero() || string(res.StrippedUsage) != usageEvent {
		t.Errorf("FirstToken %v, StrippedUsage %q", res.FirstToken, res.StrippedUsage)
	}
}

// An engine that sends Content-Length on a streamed response: stripping the
// usage chunk shortens the body, so the upstream length must not reach the
// client, or it would wait for bytes that never come.
func TestServeLocalStripDropsContentLength(t *testing.T) {
	const content = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
	full := content + usageEvent + "data: [DONE]\n\n"
	eng := engineServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(full)))
		_, _ = io.WriteString(w, full)
	})
	rec := httptest.NewRecorder()
	serveLocal(rec, chatRequest("/v1/chat/completions", "{}"), []byte("{}"), backendFor("m/0", eng), "m", "self", false, true)
	if got := rec.Body.String(); got != content+"data: [DONE]\n\n" {
		t.Errorf("client body %q", got)
	}
	if cl := rec.Header().Get("Content-Length"); cl != "" {
		t.Errorf("Content-Length %q forwarded for a %d-byte stripped body", cl, rec.Body.Len())
	}
}

// A non-SSE body has no first-token moment even though it carries content.
func TestServeLocalJSONHasNoFirstToken(t *testing.T) {
	eng := engineServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}]}`)
	})
	rec := httptest.NewRecorder()
	res := serveLocal(rec, chatRequest("/v1/chat/completions", "{}"), []byte("{}"), backendFor("m/0", eng), "m", "self", false, false)
	if !res.FirstToken.IsZero() {
		t.Errorf("FirstToken = %v on a JSON response", res.FirstToken)
	}
}

// Ruling 4: with strip on, a body that never completes an SSE event sits in
// the carry buffer; every path that wrote headers must flush it, or the
// client gets an empty or short body.
func TestServeLocalStripDeliversNonSSEBodies(t *testing.T) {
	const errBody = `{"error":{"message":"bad request","code":400}}`
	eng := engineServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = io.WriteString(w, errBody)
	})
	for _, think := range []bool{false, true} {
		rec := httptest.NewRecorder()
		res := serveLocal(rec, chatRequest("/v1/chat/completions", "{}"), []byte("{}"), backendFor("m/0", eng), "m", "self", think, true)
		want := errBody
		if think {
			want = string(rewriteThinkResponse([]byte(errBody)))
		}
		if res.Outcome != outcomeServed || rec.Code != 400 || rec.Body.String() != want {
			t.Errorf("think=%v: res=%+v code=%d body=%q", think, res, rec.Code, rec.Body.String())
		}
	}
}

// The dispatch and attempt wiring end to end: a streamed request from a
// client that did not ask for usage is asked for usage at the engine, the
// usage chunk is stripped before the client sees it, tokens_total still
// counts its completion tokens, and one sample is recorded with a grant time.
func TestHandlerMeasuresLocalStream(t *testing.T) {
	const content = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
	gotBody := make(chan string, 1)
	eng := engineServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody <- string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		time.Sleep(2 * time.Millisecond) // a prefill, so the TTFT is visibly positive
		_, _ = io.WriteString(w, content+usageEvent+"data: [DONE]\n\n")
	})
	fp := &fakePerf{}
	f := newHandlerFx(t)
	f.local.add("m", eng.Listener.Addr().String())
	f.perf = fp
	f.usage = func(string) engine.UsageReporting { return engine.UsageReporting{CachedTokens: true} }
	f.build()

	rec := f.do(http.MethodPost, "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	if b := <-gotBody; !strings.Contains(b, `"include_usage":true`) {
		t.Errorf("the engine was not asked for usage: %s", b)
	}
	// No think field means thinking disabled, so the stream goes through
	// streamThinkDisabled, which re-emits line by line (its own blank-line
	// framing); assert on the events rather than the exact bytes.
	if got := rec.Body.String(); strings.Contains(got, `"usage"`) || !strings.HasPrefix(got, content) || !strings.Contains(got, "data: [DONE]\n\n") {
		t.Errorf("the client got %q", got)
	}
	if got := f.counters.Get("m"); got != (ModelCounters{Requests: 1, Tokens: 9}) {
		t.Errorf("counters %+v: a stripped usage chunk must still count its completion tokens", got)
	}
	if fp.calls != 1 || fp.model != "m" || fp.uncached != 3000 || fp.ttft <= 0 {
		t.Errorf("recorded calls=%d model=%q uncached=%d ttft=%v", fp.calls, fp.model, fp.uncached, fp.ttft)
	}
}
