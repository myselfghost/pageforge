package manifest_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pageforge/pageforge/internal/manifest"
	"github.com/pageforge/pageforge/internal/objectstore"
	"github.com/pageforge/pageforge/internal/testutil"
)

const readerBucket = "bucket-a"

type countingStore struct {
	objectstore.ObjectStore
	mu       sync.Mutex
	shardGet int
	rootGet  int
}

func (c *countingStore) Get(ctx context.Context, bucket, key string, opt objectstore.GetOptions) (io.ReadCloser, objectstore.ObjectInfo, error) {
	c.mu.Lock()
	switch {
	case strings.Contains(key, "/shards/"):
		c.shardGet++
	case strings.HasSuffix(key, "/root.json"):
		c.rootGet++
	}
	c.mu.Unlock()
	return c.ObjectStore.Get(ctx, bucket, key, opt)
}

func (c *countingStore) counts() (shard, root int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.shardGet, c.rootGet
}

// gatedStore blocks root.json fetches until the gate is closed so concurrent
// callers are forced to overlap on the same in-flight load.
type gatedStore struct {
	*testutil.MemoryStore
	gate     chan struct{}
	mu       sync.Mutex
	rootGets int
}

func (g *gatedStore) Get(ctx context.Context, bucket, key string, opt objectstore.GetOptions) (io.ReadCloser, objectstore.ObjectInfo, error) {
	if strings.HasSuffix(key, "/root.json") {
		g.mu.Lock()
		g.rootGets++
		g.mu.Unlock()
		select {
		case <-g.gate:
		case <-ctx.Done():
			return nil, objectstore.ObjectInfo{}, ctx.Err()
		}
	}
	return g.MemoryStore.Get(ctx, bucket, key, opt)
}

func (g *gatedStore) rootFetchCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.rootGets
}

// gunzipStore simulates two S3 backend behaviours for gzip-compressed objects:
//   - transparent=false: return the stored bytes verbatim (raw gzip stream),
//     as gateways that ignore Content-Encoding do.
//   - transparent=true: decompress before returning, as the minio-go SDK does
//     for MinIO/AWS when Content-Encoding: gzip is present.
type gunzipStore struct {
	objectstore.ObjectStore
	transparent bool
}

func (s *gunzipStore) Get(ctx context.Context, bucket, key string, opt objectstore.GetOptions) (io.ReadCloser, objectstore.ObjectInfo, error) {
	body, info, err := s.ObjectStore.Get(ctx, bucket, key, opt)
	if err != nil || !s.transparent {
		return body, info, err
	}
	// Only object GETs (no Range) carry the gzip payload here.
	if opt.Range != "" {
		return body, info, err
	}
	raw, err := io.ReadAll(body)
	closeErr := body.Close()
	if err != nil {
		return nil, objectstore.ObjectInfo{}, err
	}
	if closeErr != nil {
		return nil, objectstore.ObjectInfo{}, closeErr
	}
	if magic, err := gzip.NewReader(bytes.NewReader(raw)); err == nil {
		plain, err := io.ReadAll(magic)
		_ = magic.Close()
		if err != nil {
			return nil, objectstore.ObjectInfo{}, err
		}
		return io.NopCloser(bytes.NewReader(plain)), info, nil
	}
	// Not gzip: return as-is.
	return io.NopCloser(bytes.NewReader(raw)), info, nil
}

