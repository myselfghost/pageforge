package handler_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pageforge/pageforge/internal/handler"
	"github.com/pageforge/pageforge/internal/manifest"
	"github.com/pageforge/pageforge/internal/objectstore"
)

// pacingStore wraps a MemoryStore and returns a body that emits chunks with a
// configurable gap between reads, so we can simulate a slow-but-flowing upstream
// versus a fully stalled one.
type pacingStore struct {
	objectstore.ObjectStore
	chunk      int
	gap        time.Duration
	stallAfter int // emit this many chunks then block forever (0 = never stall)
	reads      int32
}

func (s *pacingStore) Get(ctx context.Context, bucket, key string, opt objectstore.GetOptions) (io.ReadCloser, objectstore.ObjectInfo, error) {
	body, info, err := s.ObjectStore.Get(ctx, bucket, key, opt)
	if err != nil || !strings.HasPrefix(key, "projects/") {
		return body, info, err
	}
	if s.chunk <= 0 {
		s.chunk = 4
	}
	if s.gap <= 0 {
		s.gap = 20 * time.Millisecond
	}
	return &pacingBody{ctx: ctx, data: mustReadAll(body), chunk: s.chunk, gap: s.gap, stallAfter: s.stallAfter, reads: &s.reads}, info, nil
}

func mustReadAll(rc io.ReadCloser) []byte {
	defer func() { _ = rc.Close() }()
	b, _ := io.ReadAll(rc)
	return b
}

type pacingBody struct {
	ctx        context.Context
	data       []byte
	chunk      int
	gap        time.Duration
	stallAfter int
	reads      *int32
	pos        int
}

func (b *pacingBody) Read(p []byte) (int, error) {
	n := atomic.AddInt32(b.reads, 1)
	if b.stallAfter > 0 && int(n) > b.stallAfter {
		// Simulate a read that never returns until the request context is
		// cancelled (mirrors a blocked network read on a stalled S3 body).
		<-b.ctx.Done()
		return 0, b.ctx.Err()
	}
	if b.pos >= len(b.data) {
		return 0, io.EOF
	}
	// Honor context cancellation during the inter-chunk gap, like a real body
	// read that aborts when the request is cancelled.
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-time.After(b.gap):
	}
	end := b.pos + b.chunk
	if end > len(b.data) {
		end = len(b.data)
	}
	copied := copy(p, b.data[b.pos:end])
	b.pos += copied
	return copied, nil
}

func (b *pacingBody) Close() error { return nil }

