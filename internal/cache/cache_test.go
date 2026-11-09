package cache_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pageforge/pageforge/internal/cache"
)

func newCache(t *testing.T, opts cache.Options) *cache.Cache {
	t.Helper()
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	c, err := cache.Open(opts)
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func testObject(key string) cache.Object {
	return cache.Object{
		AliasID:   "ali_1",
		Bucket:    "assets",
		ProjectID: "prj_1",
		VersionID: "ver_1",
		Key:       key,
	}
}

func fetchBody(body string, counter *int32) cache.FetchFunc {
	return func(context.Context) (io.ReadCloser, error) {
		if counter != nil {
			atomic.AddInt32(counter, 1)
		}
		return io.NopCloser(strings.NewReader(body)), nil
	}
}

func readGet(t *testing.T, c *cache.Cache, obj cache.Object, size int64, fetch cache.FetchFunc) (string, cache.Status) {
	t.Helper()
	reader, _, status, err := c.Get(context.Background(), obj, size, fetch)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if status == cache.StatusNegative {
		return "", status
	}
	defer func() { _ = reader.Close() }()
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(body), status
}

func TestGetFillsThenServesFromDisk(t *testing.T) {
	c := newCache(t, cache.Options{MaxBytes: 1024})
	counter := int32(0)
	obj := testObject("assets/app.js")

	body, status := readGet(t, c, obj, 5, fetchBody("hello", &counter))
	if body != "hello" || status != cache.StatusMiss {
		t.Fatalf("first body=%q status=%s", body, status)
	}
	body, status = readGet(t, c, obj, 5, fetchBody("hello", &counter))
	if body != "hello" || status != cache.StatusHit {
		t.Fatalf("second body=%q status=%s", body, status)
	}
	if counter != 1 {
		t.Fatalf("origin fetches = %d, want 1", counter)
	}
}

func TestCacheDirectoryMirrorsObjectStorage(t *testing.T) {
	dir := t.TempDir()
	c := newCache(t, cache.Options{Dir: dir, MaxBytes: 1024})
	obj := testObject("assets/app.js")
	if _, status := readGet(t, c, obj, 2, fetchBody("ok", nil)); status != cache.StatusMiss {
		t.Fatalf("status = %s", status)
	}

	want := filepath.Join(dir, "ali_1", "assets", "prj_1", "ver_1", "assets", "app.js")
	path, err := c.Path(obj)
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("cached file missing: %v", err)
	}
}

func TestMissingCachedFileIsRefetched(t *testing.T) {
	c := newCache(t, cache.Options{MaxBytes: 1024})
	counter := int32(0)
	obj := testObject("assets/app.js")
	readGet(t, c, obj, 5, fetchBody("hello", &counter))

	path, err := c.Path(obj)
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove cached file: %v", err)
	}

	body, status := readGet(t, c, obj, 5, fetchBody("hello", &counter))
	if body != "hello" || status != cache.StatusMiss {
		t.Fatalf("after delete body=%q status=%s", body, status)
	}
	if counter != 2 {
		t.Fatalf("origin fetches = %d, want 2", counter)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cached file was not rebuilt: %v", err)
	}
}

func TestIdleTTLExpiresEntries(t *testing.T) {
	c := newCache(t, cache.Options{MaxBytes: 1024, TTL: 30 * time.Millisecond})
	obj := testObject("page.html")
	readGet(t, c, obj, 4, fetchBody("page", nil))

	time.Sleep(60 * time.Millisecond)
	if _, _, ok := c.Open(obj); ok {
		t.Fatal("idle entry should have expired")
	}
	path, _ := c.Path(obj)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expired file still exists (err=%v)", err)
	}
	if got := c.Stats().Expired; got != 1 {
		t.Fatalf("expired = %d, want 1", got)
	}
}

func TestTTLUsesLastAccessNotFillTime(t *testing.T) {
	c := newCache(t, cache.Options{MaxBytes: 1024, TTL: 120 * time.Millisecond})
	obj := testObject("page.html")
	readGet(t, c, obj, 4, fetchBody("page", nil))

	time.Sleep(80 * time.Millisecond)
	if file, _, ok := c.Open(obj); ok {
		_ = file.Close()
	} else {
		t.Fatal("entry should still be alive before the idle window")
	}
	time.Sleep(80 * time.Millisecond)
	// 160ms after the fill, but only 80ms after the last access.
	if file, _, ok := c.Open(obj); ok {
		_ = file.Close()
	} else {
		t.Fatal("entry must expire based on the last access, not the fill time")
	}
}

func TestBypassWhenBudgetIsFull(t *testing.T) {
	c := newCache(t, cache.Options{MaxBytes: 10})
	counter := int32(0)
	if _, status := readGet(t, c, testObject("a.bin"), 6, fetchBody("123456", &counter)); status != cache.StatusMiss {
		t.Fatalf("first status = %s", status)
	}
	body, status := readGet(t, c, testObject("b.bin"), 5, fetchBody("12345", &counter))
	if body != "12345" || status != cache.StatusBypass {
		t.Fatalf("second body=%q status=%s", body, status)
	}
	if counter != 2 {
		t.Fatalf("origin fetches = %d, want 2", counter)
	}
	if got := c.UsedBytes(); got != 6 {
		t.Fatalf("used = %d, want 6", got)
	}
	path, _ := c.Path(testObject("b.bin"))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("bypassed object must not be written (err=%v)", err)
	}
	if got := c.Stats().Bypasses; got != 1 {
		t.Fatalf("bypasses = %d, want 1", got)
	}
}

