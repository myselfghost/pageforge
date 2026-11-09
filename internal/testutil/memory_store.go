package testutil

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pageforge/pageforge/internal/objectstore"
)

// MemoryStore is an in-memory ObjectStore used by publish, manifest and GC tests.
type MemoryStore struct {
	mu      sync.RWMutex
	objects map[string]memoryObject
	order   []string
}

type memoryObject struct {
	data     []byte
	info     objectstore.ObjectInfo
	encoding string
}

var _ objectstore.ObjectStore = (*MemoryStore)(nil)

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{objects: make(map[string]memoryObject)}
}

func objectID(bucket, key string) string {
	return bucket + "/" + key
}

func (m *MemoryStore) Stat(_ context.Context, bucket, key string) (objectstore.ObjectInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	object, ok := m.objects[objectID(bucket, key)]
	if !ok {
		return objectstore.ObjectInfo{}, objectstore.ErrNotFound
	}
	return object.info, nil
}

func (m *MemoryStore) Get(_ context.Context, bucket, key string, opt objectstore.GetOptions) (io.ReadCloser, objectstore.ObjectInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	object, ok := m.objects[objectID(bucket, key)]
	if !ok {
		return nil, objectstore.ObjectInfo{}, objectstore.ErrNotFound
	}
	data := object.data
	info := object.info
	if opt.Range != "" {
		start, end, ok := parseMemoryRange(opt.Range, int64(len(data)))
		if !ok {
			return nil, info, fmt.Errorf("invalid range %q", opt.Range)
		}
		data = data[start : end+1]
		info.Size = int64(len(data))
	}
	return io.NopCloser(bytes.NewReader(data)), info, nil
}

func parseMemoryRange(header string, size int64) (int64, int64, bool) {
	spec := strings.TrimPrefix(header, "bytes=")
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end := size - 1
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start {
			return 0, 0, false
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end, true
}

func (m *MemoryStore) Put(_ context.Context, bucket, key string, r io.Reader, size int64, opt objectstore.PutOptions) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if size >= 0 && int64(len(data)) != size {
		return fmt.Errorf("put %s: wrote %d bytes, declared %d", key, len(data), size)
	}
	sum := sha256.Sum256(data)
	info := objectstore.ObjectInfo{
		Key:          key,
		Size:         int64(len(data)),
		ETag:         hex.EncodeToString(sum[:]),
		ContentType:  opt.ContentType,
		LastModified: time.Now().UTC(),
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[objectID(bucket, key)] = memoryObject{data: data, info: info, encoding: opt.Encoding}
	m.order = append(m.order, key)
	return nil
}

func (m *MemoryStore) List(_ context.Context, bucket, prefix string, fn func(objectstore.ObjectInfo) error) error {
	m.mu.RLock()
	infos := make([]objectstore.ObjectInfo, 0)
	for id, object := range m.objects {
		if len(id) > len(bucket)+1 && id[:len(bucket)+1] == bucket+"/" && hasPrefix(object.info.Key, prefix) {
			infos = append(infos, object.info)
		}
	}
	m.mu.RUnlock()

	sort.Slice(infos, func(i, j int) bool { return infos[i].Key < infos[j].Key })
	for _, info := range infos {
		if err := fn(info); err != nil {
			return err
		}
	}
	return nil
}

func (m *MemoryStore) Remove(_ context.Context, bucket, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, objectID(bucket, key))
	return nil
}

func (m *MemoryStore) PresignPut(context.Context, string, string, time.Duration) (string, error) {
	return "", fmt.Errorf("presigned uploads are not implemented")
}

func (m *MemoryStore) Copy(_ context.Context, srcBucket, srcKey, dstBucket, dstKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	object, ok := m.objects[objectID(srcBucket, srcKey)]
	if !ok {
		return objectstore.ErrNotFound
	}
	object.info.Key = dstKey
	m.objects[objectID(dstBucket, dstKey)] = object
	m.order = append(m.order, dstKey)
	return nil
}

// Read returns the stored body for a key.
func (m *MemoryStore) Read(bucket, key string) ([]byte, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	object, ok := m.objects[objectID(bucket, key)]
	return object.data, ok
}

// Encoding returns the content encoding stored for a key.
func (m *MemoryStore) Encoding(bucket, key string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.objects[objectID(bucket, key)].encoding
}

// Keys returns all keys of a bucket, sorted.
func (m *MemoryStore) Keys(bucket string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	keys := make([]string, 0, len(m.objects))
	for id, object := range m.objects {
		if len(id) > len(bucket)+1 && id[:len(bucket)+1] == bucket+"/" {
			keys = append(keys, object.info.Key)
		}
	}
	sort.Strings(keys)
	return keys
}

// PutOrder records the key of every successful Put in call order.
func (m *MemoryStore) PutOrder() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]string(nil), m.order...)
}

func hasPrefix(value, prefix string) bool {
	return len(value) >= len(prefix) && value[:len(prefix)] == prefix
}
