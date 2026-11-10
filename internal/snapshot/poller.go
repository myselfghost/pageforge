package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultPollInterval matches the documented two-second refresh cadence.
const DefaultPollInterval = 2 * time.Second

const (
	// BootstrapRetries is how many extra attempts follow the first control-plane
	// fetch at startup before the poller falls back to the disk cache.
	BootstrapRetries = 3
	// bootstrapAttemptTimeout bounds one startup attempt so a hung control plane
	// cannot block the fallback for the full poll client timeout.
	bootstrapAttemptTimeout = 5 * time.Second
	// bootstrapRetryDelay spaces out startup attempts.
	bootstrapRetryDelay = 500 * time.Millisecond

	// cacheFileName is the snapshot copy persisted for restarts.
	cacheFileName = "snapshot.json"
	// authCacheFileName is the password directory copy persisted for restarts.
	authCacheFileName = "auth-policies.json"
	// credentialCacheFileName is the S3 credential directory copy persisted for
	// restarts.
	credentialCacheFileName = "aliases.json"
	// shareCacheFileName is the share-link directory copy persisted for restarts.
	shareCacheFileName = "shares.json"
	// tempFilePrefix marks in-flight writes; leftovers are removed on startup.
	tempFilePrefix = ".pf-snap-"
)

var (
	// ErrStaleGeneration means the fetched snapshot is older than the active
	// one and must never replace it.
	ErrStaleGeneration = errors.New("snapshot generation is stale")
	// ErrSameGeneration means the fetched snapshot has the same generation as
	// the active one. The route data is identical, so callers may keep serving
	// it while still refreshing separately fetched data such as password hashes.
	ErrSameGeneration = errors.New("snapshot generation is already active")
)

