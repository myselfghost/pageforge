package manifest

import (
	"bufio"
	"compress/gzip"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pageforge/pageforge/internal/objectstore"
)

// DefaultRootPrefix is the documented manifest object prefix.
const DefaultRootPrefix = "_system/manifests"

const (
	// DefaultMaxBytes is the default memory budget for cached manifests.
	DefaultMaxBytes int64 = 256 << 20
	// DefaultNegativeTTL is how long a missing root/shard is remembered.
	DefaultNegativeTTL = 30 * time.Second
	// maxObjectBytes caps the decompressed size of one manifest object.
	maxObjectBytes = 32 << 20
)

const (
	kindRoot uint8 = iota + 1
	kindShard
)

// Stats is a snapshot of the manifest cache counters.
type Stats struct {
	Entries      int   `json:"entries"`
	RootEntries  int   `json:"root_entries"`
	ShardEntries int   `json:"shard_entries"`
	UsedBytes    int64 `json:"used_bytes"`
	MaxBytes     int64 `json:"max_bytes"`
	Hits         int64 `json:"hits"`
	Misses       int64 `json:"misses"`
	Loads        int64 `json:"loads"`
	LoadErrors   int64 `json:"load_errors"`
	Evictions    int64 `json:"evictions"`
	NegativeHits int64 `json:"negative_hits"`
}

type entry struct {
	key     string
	value   any // *Root or *Shard
	kind    uint8
	weight  int64
	expires time.Time
}

type flight struct {
	key   string
	done  chan struct{}
	value any
	err   error
}

// Reader loads manifest roots and lazily materializes shards. Roots and shards
// share one weighted LRU so memory stays under MaxBytes; entries are immutable
// once loaded, so eviction never affects requests that already hold a pointer.
type Reader struct {
	Prefix      string
	MaxBytes    int64
	TTL         time.Duration
	NegativeTTL time.Duration
	Now         func() time.Time

	mu       sync.Mutex
	entries  map[string]*list.Element
	order    *list.List
	used     int64
	flights  map[string]*flight
	negative map[string]time.Time

	hits         atomic.Int64
	misses       atomic.Int64
	loads        atomic.Int64
	loadErrors   atomic.Int64
	evictions    atomic.Int64
	negativeHits atomic.Int64
}

// NewReader builds a reader with the default budget and no TTL.
func NewReader() *Reader {
	return &Reader{
		Prefix:      DefaultRootPrefix,
		MaxBytes:    DefaultMaxBytes,
		NegativeTTL: DefaultNegativeTTL,
		Now:         time.Now,
		entries:     map[string]*list.Element{},
		order:       list.New(),
		flights:     map[string]*flight{},
		negative:    map[string]time.Time{},
	}
}

// Root returns the manifest root for a project version.
func (r *Reader) Root(ctx context.Context, store objectstore.ObjectStore, bucket, projectID, versionID string) (*Root, error) {
	key := r.rootPrefix(projectID, versionID)
	cacheKey := bucket + "\x00" + key
	value, err := r.load(ctx, cacheKey, func(ctx context.Context) (any, int64, error) {
		var root Root
		size, err := fetchJSON(ctx, store, bucket, key+"/root.json", &root)
		if err != nil {
			return nil, 0, err
		}
		if root.Schema != SchemaRoot {
			return nil, 0, fmt.Errorf("unexpected manifest schema %q", root.Schema)
		}
		if root.DefaultShard == "" {
			root.DefaultShard = root.Shards[rootShardPrefix]
		}
		return &root, size, nil
	})
	if err != nil {
		return nil, err
	}
	return value.(*Root), nil
}

// Lookup resolves a project-relative key to a manifest entry.
func (r *Reader) Lookup(ctx context.Context, store objectstore.ObjectStore, bucket string, root *Root, key string) (Entry, bool, error) {
	shard, err := r.shardFor(ctx, store, bucket, root, key)
	if err != nil || shard == nil {
		return Entry{}, false, err
	}
	entry, found := shard.Files[key]
	return entry, found, nil
}

// HasDir reports whether a directory exists in the manifest.
func (r *Reader) HasDir(ctx context.Context, store objectstore.ObjectStore, bucket string, root *Root, dir string) (bool, error) {
	shard, err := r.shardFor(ctx, store, bucket, root, dir)
	if err != nil || shard == nil {
		return false, err
	}
	for _, candidate := range shard.Dirs {
		if candidate == dir {
			return true, nil
		}
	}
	return false, nil
}