// TestReaderRootAcrossGzipBackends proves the manifest reader tolerates both a
// backend that transparently decompresses gzip objects and one that returns the
// raw gzip bytes.
func TestReaderRootAcrossGzipBackends(t *testing.T) {
	ctx := context.Background()
	for _, transparent := range []bool{false, true} {
		name := "raw-gzip"
		if transparent {
			name = "transparent-decompress"
		}
		t.Run(name, func(t *testing.T) {
			_, store := buildSite(t)
			wrapped := &gunzipStore{ObjectStore: store, transparent: transparent}
			reader := manifest.NewReader()
			root, err := reader.Root(ctx, wrapped, readerBucket, "prj_1", "ver_1")
			if err != nil {
				t.Fatalf("load root: %v", err)
			}
			if root.Schema != manifest.SchemaRoot || root.IndexDoc != "index.html" {
				t.Fatalf("unexpected root: %+v", root)
			}
			entry, ok, err := reader.Lookup(ctx, wrapped, readerBucket, root, "assets/a.js")
			if err != nil || !ok || entry.Size != 20 {
				t.Fatalf("lookup assets/a.js = %+v ok=%v err=%v", entry, ok, err)
			}
		})
	}
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func buildSite(t *testing.T) (*testutil.MemoryStore, *countingStore) {
	t.Helper()
	store := testutil.NewMemoryStore()
	builder, err := manifest.NewBuilder("_system/manifests/prj_1/ver_1", manifest.Limits{ShardFiles: 2}, t.TempDir())
	if err != nil {
		t.Fatalf("new builder: %v", err)
	}
	defer func() { _ = builder.Close() }()
	entries := []manifest.Entry{
		{Key: "index.html", Size: 10, ContentType: "text/html"},
		{Key: "assets/a.js", Size: 20, ContentType: "text/javascript"},
		{Key: "assets/b.css", Size: 30, ContentType: "text/css"},
		{Key: "docs/x/y.html", Size: 40, ContentType: "text/html"},
		{Key: "docs/z.html", Size: 50, ContentType: "text/html"},
		{Key: "media/v.mp4", Size: 60, ContentType: "video/mp4"},
	}
	for _, entry := range entries {
		if err := builder.Add(entry); err != nil {
			t.Fatalf("add entry: %v", err)
		}
	}
	if _, err := builder.Build(context.Background(), store, readerBucket, manifest.RootOptions{
		ProjectID: "prj_1",
		VersionID: "ver_1",
		IndexDoc:  "index.html",
	}); err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	return store, &countingStore{ObjectStore: store}
}

func TestReaderLoadsShardsLazily(t *testing.T) {
	_, store := buildSite(t)
	reader := manifest.NewReader()
	ctx := context.Background()

	root, err := reader.Root(ctx, store, readerBucket, "prj_1", "ver_1")
	if err != nil {
		t.Fatalf("load root: %v", err)
	}
	if root.Schema != manifest.SchemaRoot || root.IndexDoc != "index.html" || len(root.Shards) == 0 {
		t.Fatalf("unexpected root: %+v", root)
	}
	if _, rootGet := store.counts(); rootGet != 1 {
		t.Fatalf("root fetches = %d, want 1", rootGet)
	}

	if _, err := reader.Root(ctx, store, readerBucket, "prj_1", "ver_1"); err != nil {
		t.Fatalf("reload root: %v", err)
	}
	if _, rootGet := store.counts(); rootGet != 1 {
		t.Fatalf("root should be resident, got %d fetches", rootGet)
	}

	entry, ok, err := reader.Lookup(ctx, store, readerBucket, root, "assets/a.js")
	if err != nil || !ok || entry.Size != 20 {
		t.Fatalf("lookup assets/a.js = %+v ok=%v err=%v", entry, ok, err)
	}
	if shardGet, _ := store.counts(); shardGet != 1 {
		t.Fatalf("shard fetches = %d, want 1", shardGet)
	}
	if _, ok, err := reader.Lookup(ctx, store, readerBucket, root, "assets/b.css"); err != nil || !ok {
		t.Fatalf("lookup assets/b.css ok=%v err=%v", ok, err)
	}
	if shardGet, _ := store.counts(); shardGet != 1 {
		t.Fatalf("cached shard reloaded, fetches = %d", shardGet)
	}
	if _, ok, err := reader.Lookup(ctx, store, readerBucket, root, "docs/x/y.html"); err != nil || !ok {
		t.Fatalf("lookup docs ok=%v err=%v", ok, err)
	}
	if shardGet, _ := store.counts(); shardGet != 2 {
		t.Fatalf("shard fetches = %d, want 2", shardGet)
	}
	if _, ok, err := reader.Lookup(ctx, store, readerBucket, root, "missing.txt"); err != nil || ok {
		t.Fatalf("missing lookup ok=%v err=%v", ok, err)
	}

	has, err := reader.HasDir(ctx, store, readerBucket, root, "assets")
	if err != nil || !has {
		t.Fatalf("HasDir(assets) = %v err=%v", has, err)
	}
	has, err = reader.HasDir(ctx, store, readerBucket, root, "missing")
	if err != nil || has {
		t.Fatalf("HasDir(missing) = %v err=%v", has, err)
	}
}

func TestReaderEvictsByByteBudgetKeepingRoot(t *testing.T) {
	_, store := buildSite(t)
	ctx := context.Background()

	// Measure the decompressed weight of every cached object with a throwaway
	// reader, then use that to force exactly one eviction.
	measure := manifest.NewReader()
	root, err := measure.Root(ctx, store, readerBucket, "prj_1", "ver_1")
	if err != nil {
		t.Fatalf("measure root: %v", err)
	}
	rootWeight := measure.Stats().UsedBytes
	for _, key := range []string{"assets/a.js", "docs/z.html", "media/v.mp4"} {
		if _, _, err := measure.Lookup(ctx, store, readerBucket, root, key); err != nil {
			t.Fatalf("measure %s: %v", key, err)
		}
	}
	total := measure.Stats().UsedBytes
	if rootWeight <= 0 || total <= rootWeight {
		t.Fatalf("unexpected weights: root=%d total=%d", rootWeight, total)
	}

	reader := manifest.NewReader()
	reader.MaxBytes = total - 1
	if _, err := reader.Root(ctx, store, readerBucket, "prj_1", "ver_1"); err != nil {
		t.Fatalf("load root: %v", err)
	}
	for _, key := range []string{"assets/a.js", "docs/z.html"} {
		if _, _, err := reader.Lookup(ctx, store, readerBucket, root, key); err != nil {
			t.Fatalf("lookup %s: %v", key, err)
		}
	}
	before, _ := store.counts()

	// Inserting the media shard pushes the reader one byte over budget, so the
	// least recently used shard (assets) must be evicted.
	if _, _, err := reader.Lookup(ctx, store, readerBucket, root, "media/v.mp4"); err != nil {
		t.Fatalf("lookup media: %v", err)
	}

	stats := reader.Stats()
	if stats.RootEntries != 1 {
		t.Fatalf("root entries = %d, want 1 (root is kept)", stats.RootEntries)
	}
	if stats.ShardEntries != 2 {
		t.Fatalf("shard entries = %d, want 2", stats.ShardEntries)
	}
	if stats.Evictions == 0 {
		t.Fatal("expected at least one eviction")
	}
	if stats.UsedBytes > stats.MaxBytes {
		t.Fatalf("used bytes = %d exceeds budget %d", stats.UsedBytes, stats.MaxBytes)
	}

	// assets was the least recently used shard, so it must be fetched again.
	if _, _, err := reader.Lookup(ctx, store, readerBucket, root, "assets/a.js"); err != nil {
		t.Fatalf("lookup assets again: %v", err)
	}
	after, _ := store.counts()
	if after != before+2 {
		t.Fatalf("shard fetches = %d, want %d (one load + one refetch)", after, before+2)
	}
}

func TestReaderLoadsConcurrentlyWithSingleflight(t *testing.T) {
	store, _ := buildSite(t)
	reader := manifest.NewReader()
	gated := &gatedStore{MemoryStore: store, gate: make(chan struct{})}
	ctx := context.Background()

	const callers = 8
	var wg sync.WaitGroup
	started := make(chan struct{}, callers)
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started <- struct{}{}
			_, err := reader.Root(ctx, gated, readerBucket, "prj_1", "ver_1")
			errs <- err
		}()
	}
	for i := 0; i < callers; i++ {
		<-started
	}
	time.Sleep(50 * time.Millisecond)
	close(gated.gate)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent root load: %v", err)
		}
	}
	if got := gated.rootFetchCount(); got != 1 {
		t.Fatalf("root fetches = %d, want 1 (singleflight)", got)
	}
	stats := reader.Stats()
	if stats.Loads != 1 {
		t.Fatalf("loads = %d, want 1", stats.Loads)
	}
	if stats.Misses+stats.Hits != callers {
		t.Fatalf("hits+misses = %d, want %d", stats.Misses+stats.Hits, callers)
	}
}

