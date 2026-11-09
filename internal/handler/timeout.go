package handler

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"
)

// clientTimeout derives the request context used for the client-facing side.
//
// The idle watchdog is armed per request through the response writer (see
// serveEntry): every successful Write resets a timer, so a slow but steady
// download never trips it while a stalled client does. An optional total
// timeout caps the whole request regardless of traffic.
//
// The returned context is cancelled when either limit fires or when the caller
// invokes the returned cancel function.
func (h *Handler) clientTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	idle := h.opts.ClientIdleTimeout
	total := h.opts.ClientTotalTimeout
	var cancel context.CancelFunc
	if total > 0 {
		ctx, cancel = context.WithTimeout(ctx, total)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	if idle > 0 {
		ctx = context.WithValue(ctx, idleKey{}, idleConfig{timeout: idle, cancel: cancel})
	}
	return ctx, cancel
}

// idleKey carries the client idle configuration through the request context.
type idleKey struct{}

type idleConfig struct {
	timeout time.Duration
	cancel  context.CancelFunc
}

// clientWriter wraps w so that each write resets the client idle watchdog.
// When no idle timeout is configured it returns w unchanged.
func clientWriter(ctx context.Context, w http.ResponseWriter) (http.ResponseWriter, func()) {
	cfg, ok := ctx.Value(idleKey{}).(idleConfig)
	if !ok || cfg.timeout <= 0 {
		return w, func() {}
	}
	iw := &idleWriter{ResponseWriter: w, timeout: cfg.timeout, cancel: cfg.cancel}
	iw.reset()
	return iw, iw.stop
}

// idleWriter resets a watchdog timer on every successful Write.
type idleWriter struct {
	http.ResponseWriter
	timeout time.Duration
	cancel  context.CancelFunc

	mu    sync.Mutex
	timer *time.Timer
}

func (w *idleWriter) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer == nil {
		w.timer = time.AfterFunc(w.timeout, w.cancel)
		return
	}
	w.timer.Reset(w.timeout)
}

func (w *idleWriter) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		w.timer.Stop()
	}
}

func (w *idleWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	if n > 0 {
		w.reset()
	}
	return n, err
}

// idleBody wraps an upstream object body so that each successful read resets an
// idle watchdog; if no bytes arrive within timeout the cancel function fires.
type idleBody struct {
	io.ReadCloser
	timeout time.Duration
	cancel  context.CancelFunc

	mu    sync.Mutex
	timer *time.Timer
}

func newIdleBody(body io.ReadCloser, timeout time.Duration, cancel context.CancelFunc) *idleBody {
	if timeout <= 0 {
		return &idleBody{ReadCloser: body}
	}
	b := &idleBody{ReadCloser: body, timeout: timeout, cancel: cancel}
	b.mu.Lock()
	b.timer = time.AfterFunc(timeout, cancel)
	b.mu.Unlock()
	return b
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.mu.Lock()
		if b.timer != nil {
			b.timer.Reset(b.timeout)
		}
		b.mu.Unlock()
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.mu.Lock()
	if b.timer != nil {
		b.timer.Stop()
	}
	b.mu.Unlock()
	return b.ReadCloser.Close()
}
