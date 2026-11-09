// Package cache implements the data-plane object cache.
//
// It stores complete S3 objects on local disk and its directory layout mirrors
// the object-storage layout, so an operator can map a cached file back to its
// origin object without any index:
//
//	{cache_dir}/{alias_id}/{bucket}/{project_id}/{version_id}/{object_key}
//
// Objects are immutable inside a published version, so an entry stays valid
// until it has not been accessed for TTL or until the byte budget forces an
// LRU eviction. There is no revalidation: a cache entry is either present
// (served) or absent (fetched from the origin and filled again).
package cache

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultNegativeTTL is the lifetime of a cached "object is missing" answer.
const DefaultNegativeTTL = 30 * time.Second

// DefaultEvictRatio is the occupancy the cache is reduced to when the byte
// budget is reached. It leaves headroom so that the next fills do not have to
// evict immediately again.
const DefaultEvictRatio = 0.6

const (
	defaultSweepInterval = 5 * time.Minute
	defaultFillWait      = 2 * time.Second
	// accessTouchInterval throttles mtime updates on hits. The mtime is only
	// used to rebuild the last-access time after a restart, so a coarse value
	// avoids a write on every request.
	accessTouchInterval = 5 * time.Minute
	tempFilePrefix      = ".pf-tmp-"
)

var (
	// ErrSizeMismatch means the origin body length did not match the manifest,
	// so the object was served but not committed to the cache.
	ErrSizeMismatch = errors.New("cache: object size mismatch")
	// ErrUncacheable means the object key cannot be mapped to a safe file path.
	ErrUncacheable = errors.New("cache: object key cannot be mapped to a file path")
	errFillAborted = errors.New("cache: fill aborted")
)

// Status describes how a request was served.
type Status string

const (
	StatusHit      Status = "HIT"
	StatusMiss     Status = "MISS"
	StatusBypass   Status = "BYPASS"
	StatusNegative Status = "NEGATIVE"
)

// FetchFunc downloads one object from the origin. The caller owns the
// returned body and must close it.
type FetchFunc func(ctx context.Context) (io.ReadCloser, error)

// Options configures the disk cache.
type Options struct {
	// Dir is the cache root. It is created when missing.
	Dir string
	// MaxBytes is the byte budget. Zero or negative means "no limit".
	MaxBytes int64
	// TTL is the idle time after which an entry is removed. Zero disables
	// idle expiry; entries then only leave the cache through the byte budget.
	TTL time.Duration
	// EvictRatio is the occupancy (0-1) the cache is reduced to when the
	// budget is reached. Defaults to 0.6.
	EvictRatio float64
	// NegativeTTL is the lifetime of a missing-object answer. Defaults to 30s.
	NegativeTTL time.Duration
	// SweepInterval is the background maintenance period. Defaults to 5m.
	SweepInterval time.Duration
	// FillWait is how long a concurrent miss waits for the in-flight fill
	// before falling back to a direct origin request. Defaults to 2s.
	FillWait time.Duration
	// Now overrides the clock in tests.
	Now func() time.Time
	// Logger receives maintenance notices, including forced LRU evictions
	// when the byte budget is reached. Defaults to slog.Default.
	Logger *slog.Logger
}

// Stats is a snapshot of cache counters.
type Stats struct {
	Entries    int   `json:"entries"`
	UsedBytes  int64 `json:"used_bytes"`
	MaxBytes   int64 `json:"max_bytes"`
	Hits       int64 `json:"hits"`
	Misses     int64 `json:"misses"`
	Bypasses   int64 `json:"bypasses"`
	Fills      int64 `json:"fills"`
	FillErrors int64 `json:"fill_errors"`
	Evictions  int64 `json:"evictions"`
	Expired    int64 `json:"expired"`
}

type entry struct {
	obj          Object
	path         string
	size         int64
	lastAccess   time.Time
	mtimeTouched time.Time
	elem         *list.Element
}

type fillCall struct {
	id   string
	done chan struct{}
	err  error
}