func TestBudgetPressureEvictsToLowWatermark(t *testing.T) {
	c := newCache(t, cache.Options{MaxBytes: 100, EvictRatio: 0.6})
	keys := []string{"a.bin", "b.bin", "c.bin"}
	for _, key := range keys {
		if _, status := readGet(t, c, testObject(key), 30, fetchBody(strings.Repeat("x", 30), nil)); status != cache.StatusMiss {
			t.Fatalf("%s status = %s", key, status)
		}
	}
	// Touching c keeps it as the most recently used entry.
	if file, _, ok := c.Open(testObject("c.bin")); ok {
		_ = file.Close()
	} else {
		t.Fatal("c.bin should be cached")
	}

	// The next object does not fit and must be served directly.
	if _, status := readGet(t, c, testObject("d.bin"), 30, fetchBody(strings.Repeat("y", 30), nil)); status != cache.StatusBypass {
		t.Fatalf("status = %s, want BYPASS", status)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c.UsedBytes() <= 60 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if used := c.UsedBytes(); used > 60 {
		t.Fatalf("used = %d after eviction, want <= 60", used)
	}
	if file, _, ok := c.Open(testObject("c.bin")); ok {
		_ = file.Close()
	} else {
		t.Fatal("most recently used entry must survive eviction")
	}
	path, _ := c.Path(testObject("a.bin"))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("least recently used entry must be evicted (err=%v)", err)
	}
}

func TestConcurrentMissesFetchOnce(t *testing.T) {
	c := newCache(t, cache.Options{MaxBytes: 1024, FillWait: 2 * time.Second})
	counter := int32(0)
	fetch := func(context.Context) (io.ReadCloser, error) {
		atomic.AddInt32(&counter, 1)
		time.Sleep(20 * time.Millisecond)
		return io.NopCloser(strings.NewReader("shared")), nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reader, _, status, err := c.Get(context.Background(), testObject("app.js"), 6, fetch)
			if err != nil {
				t.Errorf("get: %v", err)
				return
			}
			if status == cache.StatusNegative {
				t.Error("unexpected negative status")
				return
			}
			_, _ = io.Copy(io.Discard, reader)
			_ = reader.Close()
		}()
	}
	wg.Wait()
	if counter != 1 {
		t.Fatalf("origin fetches = %d, want 1", counter)
	}
}

func TestReopenScansCachedFiles(t *testing.T) {
	dir := t.TempDir()
	c := newCache(t, cache.Options{Dir: dir, MaxBytes: 1024})
	if _, status := readGet(t, c, testObject("index.html"), 4, fetchBody("home", nil)); status != cache.StatusMiss {
		t.Fatalf("status = %s", status)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened := newCache(t, cache.Options{Dir: dir, MaxBytes: 1024})
	file, size, ok := reopened.Open(testObject("index.html"))
	if !ok || size != 4 {
		t.Fatalf("persisted entry missing (ok=%v size=%d)", ok, size)
	}
	_ = file.Close()
}

func TestNegativeCacheExpires(t *testing.T) {
	c := newCache(t, cache.Options{MaxBytes: 1024, NegativeTTL: 20 * time.Millisecond})
	obj := testObject("missing.js")
	c.MarkNotFound(obj)
	if !c.IsNotFound(obj) {
		t.Fatal("negative entry should be present")
	}
	if _, status := readGet(t, c, obj, 0, fetchBody("", nil)); status != cache.StatusNegative {
		t.Fatalf("status = %s, want NEGATIVE", status)
	}
	time.Sleep(40 * time.Millisecond)
	if c.IsNotFound(obj) {
		t.Fatal("negative entry should have expired")
	}
}

func TestUnsafeKeyIsServedButNotCached(t *testing.T) {
	c := newCache(t, cache.Options{MaxBytes: 1024})
	obj := testObject("../escape.js")
	body, status := readGet(t, c, obj, 4, fetchBody("evil", nil))
	if body != "evil" || status != cache.StatusBypass {
		t.Fatalf("body=%q status=%s", body, status)
	}
	if _, err := c.Path(obj); err == nil {
		t.Fatal("unsafe key must not map to a path")
	}
	if got := c.UsedBytes(); got != 0 {
		t.Fatalf("used = %d, want 0", got)
	}
}

func TestSizeMismatchIsNotCommitted(t *testing.T) {
	c := newCache(t, cache.Options{MaxBytes: 1024})
	obj := testObject("short.txt")
	body, status := readGet(t, c, obj, 10, fetchBody("abc", nil))
	if body != "abc" || status != cache.StatusMiss {
		t.Fatalf("body=%q status=%s", body, status)
	}
	if _, _, ok := c.Open(obj); ok {
		t.Fatal("mismatched object must not be cached")
	}
	if got := c.Stats().FillErrors; got != 1 {
		t.Fatalf("fill errors = %d, want 1", got)
	}
}

func TestEmptyObjectIsCached(t *testing.T) {
	c := newCache(t, cache.Options{MaxBytes: 1024})
	obj := testObject("empty.txt")
	if _, status := readGet(t, c, obj, 0, fetchBody("", nil)); status != cache.StatusMiss {
		t.Fatalf("status = %s", status)
	}
	file, size, ok := c.Open(obj)
	if !ok || size != 0 {
		t.Fatalf("empty object not cached (ok=%v size=%d)", ok, size)
	}
	_ = file.Close()
}
