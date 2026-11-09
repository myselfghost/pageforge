package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"regexp"
	"time"
)

const RequestIDHeader = "X-Request-Id"

type requestIDKey struct{}

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// WithRequestID stores id in ctx.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFromContext returns the request ID stored in ctx, or "".
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// RequestID reuses a valid incoming X-Request-Id or generates one, injects it
// into the request context, echoes it in the response, and logs one access line.
func RequestID(logger *slog.Logger, next http.Handler) http.Handler {
	return instrument(logger, nil, next)
}

// Instrument behaves like RequestID and also records the request in rec so the
// management API can display access logs and metrics.
func Instrument(logger *slog.Logger, rec *Recorder, next http.Handler) http.Handler {
	return instrument(logger, rec, next)
}

func instrument(logger *slog.Logger, rec *Recorder, next http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if !requestIDPattern.MatchString(id) {
			id = newRequestID()
		}
		w.Header().Set(RequestIDHeader, id)

		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(recorder, r.WithContext(WithRequestID(r.Context(), id)))
		elapsed := time.Since(start)

		logger.LogAttrs(r.Context(), slog.LevelInfo, "http request",
			slog.String("request_id", id),
			slog.String("method", r.Method),
			slog.String("host", r.Host),
			slog.String("path", r.URL.Path),
			slog.Int("status", recorder.status),
			slog.Int64("bytes", recorder.bytes),
			slog.Int64("duration_ms", elapsed.Milliseconds()),
		)
		if rec != nil {
			rec.Record(RequestEntry{
				Time:       start.UTC().Format(time.RFC3339),
				RequestID:  id,
				Method:     r.Method,
				Host:       r.Host,
				Path:       r.URL.Path,
				Status:     recorder.status,
				Bytes:      recorder.bytes,
				DurationMS: elapsed.Milliseconds(),
			})
		}
	})
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}
