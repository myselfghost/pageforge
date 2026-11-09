package observability

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewLoggerWritesJSON(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger("debug", &buf)
	logger.Debug("hello", "k", "v")

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("decode log: %v", err)
	}
	if entry["msg"] != "hello" || entry["level"] != "DEBUG" {
		t.Fatalf("unexpected entry: %v", entry)
	}
}

func TestRequestIDGeneratesAndLogs(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger("info", &buf)

	var seen string
	handler := RequestID(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok"))
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/url1", nil))

	if seen == "" {
		t.Fatal("request id missing from context")
	}
	if got := rec.Header().Get(RequestIDHeader); got != seen {
		t.Fatalf("header = %q, context = %q", got, seen)
	}
	if !strings.Contains(buf.String(), seen) {
		t.Fatalf("log line does not contain request id: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"status":201`) {
		t.Fatalf("log line does not contain status: %s", buf.String())
	}
}

func TestRequestIDReusesValidHeader(t *testing.T) {
	handler := RequestID(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(RequestIDHeader, "abc-123")

	handler.ServeHTTP(rec, req)
	if got := rec.Header().Get(RequestIDHeader); got != "abc-123" {
		t.Fatalf("header = %q, want abc-123", got)
	}
}

func TestRequestIDRejectsInvalidHeader(t *testing.T) {
	handler := RequestID(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(RequestIDHeader, "bad id with spaces")

	handler.ServeHTTP(rec, req)
	got := rec.Header().Get(RequestIDHeader)
	if got == "bad id with spaces" || got == "" {
		t.Fatalf("invalid header should be replaced, got %q", got)
	}
}