// Stats returns a snapshot of the cache counters.
func (r *Reader) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	stats := Stats{
		Entries:   len(r.entries),
		UsedBytes: r.used,
		MaxBytes:  r.budget(),
	}
	for _, element := range r.entries {
		switch element.Value.(*entry).kind {
		case kindRoot:
			stats.RootEntries++
		case kindShard:
			stats.ShardEntries++
		}
	}
	stats.Hits = r.hits.Load()
	stats.Misses = r.misses.Load()
	stats.Loads = r.loads.Load()
	stats.LoadErrors = r.loadErrors.Load()
	stats.Evictions = r.evictions.Load()
	stats.NegativeHits = r.negativeHits.Load()
	return stats
}

func (r *Reader) shardFor(ctx context.Context, store objectstore.ObjectStore, bucket string, root *Root, key string) (*Shard, error) {
	prefix, ok := lookupPrefix(root.Shards, key)
	if !ok {
		return nil, nil
	}
	return r.shard(ctx, store, bucket, root, prefix)
}

func (r *Reader) shard(ctx context.Context, store objectstore.ObjectStore, bucket string, root *Root, prefix string) (*Shard, error) {
	rootKey := r.rootPrefix(root.ProjectID, root.VersionID)
	relative, ok := root.Shards[prefix]
	if !ok {
		return nil, fmt.Errorf("manifest %s has no shard for prefix %q", rootKey, prefix)
	}
	cacheKey := bucket + "\x00" + rootKey + "\x00" + prefix
	value, err := r.load(ctx, cacheKey, func(ctx context.Context) (any, int64, error) {
		var shard Shard
		size, err := fetchJSON(ctx, store, bucket, rootKey+"/"+relative, &shard)
		if err != nil {
			return nil, 0, err
		}
		if shard.Schema != SchemaShard {
			return nil, 0, fmt.Errorf("unexpected shard schema %q", shard.Schema)
		}
		return &shard, size, nil
	})
	if err != nil {
		return nil, err
	}
	return value.(*Shard), nil
}