// Cache is the local object cache. It is safe for concurrent use.
type Cache struct {
	dir           string
	maxBytes      int64
	ttl           time.Duration
	evictRatio    float64
	negativeTTL   time.Duration
	sweepInterval time.Duration
	fillWait      time.Duration
	now           func() time.Time
	logger        *slog.Logger

	mu       sync.Mutex
	entries  map[string]*entry
	order    *list.List
	used     int64
	negative map[string]time.Time
	flights  map[string]*fillCall

	evictCh   chan struct{}
	stopCh    chan struct{}
	doneCh    chan struct{}
	closeOnce sync.Once

	hits       atomic.Int64
	misses     atomic.Int64
	bypasses   atomic.Int64
	fills      atomic.Int64
	fillErrors atomic.Int64
	evictions  atomic.Int64
	expired    atomic.Int64
}

// Open opens (and scans) the cache directory and starts background
// maintenance.
func Open(opts Options) (*Cache, error) {
	if strings.TrimSpace(opts.Dir) == "" {
		return nil, errors.New("cache dir must not be empty")
	}
	if opts.EvictRatio <= 0 || opts.EvictRatio >= 1 {
		opts.EvictRatio = DefaultEvictRatio
	}
	if opts.NegativeTTL <= 0 {
		opts.NegativeTTL = DefaultNegativeTTL
	}
	if opts.SweepInterval <= 0 {
		opts.SweepInterval = defaultSweepInterval
	}
	if opts.FillWait <= 0 {
		opts.FillWait = defaultFillWait
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("create cache directory: %w", err)
	}
	c := &Cache{
		dir:           filepath.Clean(opts.Dir),
		maxBytes:      opts.MaxBytes,
		ttl:           opts.TTL,
		evictRatio:    opts.EvictRatio,
		negativeTTL:   opts.NegativeTTL,
		sweepInterval: opts.SweepInterval,
		fillWait:      opts.FillWait,
		now:           now,
		logger:        logger,
		entries:       map[string]*entry{},
		order:         list.New(),
		negative:      map[string]time.Time{},
		flights:       map[string]*fillCall{},
		evictCh:       make(chan struct{}, 1),
		stopCh:        make(chan struct{}),
		doneCh:        make(chan struct{}),
	}
	if err := c.scan(); err != nil {
		return nil, err
	}
	// The previous process may have stopped while over budget.
	c.evictOverBudget()
	go c.manage()
	return c, nil
}

// Dir returns the cache root.
func (c *Cache) Dir() string { return c.dir }