// Store holds the active snapshot. Readers never block on the network; the
// poller swaps the pointer atomically after validation.
type Store struct {
	mu     sync.Mutex
	ptr    atomic.Pointer[Snapshot]
	auth   atomic.Pointer[AuthDirectory]
	creds  atomic.Pointer[CredentialDirectory]
	shares atomic.Pointer[ShareDirectory]
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

// Credentials returns the S3 credential directory fetched separately from the
// snapshot.
func (s *Store) Credentials() *CredentialDirectory {
	return s.creds.Load()
}

// ReplaceCredentials activates a new S3 credential directory.
func (s *Store) ReplaceCredentials(dir *CredentialDirectory) {
	if dir == nil {
		return
	}
	s.creds.Store(dir)
}

// Shares returns the share-link directory fetched separately from the snapshot.
func (s *Store) Shares() *ShareDirectory {
	return s.shares.Load()
}

// ReplaceShares activates a new share-link directory.
func (s *Store) ReplaceShares(dir *ShareDirectory) {
	if dir == nil {
		return
	}
	s.shares.Store(dir)
}

// Replace activates snap when its generation is newer than the current one.
// An older generation is rejected with ErrStaleGeneration; an equal generation
// is a no-op reported as ErrSameGeneration.
func (s *Store) Replace(snap *Snapshot) error {
	if snap == nil {
		return errors.New("snapshot is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current := s.ptr.Load(); current != nil {
		switch {
		case snap.Generation < current.Generation:
			return fmt.Errorf("%w: %d is older than %d", ErrStaleGeneration, snap.Generation, current.Generation)
		case snap.Generation == current.Generation:
			return fmt.Errorf("%w: generation %d", ErrSameGeneration, snap.Generation)
		}
	}
	s.ptr.Store(snap)
	return nil
}

// Poller fetches the snapshot from the control plane and mirrors the last
// successful result to disk so a restart can survive an unreachable control
// plane.
type Poller struct {
	URL           string
	AuthURL       string
	CredentialURL string
	ShareURL      string
	Token         string
	Interval      time.Duration
	Client        *http.Client
	Store         *Store
	Logger        *slog.Logger
	// CacheDir enables the on-disk snapshot mirror when non-empty. Each
	// successful poll rewrites snapshot.json, auth-policies.json, aliases.json
	// and shares.json there.
	CacheDir string

	mu   sync.Mutex
	etag string
	// sharesHash is the last share directory written to disk. Share view counts
	// change without advancing the snapshot generation, so the 304 path still
	// refreshes shares.json and needs to know whether anything changed.
	sharesHash [32]byte
}

// PollerConfig wires a poller.
type PollerConfig struct {
	BaseURL  string
	Token    string
	Interval time.Duration
	Client   *http.Client
	Store    *Store
	Logger   *slog.Logger
	// CacheDir enables the on-disk snapshot mirror when non-empty.
	CacheDir string
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
	base := strings.TrimSuffix(cfg.BaseURL, "/")
	return &Poller{
		URL:           base + "/internal/v1/snapshot",
		AuthURL:       base + "/internal/v1/auth-policies",
		CredentialURL: base + "/internal/v1/alias-credentials",
		ShareURL:      base + "/internal/v1/share-links",
		Token:         cfg.Token,
		Interval:      interval,
		Client:        client,
		Store:         store,
		Logger:        logger,
		CacheDir:      cfg.CacheDir,
	}
}

// Run bootstraps synchronously (so the store is usable when it returns) and
// then keeps polling until ctx is cancelled. Every failure, including the
// startup bootstrap, leaves the last usable snapshot (in memory or loaded from
// disk) serving traffic.
func (p *Poller) Run(ctx context.Context) {
	if _, err := p.Bootstrap(ctx); err != nil && ctx.Err() == nil {
		p.Logger.Error("snapshot bootstrap failed", slog.String("error", err.Error()))
	}
	p.Loop(ctx)
}

// Loop polls until ctx is cancelled. Call it after Bootstrap when startup must
// wait for the first load (a data plane must not accept traffic before then).
func (p *Poller) Loop(ctx context.Context) {
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

// Bootstrap loads the initial snapshot before regular polling starts: it tries
// the control plane first (1 attempt + BootstrapRetries retries) and only then
// falls back to the on-disk copy written by an earlier successful poll. The
// returned bool reports whether the snapshot came from disk instead of the
// control plane.
func (p *Poller) Bootstrap(ctx context.Context) (bool, error) {
	var lastErr error
	for attempt := 0; attempt <= BootstrapRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(bootstrapRetryDelay):
			}
		}
		attemptCtx, cancel := context.WithTimeout(ctx, bootstrapAttemptTimeout)
		err := p.Poll(attemptCtx)
		cancel()
		if err == nil {
			return false, nil
		}
		lastErr = err
		if errors.Is(err, ErrStaleGeneration) || errors.Is(err, ErrSameGeneration) {
			// The active snapshot is already usable; retrying cannot help.
			return false, nil
		}
		p.Logger.Warn("snapshot bootstrap attempt failed",
			slog.Int("attempt", attempt+1),
			slog.Int("max_attempts", BootstrapRetries+1),
			slog.String("error", err.Error()))
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}

	loaded, err := p.LoadCache()
	if err != nil {
		return false, fmt.Errorf("control plane unavailable after %d attempts and cached snapshot is unusable: %w: %v",
			BootstrapRetries+1, err, lastErr)
	}
	p.Logger.Warn("control plane unreachable at startup, serving snapshot from disk cache",
		slog.Uint64("generation", loaded.Generation),
		slog.Time("published_at", loaded.PublishedAt),
		slog.String("cache_dir", p.CacheDir))
	return true, nil
}

