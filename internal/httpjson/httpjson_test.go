package httpjson

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, http.StatusCreated, map[string]int{"a": 1})
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	if got := rec.Body.String(); got != "{\"a\":1}\n" {
		t.Errorf("body = %q", got)
	}
}

func TestError(t *testing.T) {
	rec := httptest.NewRecorder()
	Error(rec, http.StatusTooManyRequests, "rate_limit", "no <free> slot")
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d", rec.Code)
	}
	// json.Encoder escapes HTML; the bytes must stay exactly what the proxy
	// has always sent.
	want := "{\"error\":{\"message\":\"no \\u003cfree\\u003e slot\",\"type\":\"rate_limit\"}}\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}
