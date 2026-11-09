package objectstore

import (
	"fmt"
	"sync"
)

// Pool caches one store per alias, keyed by the alias credential version.
type Pool struct {
	mu      sync.Mutex
	entries map[string]poolEntry
}

type poolEntry struct {
	version int64
	store   ObjectStore
}

// NewPool returns an empty pool.
func NewPool() *Pool {
	return &Pool{entries: make(map[string]poolEntry)}
}

// Get returns the cached store for aliasID when it was built from
// credentialVersion, otherwise it builds and caches a new one.
func (p *Pool) Get(aliasID string, credentialVersion int64, creds Credentials) (ObjectStore, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.entries == nil {
		p.entries = make(map[string]poolEntry)
	}
	if entry, ok := p.entries[aliasID]; ok && entry.version == credentialVersion {
		return entry.store, nil
	}
	store, err := NewMinIO(creds)
	if err != nil {
		return nil, fmt.Errorf("create store for alias %q: %w", aliasID, err)
	}
	p.entries[aliasID] = poolEntry{version: credentialVersion, store: store}
	return store, nil
}

// Invalidate drops the cached store for aliasID.
func (p *Pool) Invalidate(aliasID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.entries, aliasID)
}
