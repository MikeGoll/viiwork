package proxy

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// cutEngine sends one SSE chunk and then drops the connection mid-stream.
func cutEngine(chunk string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chunk)
		w.(http.Flusher).Flush()
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close()
		}
	}
}

const cutReasoningChunk = "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"one\"}}]}\n\n"

// A local backend that dies mid-stream must not look like a finished response:
// the handler aborts the connection, closes the dashboard row as aborted, and
// gives the slot back — on both the plain and the think-rewriting stream.
func TestHandlerLocalStreamCutIsAborted(t *testing.T) {
	cases := map[string]string{
		"think enabled":  `{"model":"m","think":true,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		"think disabled": `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			f := newHandlerFx(t)
			bs := f.local.add("m", newRecEngine(t, cutEngine(cutReasoningChunk)).addr())
			f.build()
			rec := f.do(http.MethodPost, "/v1/chat/completions", body)
			if !f.aborted {
				t.Errorf("a cut stream ended cleanly: code=%d body=%q", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "one") {
				t.Errorf("the chunk before the cut was not passed on: %q", rec.Body.String())
			}
			evs := f.requestEvents()
			if len(evs) != 2 || !strings.Contains(evs[1].Message, "aborted") {
				t.Errorf("request events = %+v, want started then aborted", evs)
			}
			if n := bs[0].InFlight(); n != 0 {
				t.Errorf("slot not released: in flight = %d", n)
			}
			if _, ok := f.log.GetPrompt(evs[0].RequestID); !ok {
				t.Error("the aborted request left no prompt history")
			}
		})
	}
}

// Through a real server the abort reaches the client as a broken response,
// not a clean end of stream.
func TestHandlerStreamCutReachesTheClient(t *testing.T) {
	f := newHandlerFx(t)
	f.local.add("m", newRecEngine(t, cutEngine(chunkOne)).addr())
	f.build()
	front := httptest.NewServer(f.h)
	t.Cleanup(front.Close)
	resp, err := http.Post(front.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m","think":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Errorf("the client read a complete response: %q", body)
	}
	if !strings.Contains(string(body), "one") {
		t.Errorf("body before the cut = %q", body)
	}
}

// A line longer than the think rewriter's cap ends the stream in error rather
// than silently.
func TestStreamThinkDisabledLineTooLong(t *testing.T) {
	sse := chunkOne + "data: " + strings.Repeat("x", maxSSELine+1) + "\n\n" + chunkTwo
	rec := httptest.NewRecorder()
	aborted, err := streamThinkDisabled(rec, strings.NewReader(sse), func() {})
	if aborted || !errors.Is(err, bufio.ErrTooLong) {
		t.Errorf("aborted=%v err=%v, want bufio.ErrTooLong", aborted, err)
	}
	if strings.Contains(rec.Body.String(), "two") {
		t.Error("the stream went on past the line it could not read")
	}
}

func TestStreamReportsUpstreamErrors(t *testing.T) {
	boom := errors.New("boom")
	rec := httptest.NewRecorder()
	if gone, err := stream(rec, io.MultiReader(strings.NewReader(chunkOne), errAfter{boom}), func() {}); gone || !errors.Is(err, boom) {
		t.Errorf("gone=%v err=%v, want boom", gone, err)
	}
	if gone, err := stream(httptest.NewRecorder(), strings.NewReader(chunkOne), func() {}); gone || err != nil {
		t.Errorf("clean EOF: gone=%v err=%v", gone, err)
	}
	if _, err := streamThinkDisabled(httptest.NewRecorder(), io.MultiReader(strings.NewReader(chunkOne), errAfter{boom}), func() {}); !errors.Is(err, boom) {
		t.Errorf("think stream: err=%v, want boom", err)
	}
}

type errAfter struct{ err error }

func (e errAfter) Read([]byte) (int, error) { return 0, e.err }

// A panic while a request runs must not leak its slot: the lease is released
// on every path out, and the dashboard row is closed as aborted.
func TestHandlerPanicReleasesTheLease(t *testing.T) {
	f := newHandlerFx(t)
	bs := f.local.add("m", closedAddr(t))
	bs[0].panicOnHard = true // a stand-in for any bug on the execution path
	f.build()

	func() {
		defer func() {
			if rv := recover(); rv != "boom" {
				t.Errorf("recovered %v, want the backend's panic", rv)
			}
		}()
		f.h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatReq)))
	}()
	if n := bs[0].InFlight(); n != 0 {
		t.Errorf("slot leaked: in flight = %d", n)
	}
	evs := f.requestEvents()
	if len(evs) != 2 || !strings.Contains(evs[1].Message, "aborted") {
		t.Errorf("request events = %+v, want started then aborted", evs)
	}
}
