package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultPollInterval matches the documented two-second refresh cadence.
const DefaultPollInterval = 2 * time.Second

// Store holds the active snapshot. Readers never block on the network; the
// poller swaps the pointer atomically after validation.
type Store struct {
	mu   sync.Mutex
	ptr  atomic.Pointer[Snapshot]
	auth atomic.Pointer[AuthDirectory]
}

// NewStore returns an empty snapshot store.
func NewStore() *Store {
	return &Store{}
}

// Load returns the active snapshot or nil before the first successful poll.
func (s *Store) Load() *Snapshot {
	return s.ptr.Load()
}

// Auth returns the password directory fetched separately from the snapshot.
func (s *Store) Auth() *AuthDirectory {
	return s.auth.Load()
}

// ReplaceAuth activates a new password directory.
func (s *Store) ReplaceAuth(dir *AuthDirectory) {
	if dir == nil {
		return
	}
	s.auth.Store(dir)
}

// Replace activates snap when its generation is strictly greater than the
// current one. Replaying an equal generation is a no-op.
func (s *Store) Replace(snap *Snapshot) error {
	if snap == nil {
		return errors.New("snapshot is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current := s.ptr.Load(); current != nil && snap.Generation <= current.Generation {
		return fmt.Errorf("snapshot generation %d is not newer than %d", snap.Generation, current.Generation)
	}
	s.ptr.Store(snap)
	return nil
}

// Poller fetches the snapshot from the control plane.
type Poller struct {
	URL      string
	AuthURL  string
	Token    string
	Interval time.Duration
	Client   *http.Client
	Store    *Store
	Logger   *slog.Logger

	mu   sync.Mutex
	etag string
}

// PollerConfig wires a poller.
type PollerConfig struct {
	BaseURL  string
	Token    string
	Interval time.Duration
	Client   *http.Client
	Store    *Store
	Logger   *slog.Logger
}

// NewPoller builds a poller for <baseURL>/internal/v1/snapshot.
func NewPoller(cfg PollerConfig) *Poller {
	interval := cfg.Interval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	store := cfg.Store
	if store == nil {
		store = NewStore()
	}
	return &Poller{
		URL:      strings.TrimSuffix(cfg.BaseURL, "/") + "/internal/v1/snapshot",
		AuthURL:  strings.TrimSuffix(cfg.BaseURL, "/") + "/internal/v1/auth-policies",
		Token:    cfg.Token,
		Interval: interval,
		Client:   client,
		Store:    store,
		Logger:   logger,
	}
}

// Run polls until ctx is cancelled. The first poll happens immediately.
func (p *Poller) Run(ctx context.Context) {
	if err := p.Poll(ctx); err != nil && ctx.Err() == nil {
		p.Logger.Error("snapshot poll failed", slog.String("error", err.Error()))
	}
	ticker := time.NewTicker(p.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := p.Poll(ctx); err != nil && ctx.Err() == nil {
				p.Logger.Error("snapshot poll failed", slog.String("error", err.Error()))
			}
		}
	}
}

// Force drops the cached ETag and fetches a fresh snapshot.
func (p *Poller) Force(ctx context.Context) error {
	p.mu.Lock()
	p.etag = ""
	p.mu.Unlock()
	return p.Poll(ctx)
}

// Poll performs one conditional request.
func (p *Poller) Poll(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return fmt.Errorf("build snapshot request: %w", err)
	}
	p.mu.Lock()
	etag := p.etag
	p.mu.Unlock()
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}

	resp, err := p.Client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch snapshot: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil
	case http.StatusOK:
	default:
		return fmt.Errorf("snapshot endpoint returned %s", resp.Status)
	}

	var snap Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return fmt.Errorf("decode snapshot: %w", err)
	}
	if err := snap.Validate(); err != nil {
		return fmt.Errorf("invalid snapshot: %w", err)
	}
	dir, err := p.fetchAuth(ctx)
	if err != nil {
		return err
	}
	if err := p.Store.Replace(&snap); err != nil {
		return err
	}
	p.Store.ReplaceAuth(dir)
	p.mu.Lock()
	p.etag = resp.Header.Get("ETag")
	p.mu.Unlock()
	return nil
}

// fetchAuth pulls password hashes, which deliberately live outside the
// snapshot body (FR-08). The snapshot only advances when this succeeds.
func (p *Poller) fetchAuth(ctx context.Context) (*AuthDirectory, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.AuthURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build auth directory request: %w", err)
	}
	if p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch auth directory: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("auth directory endpoint returned %s", resp.Status)
	}
	var dir AuthDirectory
	if err := json.NewDecoder(resp.Body).Decode(&dir); err != nil {
		return nil, fmt.Errorf("decode auth directory: %w", err)
	}
	if dir.Policies == nil {
		dir.Policies = map[string]AuthPolicySecrets{}
	}
	return &dir, nil
}