// removeFile deletes the cached file and then prunes now-empty parent
// directories up to (but not including) the cache root. This keeps the cache
// directory tidy after TTL expiry, LRU eviction and operator deletions. A
// non-empty directory (sibling files still present) is left untouched.
func (c *Cache) removeFile(path string) {
	if err := os.Remove(path); err != nil {
		return
	}
	dir := filepath.Dir(path)
	for dir != c.dir {
		if err := os.Remove(dir); err != nil {
			// Not empty (sibling files) or not removable; stop walking up.
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
}

// Close stops background maintenance. It is safe to call more than once.
func (c *Cache) Close() error {
	c.closeOnce.Do(func() {
		close(c.stopCh)
		<-c.doneCh
	})
	return nil
}

// Open returns a cached object for reading. A missing file (for example one
// removed by an operator) is treated as a miss; the caller refetches and the
// fill recreates it.
func (c *Cache) Open(obj Object) (*os.File, int64, bool) {
	if _, ok := obj.segments(); !ok {
		return nil, 0, false
	}
	now := c.now()
	id := obj.ID()

	c.mu.Lock()
	e, ok := c.entries[id]
	if !ok {
		c.mu.Unlock()
		return nil, 0, false
	}
	if c.expiredLocked(e, now) {
		c.removeLocked(e)
		c.mu.Unlock()
		c.removeFile(e.path)
		c.expired.Add(1)
		return nil, 0, false
	}
	e.lastAccess = now
	c.order.MoveToFront(e.elem)
	path, size := e.path, e.size
	touch := now.Sub(e.mtimeTouched) >= accessTouchInterval
	if touch {
		e.mtimeTouched = now
	}
	c.mu.Unlock()

	file, err := os.Open(path)
	if err != nil {
		// The file vanished or became unreadable: drop the stale index entry.
		c.mu.Lock()
		if current, ok := c.entries[id]; ok && current == e {
			c.removeLocked(current)
		}
		c.mu.Unlock()
		return nil, 0, false
	}
	if touch {
		_ = os.Chtimes(path, now, now)
	}
	c.hits.Add(1)
	return file, size, true
}

// Get serves obj from the cache or fills it from the origin.
//
// It returns a readable body, the object size reported to the client, and the
// cache status. When the status is StatusNegative the caller must answer 404
// without touching the origin. When the cache is disabled by budget the body
// is streamed from the origin and is not stored (StatusBypass).
func (c *Cache) Get(ctx context.Context, obj Object, size int64, fetch FetchFunc) (io.ReadCloser, int64, Status, error) {
	if file, n, ok := c.Open(obj); ok {
		return file, n, StatusHit, nil
	}
	if c.IsNotFound(obj) {
		return nil, 0, StatusNegative, nil
	}
	c.misses.Add(1)

	if !c.admit(size) {
		c.bypasses.Add(1)
		c.signalEvict()
		body, err := fetch(ctx)
		if err != nil {
			return nil, 0, StatusBypass, err
		}
		return body, size, StatusBypass, nil
	}

	flight, leader := c.beginFlight(obj.ID())
	if !leader {
		select {
		case <-flight.done:
			if flight.err == nil {
				if file, n, ok := c.Open(obj); ok {
					return file, n, StatusHit, nil
				}
			}
		case <-time.After(c.fillWait):
		case <-ctx.Done():
			return nil, 0, "", ctx.Err()
		}
		c.bypasses.Add(1)
		body, err := fetch(ctx)
		if err != nil {
			return nil, 0, StatusBypass, err
		}
		return body, size, StatusBypass, nil
	}

	body, err := fetch(ctx)
	if err != nil {
		c.finishFlight(flight, err)
		return nil, 0, "", err
	}
	tmp, path, err := c.createTemp(obj)
	if err != nil {
		// The object key cannot be stored on this file system; stream it.
		c.finishFlight(flight, err)
		c.bypasses.Add(1)
		return body, size, StatusBypass, nil
	}
	c.fills.Add(1)
	return &fillReader{
		cache:   c,
		src:     body,
		tmp:     tmp,
		tmpPath: tmp.Name(),
		path:    path,
		obj:     obj,
		size:    size,
		flight:  flight,
	}, size, StatusMiss, nil
}

// IsNotFound reports whether obj is inside the negative cache window.
func (c *Cache) IsNotFound(obj Object) bool {
	id := obj.ID()
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	expires, ok := c.negative[id]
	if !ok {
		return false
	}
	if now.After(expires) {
		delete(c.negative, id)
		return false
	}
	return true
}

// MarkNotFound records a missing object so repeated probes do not hit S3.
func (c *Cache) MarkNotFound(obj Object) {
	c.mu.Lock()
	c.negative[obj.ID()] = c.now().Add(c.negativeTTL)
	c.mu.Unlock()
}

// UsedBytes reports the current cache size.
func (c *Cache) UsedBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

// Stats returns a snapshot of the cache counters.
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	entries := len(c.entries)
	used := c.used
	c.mu.Unlock()
	return Stats{
		Entries:    entries,
		UsedBytes:  used,
		MaxBytes:   c.maxBytes,
		Hits:       c.hits.Load(),
		Misses:     c.misses.Load(),
		Bypasses:   c.bypasses.Load(),
		Fills:      c.fills.Load(),
		FillErrors: c.fillErrors.Load(),
		Evictions:  c.evictions.Load(),
		Expired:    c.expired.Load(),
	}
}

// Sweep removes idle entries and enforces the byte budget. It is exposed for
// tests and for operators that want a deterministic cleanup point.
func (c *Cache) Sweep() {
	now := c.now()
	var idle []*entry
	c.mu.Lock()
	for _, e := range c.entries {
		if c.expiredLocked(e, now) {
			idle = append(idle, e)
		}
	}
	for _, e := range idle {
		c.removeLocked(e)
	}
	c.mu.Unlock()
	for _, e := range idle {
		c.removeFile(e.path)
	}
	c.expired.Add(int64(len(idle)))
	c.evictOverBudget()
}

func (c *Cache) expiredLocked(e *entry, now time.Time) bool {
	return c.ttl > 0 && now.Sub(e.lastAccess) > c.ttl
}

// admit reports whether an object of size bytes fits without exceeding the
// budget. Objects that do not fit are served directly from the origin.
func (c *Cache) admit(size int64) bool {
	if size < 0 {
		return false
	}
	if c.maxBytes <= 0 {
		return true
	}
	if size > c.maxBytes {
		return false
	}
	c.mu.Lock()
	used := c.used
	c.mu.Unlock()
	return used+size <= c.maxBytes
}

func (c *Cache) beginFlight(id string) (*fillCall, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.flights[id]; ok {
		return existing, false
	}
	flight := &fillCall{id: id, done: make(chan struct{})}
	c.flights[id] = flight
	return flight, true
}

func (c *Cache) finishFlight(flight *fillCall, err error) {
	c.mu.Lock()
	if current, ok := c.flights[flight.id]; ok && current == flight {
		delete(c.flights, flight.id)
	}
	c.mu.Unlock()
	flight.err = err
	close(flight.done)
}

func (c *Cache) register(obj Object, path string, size int64) {
	now := c.now()
	c.mu.Lock()
	if old, ok := c.entries[obj.ID()]; ok {
		c.order.Remove(old.elem)
		c.used -= old.size
	}
	e := &entry{obj: obj, path: path, size: size, lastAccess: now, mtimeTouched: now}
	e.elem = c.order.PushFront(e)
	c.entries[obj.ID()] = e
	c.used += size
	over := c.maxBytes > 0 && c.used > c.maxBytes
	c.mu.Unlock()
	if over {
		c.signalEvict()
	}
}

func (c *Cache) removeLocked(e *entry) {
	c.order.Remove(e.elem)
	delete(c.entries, e.obj.ID())
	c.used -= e.size
}

func (c *Cache) createTemp(obj Object) (*os.File, string, error) {
	segments, ok := obj.segments()
	if !ok {
		return nil, "", ErrUncacheable
	}
	parts := append([]string{c.dir}, segments...)
	path := filepath.Join(parts...)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", err
	}
	tmp, err := os.CreateTemp(dir, tempFilePrefix+"*")
	if err != nil {
		return nil, "", err
	}
	return tmp, path, nil
}