// LoadCache activates the snapshot and password directory persisted by a
// previous successful poll. Entries are validated like remote ones; a missing
// or corrupt cache is reported as an error and leaves the store untouched.
func (p *Poller) LoadCache() (*Snapshot, error) {
	if p.CacheDir == "" {
		return nil, errors.New("snapshot disk cache is disabled")
	}
	snap, err := readSnapshotFile(filepath.Join(p.CacheDir, cacheFileName))
	if err != nil {
		return nil, err
	}
	if err := p.Store.Replace(snap); err != nil {
		return nil, err
	}
	dir, err := readAuthFile(filepath.Join(p.CacheDir, authCacheFileName))
	if err != nil {
		p.Logger.Warn("cached password directory is unusable, authenticated routes will return 503",
			slog.String("error", err.Error()))
	} else {
		p.Store.ReplaceAuth(dir)
	}
	if creds, err := readCredentialFile(filepath.Join(p.CacheDir, credentialCacheFileName)); err != nil {
		p.Logger.Warn("cached credential directory is unusable, object storage will be unavailable",
			slog.String("error", err.Error()))
	} else {
		p.Store.ReplaceCredentials(creds)
	}
	if shares, err := readShareDirectoryFile(filepath.Join(p.CacheDir, shareCacheFileName)); err != nil {
		p.Logger.Warn("cached share directory is unusable, share links will return 503",
			slog.String("error", err.Error()))
	} else {
		p.Store.ReplaceShares(shares)
	}
	return snap, nil
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
		// Routing is unchanged, but credentials, password hashes and share-link
		// counters have no generation of their own and must still be refreshed.
		return p.refreshDirectories(ctx)
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
	// Reject a stale snapshot before touching any dependency, so an older
	// generation can never regress the active data.
	if current := p.Store.Load(); current != nil && snap.Generation < current.Generation {
		return fmt.Errorf("%w: %d is older than %d", ErrStaleGeneration, snap.Generation, current.Generation)
	}
	// Credentials and password hashes gate activation: a snapshot whose aliases
	// have no keys would only produce 502s, so the old snapshot keeps serving
	// until every dependency is fetched.
	if err := p.refreshDirectories(ctx); err != nil {
		return err
	}

	if replaceErr := p.Store.Replace(&snap); replaceErr != nil && !errors.Is(replaceErr, ErrSameGeneration) {
		return replaceErr
	}
	if p.CacheDir != "" {
		p.writeCache(&snap)
	}
	// Remember the ETag even when the generation was already active: the routing
	// body is identical, so the next poll can ask conditionally instead of
	// downloading it again.
	p.mu.Lock()
	p.etag = resp.Header.Get("ETag")
	p.mu.Unlock()
	return nil
}

// refreshDirectories fetches and activates the credential, password and
// share-link directories. Credential and password failures are returned because
// activating a snapshot without them would break every request; a share-link
// failure keeps the previous directory, since share links are opt-in per route.
func (p *Poller) refreshDirectories(ctx context.Context) error {
	creds, err := fetchCredentialDirectory(ctx, p.Client, p.CredentialURL, p.Token)
	if err != nil {
		return err
	}
	auth, err := fetchAuthDirectory(ctx, p.Client, p.AuthURL, p.Token)
	if err != nil {
		return err
	}
	p.Store.ReplaceCredentials(creds)
	p.Store.ReplaceAuth(auth)

	shares, err := fetchShareDirectory(ctx, p.Client, p.ShareURL, p.Token)
	if err != nil {
		p.Logger.Warn("share link directory fetch failed", slog.String("error", err.Error()))
		return nil
	}
	p.Store.ReplaceShares(shares)
	p.persistShares(shares)
	return nil
}

// persistShares rewrites shares.json when the directory changed. Share view
// counts and link edits never advance the snapshot generation, so this cannot
// hang off the generation path alone.
func (p *Poller) persistShares(dir *ShareDirectory) {
	if p.CacheDir == "" {
		return
	}
	body, err := json.Marshal(dir)
	if err != nil {
		p.Logger.Error("encode share directory failed", slog.String("error", err.Error()))
		return
	}
	hash := sha256.Sum256(body)
	p.mu.Lock()
	unchanged := p.sharesHash == hash
	p.sharesHash = hash
	p.mu.Unlock()
	if unchanged {
		return
	}
	if err := writeSnapshotFile(filepath.Join(p.CacheDir, shareCacheFileName), dir); err != nil {
		p.Logger.Error("write share directory disk cache failed", slog.String("error", err.Error()))
	}
}

// writeCache persists the snapshot and the generation-independent directories
// for the next startup. shares.json is written by persistShares. Failures are
// logged and never disturb the active data.
func (p *Poller) writeCache(snap *Snapshot) {
	if snap != nil {
		if err := writeSnapshotFile(filepath.Join(p.CacheDir, cacheFileName), snap); err != nil {
			p.Logger.Error("write snapshot disk cache failed", slog.String("error", err.Error()))
		}
	}
	if dir := p.Store.Auth(); dir != nil {
		if err := writeSnapshotFile(filepath.Join(p.CacheDir, authCacheFileName), dir); err != nil {
			p.Logger.Error("write password directory disk cache failed", slog.String("error", err.Error()))
		}
	}
	if dir := p.Store.Credentials(); dir != nil {
		if err := writeSnapshotFile(filepath.Join(p.CacheDir, credentialCacheFileName), dir); err != nil {
			p.Logger.Error("write credential directory disk cache failed", slog.String("error", err.Error()))
		}
	}
}