func (r *Reader) load(ctx context.Context, key string, fetch func(context.Context) (any, int64, error)) (any, error) {
	if value, ok := r.get(key); ok {
		return value, nil
	}
	if r.negativeHit(key) {
		return nil, objectstore.ErrNotFound
	}
	r.misses.Add(1)

	current, leader := r.beginFlight(key)
	if !leader {
		select {
		case <-current.done:
			if current.err != nil {
				return nil, current.err
			}
			return current.value, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	r.loads.Add(1)
	value, weight, err := fetch(ctx)
	if err != nil {
		if errors.Is(err, objectstore.ErrNotFound) {
			r.markNegative(key)
		}
		r.loadErrors.Add(1)
		r.finishFlight(current, nil, err)
		return nil, err
	}
	r.put(key, value, weight)
	r.finishFlight(current, value, nil)
	return value, nil
}

func (r *Reader) get(key string) (any, bool) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	element, ok := r.entries[key]
	if !ok {
		return nil, false
	}
	e := element.Value.(*entry)
	if !e.expires.IsZero() && now.After(e.expires) {
		r.removeLocked(e)
		return nil, false
	}
	r.order.MoveToFront(element)
	r.hits.Add(1)
	return e.value, true
}

func (r *Reader) put(key string, value any, weight int64) {
	if weight <= 0 {
		weight = 1
	}
	budget := r.budget()
	if weight > budget {
		return
	}
	kind := kindShard
	if _, ok := value.(*Root); ok {
		kind = kindRoot
	}
	now := r.now()

	r.mu.Lock()
	defer r.mu.Unlock()
	if element, ok := r.entries[key]; ok {
		r.removeLocked(element.Value.(*entry))
	}
	e := &entry{key: key, value: value, kind: kind, weight: weight}
	if r.TTL > 0 {
		e.expires = now.Add(r.TTL)
	}
	r.entries[key] = r.order.PushFront(e)
	r.used += weight
	for r.used > budget {
		victim := r.victimLocked()
		if victim == nil {
			break
		}
		r.removeLocked(victim)
		r.evictions.Add(1)
	}
}

// victimLocked picks a shard first and keeps roots resident whenever possible.
func (r *Reader) victimLocked() *entry {
	var rootVictim *entry
	for element := r.order.Back(); element != nil; element = element.Prev() {
		e := element.Value.(*entry)
		if e.kind == kindShard {
			return e
		}
		if rootVictim == nil {
			rootVictim = e
		}
	}
	return rootVictim
}

func (r *Reader) removeLocked(e *entry) {
	if element, ok := r.entries[e.key]; ok {
		r.order.Remove(element)
		delete(r.entries, e.key)
		r.used -= e.weight
	}
}

func (r *Reader) beginFlight(key string) (*flight, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.flights[key]; ok {
		return existing, false
	}
	f := &flight{key: key, done: make(chan struct{})}
	r.flights[key] = f
	return f, true
}

func (r *Reader) finishFlight(f *flight, value any, err error) {
	r.mu.Lock()
	if current, ok := r.flights[f.key]; ok && current == f {
		delete(r.flights, f.key)
	}
	r.mu.Unlock()
	f.value, f.err = value, err
	close(f.done)
}

func (r *Reader) negativeHit(key string) bool {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	expires, ok := r.negative[key]
	if !ok {
		return false
	}
	if now.After(expires) {
		delete(r.negative, key)
		return false
	}
	r.negativeHits.Add(1)
	return true
}

func (r *Reader) markNegative(key string) {
	ttl := r.NegativeTTL
	if ttl <= 0 {
		ttl = DefaultNegativeTTL
	}
	r.mu.Lock()
	r.negative[key] = r.now().Add(ttl)
	r.mu.Unlock()
}

func (r *Reader) budget() int64 {
	if r.MaxBytes <= 0 {
		return DefaultMaxBytes
	}
	return r.MaxBytes
}

func (r *Reader) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

func (r *Reader) rootPrefix(projectID, versionID string) string {
	prefix := strings.Trim(r.Prefix, "/")
	if prefix == "" {
		prefix = DefaultRootPrefix
	}
	return prefix + "/" + projectID + "/" + versionID
}

// lookupPrefix finds the shard prefix for key by walking its ancestors, which
// is O(path depth) instead of scanning every shard prefix on each request.
func lookupPrefix(shards map[string]string, key string) (string, bool) {
	key = strings.TrimPrefix(key, "/")
	for key != "" {
		if _, ok := shards[key]; ok {
			return key, true
		}
		slash := strings.LastIndexByte(key, '/')
		if slash < 0 {
			break
		}
		key = key[:slash]
	}
	if _, ok := shards[rootShardPrefix]; ok {
		return rootShardPrefix, true
	}
	return "", false
}

// fetchJSON reads one manifest object and returns its decompressed size for
// memory accounting.
//
// Manifest objects are stored as raw gzip bytes with the encoding recorded in
// user metadata (objectstore.MetadataEncoding), never the S3 Content-Encoding
// attribute, so no backend transparently rewrites them. Objects written by older
// builds may still carry Content-Encoding: gzip, in which case a SDK such as
// minio-go returns the body already decompressed. Detecting the gzip magic bytes
// keeps both shapes decodable across providers.
func fetchJSON(ctx context.Context, store objectstore.ObjectStore, bucket, key string, v any) (int64, error) {
	body, _, err := store.Get(ctx, bucket, key, objectstore.GetOptions{})
	if err != nil {
		return 0, err
	}
	defer func() { _ = body.Close() }()

	reader, closeReader, err := maybeGunzip(body, key)
	if err != nil {
		return 0, err
	}
	defer closeReader()

	data, err := io.ReadAll(io.LimitReader(reader, maxObjectBytes+1))
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", key, err)
	}
	if int64(len(data)) > maxObjectBytes {
		return 0, fmt.Errorf("manifest %s exceeds %d bytes", key, maxObjectBytes)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return 0, fmt.Errorf("decode %s: %w", key, err)
	}
	return int64(len(data)), nil
}

// maybeGunzip returns a reader over the decompressed payload. When the body
// already starts with the gzip magic bytes (0x1f 0x8b) it wraps a gzip reader,
// otherwise it forwards the bytes unchanged (backend already decompressed).
func maybeGunzip(body io.Reader, key string) (io.Reader, func(), error) {
	br := bufio.NewReader(body)
	magic, err := br.Peek(2)
	if err != nil {
		// Fewer than 2 bytes: not gzip; treat as-is (JSON decode will judge).
		if errors.Is(err, io.EOF) {
			return br, func() {}, nil
		}
		return nil, func() {}, fmt.Errorf("read %s: %w", key, err)
	}
	if magic[0] != 0x1f || magic[1] != 0x8b {
		return br, func() {}, nil
	}
	gz, err := gzip.NewReader(br)
	if err != nil {
		return nil, func() {}, fmt.Errorf("decompress %s: %w", key, err)
	}
	return gz, func() { _ = gz.Close() }, nil
}