func (c *Cache) manage() {
	defer close(c.doneCh)
	ticker := time.NewTicker(c.sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.Sweep()
		case <-c.evictCh:
			c.evictToLowWatermark()
		}
	}
}

func (c *Cache) signalEvict() {
	select {
	case c.evictCh <- struct{}{}:
	default:
	}
}

// evictOverBudget evicts only when the budget is actually exceeded.
func (c *Cache) evictOverBudget() {
	c.mu.Lock()
	over := c.maxBytes > 0 && c.used > c.maxBytes
	c.mu.Unlock()
	if over {
		c.evictToLowWatermark()
	}
}

// evictToLowWatermark removes least recently used entries until the cache
// occupies at most EvictRatio of the budget. TTL is ignored here.
func (c *Cache) evictToLowWatermark() {
	if c.maxBytes <= 0 {
		return
	}
	target := int64(float64(c.maxBytes) * c.evictRatio)
	var victims []*entry
	var usedBefore int64
	c.mu.Lock()
	usedBefore = c.used
	for c.used > target {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		e := oldest.Value.(*entry)
		c.removeLocked(e)
		victims = append(victims, e)
	}
	c.mu.Unlock()
	for _, e := range victims {
		c.removeFile(e.path)
	}
	if len(victims) > 0 {
		c.evictions.Add(int64(len(victims)))
		c.logger.Error("cache forced LRU eviction: byte budget reached",
			slog.Int64("used_bytes_before", usedBefore),
			slog.Int64("max_bytes", c.maxBytes),
			slog.Int64("low_watermark_bytes", target),
			slog.Int("evicted_entries", len(victims)),
		)
	}
}