func timeoutFixture(t *testing.T, store objectstore.ObjectStore, tune func(*handler.Options)) *httptest.ResponseRecorder {
	t.Helper()
	f := newFixture(t, nil)
	opts := fixtureOptions(t)
	opts.Snapshots = f.snapshots
	opts.Stores = &fakeProvider{store: store}
	opts.Manifests = manifest.NewReader()
	if tune != nil {
		tune(&opts)
	}
	h := handler.New(opts)
	req := httptest.NewRequest(http.MethodGet, "http://"+testHost+"/big.txt", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// A slow-but-flowing upstream must survive even when the total transfer time
// exceeds the idle timeout, because every read resets the idle watchdog.
func TestUpstreamIdleTimeoutAllowsSlowFlow(t *testing.T) {
	f := newFixture(t, nil)
	store := &pacingStore{ObjectStore: f.wrapped, chunk: 1, gap: 30 * time.Millisecond}
	rec := timeoutFixture(t, store, func(o *handler.Options) {
		o.UpstreamIdleTimeout = 100 * time.Millisecond
		o.UpstreamTotalTimeout = 0
		o.ClientIdleTimeout = 5 * time.Second
		o.ClientTotalTimeout = 0
	})
	// "0123456789" = 10 chunks * 30ms = 300ms total, well over the 100ms idle.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "0123456789" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

// A stalled upstream (no bytes for longer than the idle timeout) must abort.
func TestUpstreamIdleTimeoutAbortsStall(t *testing.T) {
	f := newFixture(t, nil)
	store := &pacingStore{ObjectStore: f.wrapped, chunk: 2, gap: 10 * time.Millisecond, stallAfter: 1}
	start := time.Now()
	rec := timeoutFixture(t, store, func(o *handler.Options) {
		o.UpstreamIdleTimeout = 150 * time.Millisecond
		o.UpstreamTotalTimeout = 0
		o.ClientIdleTimeout = 5 * time.Second
		o.ClientTotalTimeout = 0
	})
	elapsed := time.Since(start)
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("abort took %v, expected the idle watchdog to fire quickly", elapsed)
	}
	// The response must not be a complete, valid body.
	if rec.Body.Len() >= len("0123456789") {
		t.Fatalf("expected truncated body, got %q", rec.Body.String())
	}
}

// A hard total ceiling must fire even while data keeps flowing.
func TestUpstreamTotalTimeoutFiresDuringFlow(t *testing.T) {
	f := newFixture(t, nil)
	store := &pacingStore{ObjectStore: f.wrapped, chunk: 1, gap: 40 * time.Millisecond}
	start := time.Now()
	rec := timeoutFixture(t, store, func(o *handler.Options) {
		o.UpstreamIdleTimeout = 5 * time.Second // generous: must NOT be the trigger
		o.UpstreamTotalTimeout = 120 * time.Millisecond
		o.ClientIdleTimeout = 5 * time.Second
		o.ClientTotalTimeout = 0
	})
	elapsed := time.Since(start)
	if elapsed > 1000*time.Millisecond {
		t.Fatalf("total ceiling did not fire; elapsed=%v", elapsed)
	}
	if rec.Body.String() == "0123456789" {
		t.Fatalf("expected truncated body due to total ceiling")
	}
}

// A client that keeps receiving data never trips the client idle timeout, even
// when the whole transfer is slower than that timeout.
func TestClientIdleTimeoutAllowsSteadyTransfer(t *testing.T) {
	f := newFixture(t, nil)
	store := &pacingStore{ObjectStore: f.wrapped, chunk: 1, gap: 30 * time.Millisecond}
	rec := timeoutFixture(t, store, func(o *handler.Options) {
		o.UpstreamIdleTimeout = 5 * time.Second
		o.UpstreamTotalTimeout = 0
		o.ClientIdleTimeout = 100 * time.Millisecond
		o.ClientTotalTimeout = 0
	})
	if rec.Code != http.StatusOK || rec.Body.String() != "0123456789" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
}

// stallingWriter blocks on the first Write until released, then reports how many
// bytes the handler attempted to write, to prove the client idle watchdog fired.
type stallingWriter struct {
	header       http.Header
	mu           sync.Mutex
	written      int
	release      chan struct{}
	blockOnFirst bool
	blockedOnce  int32
}

func newStallingWriter() *stallingWriter {
	return &stallingWriter{header: http.Header{}, release: make(chan struct{}), blockOnFirst: true}
}

func (w *stallingWriter) Header() http.Header { return w.header }

func (w *stallingWriter) WriteHeader(int) {}

func (w *stallingWriter) Write(b []byte) (int, error) {
	if w.blockOnFirst && atomic.CompareAndSwapInt32(&w.blockedOnce, 0, 1) {
		<-w.release // simulate a stalled client socket
	}
	w.mu.Lock()
	w.written += len(b)
	w.mu.Unlock()
	return len(b), nil
}

func TestClientIdleTimeoutCancelsStalledDownload(t *testing.T) {
	f := newFixture(t, nil)
	// Upstream is slow but flows; the client stalls on first write.
	store := &pacingStore{ObjectStore: f.wrapped, chunk: 1, gap: 20 * time.Millisecond}
	opts := fixtureOptions(t)
	opts.Snapshots = f.snapshots
	opts.Stores = &fakeProvider{store: store}
	opts.Manifests = manifest.NewReader()
	opts.UpstreamIdleTimeout = 5 * time.Second
	opts.ClientIdleTimeout = 100 * time.Millisecond
	opts.ClientTotalTimeout = 0
	h := handler.New(opts)

	req := httptest.NewRequest(http.MethodGet, "http://"+testHost+"/big.txt", nil)
	w := newStallingWriter()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(w, req)
		close(done)
	}()

	select {
	case <-done:
		t.Fatalf("handler returned before the client stalled; watchdog should block it")
	case <-time.After(400 * time.Millisecond):
		// Still blocked on the first write: the client watchdog should have
		// cancelled the request by now. Release the writer to let it unwind.
	}
	w.mu.Lock()
	written := w.written
	w.mu.Unlock()
	if written != 0 {
		t.Fatalf("expected no bytes written while stalled, got %d", written)
	}
	close(w.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("handler did not unwind after releasing writer")
	}
}