// writeSnapshotFile encodes value next to the target and renames it, so a crash
// mid-write never leaves a truncated cache file behind.
func writeSnapshotFile(path string, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode %s: %w", filepath.Base(path), err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, tempFilePrefix+"*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

func readSnapshotFile(path string) (*Snapshot, error) {
	var snap Snapshot
	if err := readJSONFile(path, &snap); err != nil {
		return nil, err
	}
	if err := snap.Validate(); err != nil {
		return nil, fmt.Errorf("invalid cached snapshot %s: %w", path, err)
	}
	return &snap, nil
}

func readAuthFile(path string) (*AuthDirectory, error) {
	var dir AuthDirectory
	if err := readJSONFile(path, &dir); err != nil {
		return nil, err
	}
	if dir.Policies == nil {
		dir.Policies = map[string]AuthPolicySecrets{}
	}
	return &dir, nil
}

func readCredentialFile(path string) (*CredentialDirectory, error) {
	var dir CredentialDirectory
	if err := readJSONFile(path, &dir); err != nil {
		return nil, err
	}
	if dir.Aliases == nil {
		// Compatibility: an earlier build mirrored a bare alias→credentials map.
		var legacy map[string]AliasCredentials
		if err := readJSONFile(path, &legacy); err == nil && len(legacy) > 0 {
			dir.Aliases = legacy
		} else {
			dir.Aliases = map[string]AliasCredentials{}
		}
	}
	return &dir, nil
}

func readShareDirectoryFile(path string) (*ShareDirectory, error) {
	var dir ShareDirectory
	if err := readJSONFile(path, &dir); err != nil {
		return nil, err
	}
	if dir.Links == nil {
		dir.Links = map[string]ShareLinkSecrets{}
	}
	return &dir, nil
}

func readJSONFile(path string, value any) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(body, value); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// fetchAuthDirectory pulls password hashes, which deliberately live outside the
// snapshot body (FR-08). The snapshot only advances when this succeeds.
func fetchAuthDirectory(ctx context.Context, client *http.Client, url, token string) (*AuthDirectory, error) {
	var dir AuthDirectory
	if err := fetchJSON(ctx, client, url, token, &dir); err != nil {
		return nil, fmt.Errorf("fetch auth directory: %w", err)
	}
	if dir.Policies == nil {
		dir.Policies = map[string]AuthPolicySecrets{}
	}
	return &dir, nil
}

// fetchCredentialDirectory pulls every alias's S3 credentials so the data plane
// never calls the control plane per request.
func fetchCredentialDirectory(ctx context.Context, client *http.Client, url, token string) (*CredentialDirectory, error) {
	var dir CredentialDirectory
	if err := fetchJSON(ctx, client, url, token, &dir); err != nil {
		return nil, fmt.Errorf("fetch credential directory: %w", err)
	}
	if dir.Aliases == nil {
		dir.Aliases = map[string]AliasCredentials{}
	}
	// Drop aliases with no usable secret: an object store built from an empty
	// key would only fail later with a confusing storage error.
	for id, creds := range dir.Aliases {
		if creds.AccessKey == "" || len(creds.SecretCipher) == 0 {
			delete(dir.Aliases, id)
		}
	}
	return &dir, nil
}

// fetchShareDirectory pulls every share link's authorization material so an
// offline data plane can still validate tokens, passwords and budgets.
func fetchShareDirectory(ctx context.Context, client *http.Client, url, token string) (*ShareDirectory, error) {
	var dir ShareDirectory
	if err := fetchJSON(ctx, client, url, token, &dir); err != nil {
		return nil, fmt.Errorf("fetch share directory: %w", err)
	}
	if dir.Links == nil {
		dir.Links = map[string]ShareLinkSecrets{}
	}
	return &dir, nil
}

func fetchJSON(ctx context.Context, client *http.Client, url, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("endpoint returned %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