func TestReaderNegativeCache(t *testing.T) {
	_, store := buildSite(t)
	reader := manifest.NewReader()
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := reader.Root(ctx, store, readerBucket, "prj_missing", "ver_missing"); err == nil {
			t.Fatal("expected missing root error")
		}
	}
	if _, rootGet := store.counts(); rootGet != 1 {
		t.Fatalf("root fetches = %d, want 1 (negative cache)", rootGet)
	}
	stats := reader.Stats()
	if stats.NegativeHits != 2 {
		t.Fatalf("negative hits = %d, want 2", stats.NegativeHits)
	}
	if stats.LoadErrors != 1 {
		t.Fatalf("load errors = %d, want 1", stats.LoadErrors)
	}
}

func TestReaderExpiresEntriesAfterTTL(t *testing.T) {
	_, store := buildSite(t)
	reader := manifest.NewReader()
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	reader.Now = clock.Now
	reader.TTL = time.Minute
	ctx := context.Background()

	if _, err := reader.Root(ctx, store, readerBucket, "prj_1", "ver_1"); err != nil {
		t.Fatalf("load root: %v", err)
	}
	if _, rootGet := store.counts(); rootGet != 1 {
		t.Fatalf("root fetches = %d, want 1", rootGet)
	}

	clock.Advance(30 * time.Second)
	if _, err := reader.Root(ctx, store, readerBucket, "prj_1", "ver_1"); err != nil {
		t.Fatalf("reload root before expiry: %v", err)
	}
	if _, rootGet := store.counts(); rootGet != 1 {
		t.Fatalf("root refetched before TTL, fetches = %d", rootGet)
	}

	clock.Advance(2 * time.Minute)
	if _, err := reader.Root(ctx, store, readerBucket, "prj_1", "ver_1"); err != nil {
		t.Fatalf("reload root after expiry: %v", err)
	}
	if _, rootGet := store.counts(); rootGet != 2 {
		t.Fatalf("root fetches = %d, want 2 after TTL expiry", rootGet)
	}
}