func (c *Cache) scan() error {
	var found []*entry
	err := filepath.WalkDir(c.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), tempFilePrefix) {
			_ = os.Remove(path)
			return nil
		}
		rel, relErr := filepath.Rel(c.dir, path)
		if relErr != nil {
			return nil
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) < 5 {
			return nil
		}
		obj := Object{
			AliasID:   parts[0],
			Bucket:    parts[1],
			ProjectID: parts[2],
			VersionID: parts[3],
			Key:       strings.Join(parts[4:], "/"),
		}
		if _, ok := obj.segments(); !ok {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil || !info.Mode().IsRegular() {
			return nil
		}
		found = append(found, &entry{
			obj:          obj,
			path:         path,
			size:         info.Size(),
			lastAccess:   info.ModTime(),
			mtimeTouched: info.ModTime(),
		})
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(found, func(i, j int) bool {
		return found[i].lastAccess.Before(found[j].lastAccess)
	})
	c.mu.Lock()
	for _, e := range found {
		e.elem = c.order.PushFront(e)
		c.entries[e.obj.ID()] = e
		c.used += e.size
	}
	c.mu.Unlock()
	return nil
}

// fillReader streams an origin body to the client while writing a temporary
// file. The entry is committed only when the body was read to EOF and its
// length matched the manifest.
type fillReader struct {
	cache    *Cache
	src      io.ReadCloser
	tmp      *os.File
	tmpPath  string
	path     string
	obj      Object
	size     int64
	written  int64
	flight   *fillCall
	done     bool
	writeErr error
}

func (r *fillReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	if n > 0 {
		if r.tmp != nil {
			if _, writeErr := r.tmp.Write(p[:n]); writeErr != nil {
				r.writeErr = writeErr
				_ = r.tmp.Close()
				_ = os.Remove(r.tmpPath)
				r.tmp = nil
			}
		}
		r.written += int64(n)
	}
	if err == io.EOF {
		r.finish(r.writeErr)
		return n, io.EOF
	}
	if err != nil {
		r.finish(err)
	}
	return n, err
}

func (r *fillReader) Close() error {
	if !r.done {
		r.finish(errFillAborted)
	}
	return nil
}

func (r *fillReader) finish(cause error) {
	if r.done {
		return
	}
	r.done = true
	_ = r.src.Close()

	if cause != nil || r.tmp == nil {
		if r.tmp != nil {
			_ = r.tmp.Close()
			_ = os.Remove(r.tmpPath)
		}
		if cause == nil {
			cause = ErrSizeMismatch
		}
		if !errors.Is(cause, errFillAborted) {
			r.cache.fillErrors.Add(1)
		}
		r.cache.finishFlight(r.flight, cause)
		return
	}
	if r.size >= 0 && r.written != r.size {
		_ = r.tmp.Close()
		_ = os.Remove(r.tmpPath)
		r.cache.fillErrors.Add(1)
		r.cache.finishFlight(r.flight, ErrSizeMismatch)
		return
	}
	if err := r.tmp.Close(); err != nil {
		_ = os.Remove(r.tmpPath)
		r.cache.fillErrors.Add(1)
		r.cache.finishFlight(r.flight, err)
		return
	}
	if err := renameWithRetry(r.tmpPath, r.path); err != nil {
		_ = os.Remove(r.tmpPath)
		r.cache.fillErrors.Add(1)
		r.cache.finishFlight(r.flight, err)
		return
	}
	r.tmp = nil
	r.cache.register(r.obj, r.path, r.written)
	r.cache.finishFlight(r.flight, nil)
}

// renameWithRetry retries on Windows, where a reader may still hold the
// previous file open when a fill replaces it.
func renameWithRetry(from, to string) error {
	var err error
	for i := 0; i < 5; i++ {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		time.Sleep(2 * time.Millisecond)
	}
	return err
}

// Path returns the file-system path for obj. It mirrors the object-storage
// layout: alias_id/bucket/project_id/version_id/object_key.
func (c *Cache) Path(obj Object) (string, error) {
	segments, ok := obj.segments()
	if !ok {
		return "", ErrUncacheable
	}
	return filepath.Join(append([]string{c.dir}, segments...)...), nil
}
