// Package s3static registers the http.handlers.s3static Caddy module.
// The module wires the PageForge data plane: snapshot polling, credential
// resolution, manifest reading, disk caching and request handling.
package s3static

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/pageforge/pageforge/internal/cache"
	"github.com/pageforge/pageforge/internal/credential"
	"github.com/pageforge/pageforge/internal/handler"
	"github.com/pageforge/pageforge/internal/manifest"
	"github.com/pageforge/pageforge/internal/snapshot"
)

const defaultReservedPrefix = "/__pageforge"

// DefaultCacheTTL is the default idle time after which a cached object is
// removed. It is measured from the last access, not from the fill time.
const DefaultCacheTTL = 24 * time.Hour

// DefaultCacheEvictRatio is the occupancy the cache is reduced to when the
// byte budget is reached.
const DefaultCacheEvictRatio = 0.6

// DefaultCacheMaxBytes is the default cache budget: 10GB.
const DefaultCacheMaxBytes int64 = 10 << 30

// Timeout defaults, following nginx conventions.
const (
	// DefaultUpstreamConnectTimeout bounds connecting to storage.
	DefaultUpstreamConnectTimeout = 10 * time.Second
	// DefaultUpstreamReadTimeout bounds waiting for the first byte from storage.
	DefaultUpstreamReadTimeout = 10 * time.Second
	// DefaultUpstreamIdleTimeout aborts an upstream body read that stalls.
	DefaultUpstreamIdleTimeout = 60 * time.Second
	// DefaultClientIdleTimeout aborts a stalled client transfer.
	DefaultClientIdleTimeout = 60 * time.Second
)

func init() {
	caddy.RegisterModule(S3Static{})
	httpcaddyfile.RegisterHandlerDirective("s3static", parseCaddyfile)
}

// S3Static is the data-plane handler module.
type S3Static struct {
	ControlURL            string         `json:"control_url,omitempty"`
	InternalToken         string         `json:"internal_token,omitempty"`
	InternalTokenFile     string         `json:"internal_token_file,omitempty"`
	PollInterval          caddy.Duration `json:"poll_interval,omitempty"`
	CacheDir              string         `json:"cache_dir,omitempty"`
	CacheMaxBytes         int64          `json:"cache_max_bytes,omitempty"`
	CacheTTL              caddy.Duration `json:"cache_ttl,omitempty"`
	CacheEvictRatio       float64        `json:"cache_evict_ratio,omitempty"`
	ManifestCacheMaxBytes int64          `json:"manifest_cache_max_bytes,omitempty"`
	ManifestCacheTTL      caddy.Duration `json:"manifest_cache_ttl,omitempty"`
	ReservedPrefix        string         `json:"reserved_prefix,omitempty"`

	// Timeouts follow nginx semantics. Upstream* govern the storage side;
	// Client* govern the client side.
	// UpstreamConnectTimeout bounds TCP/TLS connect to storage
	// (proxy_connect_timeout).
	UpstreamConnectTimeout caddy.Duration `json:"upstream_connect_timeout,omitempty"`
	// UpstreamReadTimeout bounds waiting for the first byte from storage
	// (proxy_read_timeout, header phase).
	UpstreamReadTimeout caddy.Duration `json:"upstream_read_timeout,omitempty"`
	// UpstreamIdleTimeout aborts an upstream body read that stalls for this long
	// (proxy_read_timeout, body phase). A flowing download keeps resetting it.
	UpstreamIdleTimeout caddy.Duration `json:"upstream_idle_timeout,omitempty"`
	// UpstreamTotalTimeout is an optional hard ceiling on one upstream fetch
	// (0 disables it).
	UpstreamTotalTimeout caddy.Duration `json:"upstream_total_timeout,omitempty"`
	// ClientIdleTimeout is the maximum idle time between proxy and client
	// (send_timeout). A steady download never trips it.
	ClientIdleTimeout caddy.Duration `json:"client_idle_timeout,omitempty"`
	// ClientTotalTimeout is an optional hard ceiling on the whole client request
	// (0 disables it).
	ClientTotalTimeout caddy.Duration `json:"client_total_timeout,omitempty"`

	handler *handler.Handler
	cache   *cache.Cache
	cancel  context.CancelFunc
}

// CaddyModule implements caddy.Module.
func (S3Static) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.s3static",
		New: func() caddy.Module { return new(S3Static) },
	}
}

// Provision implements caddy.Provisioner.
func (s *S3Static) Provision(ctx caddy.Context) error {
	logger := ctx.Slogger()
	if s.ReservedPrefix == "" {
		s.ReservedPrefix = defaultReservedPrefix
	}
	s.ReservedPrefix = strings.TrimSuffix(s.ReservedPrefix, "/")
	if s.PollInterval <= 0 {
		s.PollInterval = caddy.Duration(snapshot.DefaultPollInterval)
	}
	if s.CacheTTL <= 0 {
		s.CacheTTL = caddy.Duration(DefaultCacheTTL)
	}
	if s.CacheEvictRatio <= 0 || s.CacheEvictRatio >= 1 {
		s.CacheEvictRatio = DefaultCacheEvictRatio
	}
	if s.CacheDir != "" && s.CacheMaxBytes == 0 {
		s.CacheMaxBytes = DefaultCacheMaxBytes
	}
	if s.ManifestCacheMaxBytes == 0 {
		s.ManifestCacheMaxBytes = manifest.DefaultMaxBytes
	}
	if s.CacheMaxBytes < 0 {
		return fmt.Errorf("cache_max_bytes must not be negative")
	}
	// Timeout defaults (nginx-like): connect and first-byte read default to 10s,
	// idle body transfers default to 60s. Total ceilings stay disabled unless set.
	if s.UpstreamConnectTimeout <= 0 {
		s.UpstreamConnectTimeout = caddy.Duration(DefaultUpstreamConnectTimeout)
	}
	if s.UpstreamReadTimeout <= 0 {
		s.UpstreamReadTimeout = caddy.Duration(DefaultUpstreamReadTimeout)
	}
	if s.UpstreamIdleTimeout <= 0 {
		s.UpstreamIdleTimeout = caddy.Duration(DefaultUpstreamIdleTimeout)
	}
	if s.ClientIdleTimeout <= 0 {
		s.ClientIdleTimeout = caddy.Duration(DefaultClientIdleTimeout)
	}

	token := s.InternalToken
	if s.InternalTokenFile != "" {
		body, err := os.ReadFile(s.InternalTokenFile)
		if err != nil {
			return fmt.Errorf("read internal_token_file: %w", err)
		}
		token = strings.TrimSpace(string(body))
	}

	var diskCache *cache.Cache
	if s.CacheDir != "" {
		c, err := cache.Open(cache.Options{
			Dir:        s.CacheDir,
			MaxBytes:   s.CacheMaxBytes,
			TTL:        time.Duration(s.CacheTTL),
			EvictRatio: s.CacheEvictRatio,
			Logger:     logger,
		})
		if err != nil {
			return fmt.Errorf("open cache_dir: %w", err)
		}
		s.cache = c
		diskCache = c
	}

	snapshots := snapshot.NewStore()
	poller := snapshot.NewPoller(snapshot.PollerConfig{
		BaseURL:  s.ControlURL,
		Token:    token,
		Interval: time.Duration(s.PollInterval),
		Store:    snapshots,
		Logger:   logger,
	})
	pollCtx, cancel := context.WithCancel(ctx.Context)
	s.cancel = cancel
	go poller.Run(pollCtx)

	manifests := manifest.NewReader()
	manifests.MaxBytes = s.ManifestCacheMaxBytes
	manifests.TTL = time.Duration(s.ManifestCacheTTL)

	credClient := credential.NewClient(s.ControlURL, token)
	credClient.ConnectTimeout = time.Duration(s.UpstreamConnectTimeout)
	credClient.ReadTimeout = time.Duration(s.UpstreamReadTimeout)

	s.handler = handler.New(handler.Options{
		Snapshots:            snapshots,
		Stores:               credClient,
		Manifests:            manifests,
		Cache:                diskCache,
		ReservedPrefix:       s.ReservedPrefix,
		ControlURL:           s.ControlURL,
		InternalToken:        token,
		ClientIdleTimeout:    time.Duration(s.ClientIdleTimeout),
		ClientTotalTimeout:   time.Duration(s.ClientTotalTimeout),
		UpstreamIdleTimeout:  time.Duration(s.UpstreamIdleTimeout),
		UpstreamTotalTimeout: time.Duration(s.UpstreamTotalTimeout),
		Logger:               logger,
	})
	return nil
}

// Cleanup implements caddy.CleanerUpper.
func (s *S3Static) Cleanup() error {
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	if s.cache != nil {
		_ = s.cache.Close()
		s.cache = nil
	}
	return nil
}

// Validate implements caddy.Validator.
func (s *S3Static) Validate() error {
	if !strings.HasPrefix(s.ReservedPrefix, "/") || len(s.ReservedPrefix) < 2 {
		return fmt.Errorf("reserved_prefix %q must start with / and must not be root", s.ReservedPrefix)
	}
	if strings.TrimSpace(s.ControlURL) == "" {
		return fmt.Errorf("control_url is required")
	}
	if s.CacheMaxBytes < 0 {
		return fmt.Errorf("cache_max_bytes must not be negative")
	}
	if s.CacheTTL < 0 {
		return fmt.Errorf("cache_ttl must not be negative")
	}
	if s.CacheEvictRatio < 0 || s.CacheEvictRatio >= 1 {
		return fmt.Errorf("cache_evict_ratio must be in [0, 1)")
	}
	if s.ManifestCacheMaxBytes < 0 {
		return fmt.Errorf("manifest_cache_max_bytes must not be negative")
	}
	if s.ManifestCacheTTL < 0 {
		return fmt.Errorf("manifest_cache_ttl must not be negative")
	}
	return nil
}

// ServeHTTP implements caddyhttp.MiddlewareHandler.
func (s *S3Static) ServeHTTP(w http.ResponseWriter, r *http.Request, _ caddyhttp.Handler) error {
	if s.handler == nil {
		http.Error(w, "s3static is not provisioned", http.StatusInternalServerError)
		return nil
	}
	s.handler.ServeHTTP(w, r)
	return nil
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
func (s *S3Static) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // consume directive name
	for d.NextBlock(0) {
		switch d.Val() {
		case "control_url":
			if !d.NextArg() {
				return d.ArgErr()
			}
			s.ControlURL = d.Val()
		case "internal_token":
			if !d.NextArg() {
				return d.ArgErr()
			}
			s.InternalToken = d.Val()
		case "internal_token_file":
			if !d.NextArg() {
				return d.ArgErr()
			}
			s.InternalTokenFile = d.Val()
		case "poll_interval":
			if !d.NextArg() {
				return d.ArgErr()
			}
			value, err := time.ParseDuration(d.Val())
			if err != nil {
				return d.Errf("invalid poll_interval %q: %v", d.Val(), err)
			}
			s.PollInterval = caddy.Duration(value)
		case "cache_dir":
			if !d.NextArg() {
				return d.ArgErr()
			}
			s.CacheDir = d.Val()
		case "cache_max_bytes":
			if !d.NextArg() {
				return d.ArgErr()
			}
			var value int64
			if _, err := fmt.Sscan(d.Val(), &value); err != nil {
				return d.Errf("invalid cache_max_bytes %q", d.Val())
			}
			s.CacheMaxBytes = value
		case "cache_ttl":
			if !d.NextArg() {
				return d.ArgErr()
			}
			value, err := time.ParseDuration(d.Val())
			if err != nil {
				return d.Errf("invalid cache_ttl %q: %v", d.Val(), err)
			}
			if value <= 0 {
				return d.Errf("cache_ttl must be positive, got %q", d.Val())
			}
			s.CacheTTL = caddy.Duration(value)
		case "cache_evict_ratio":
			if !d.NextArg() {
				return d.ArgErr()
			}
			var value float64
			if _, err := fmt.Sscan(d.Val(), &value); err != nil {
				return d.Errf("invalid cache_evict_ratio %q", d.Val())
			}
			if value <= 0 || value >= 1 {
				return d.Errf("cache_evict_ratio must be in (0, 1), got %q", d.Val())
			}
			s.CacheEvictRatio = value
		case "manifest_cache_max_bytes":
			if !d.NextArg() {
				return d.ArgErr()
			}
			var value int64
			if _, err := fmt.Sscan(d.Val(), &value); err != nil {
				return d.Errf("invalid manifest_cache_max_bytes %q", d.Val())
			}
			if value < 0 {
				return d.Errf("manifest_cache_max_bytes must not be negative, got %q", d.Val())
			}
			s.ManifestCacheMaxBytes = value
		case "manifest_cache_ttl":
			if !d.NextArg() {
				return d.ArgErr()
			}
			value, err := time.ParseDuration(d.Val())
			if err != nil {
				return d.Errf("invalid manifest_cache_ttl %q: %v", d.Val(), err)
			}
			if value < 0 {
				return d.Errf("manifest_cache_ttl must not be negative, got %q", d.Val())
			}
			s.ManifestCacheTTL = caddy.Duration(value)
		case "upstream_connect_timeout":
			if !d.NextArg() {
				return d.ArgErr()
			}
			value, err := time.ParseDuration(d.Val())
			if err != nil {
				return d.Errf("invalid upstream_connect_timeout %q: %v", d.Val(), err)
			}
			s.UpstreamConnectTimeout = caddy.Duration(value)
		case "upstream_read_timeout":
			if !d.NextArg() {
				return d.ArgErr()
			}
			value, err := time.ParseDuration(d.Val())
			if err != nil {
				return d.Errf("invalid upstream_read_timeout %q: %v", d.Val(), err)
			}
			s.UpstreamReadTimeout = caddy.Duration(value)
		case "upstream_idle_timeout":
			if !d.NextArg() {
				return d.ArgErr()
			}
			value, err := time.ParseDuration(d.Val())
			if err != nil {
				return d.Errf("invalid upstream_idle_timeout %q: %v", d.Val(), err)
			}
			s.UpstreamIdleTimeout = caddy.Duration(value)
		case "upstream_total_timeout":
			if !d.NextArg() {
				return d.ArgErr()
			}
			value, err := time.ParseDuration(d.Val())
			if err != nil {
				return d.Errf("invalid upstream_total_timeout %q: %v", d.Val(), err)
			}
			s.UpstreamTotalTimeout = caddy.Duration(value)
		case "client_idle_timeout":
			if !d.NextArg() {
				return d.ArgErr()
			}
			value, err := time.ParseDuration(d.Val())
			if err != nil {
				return d.Errf("invalid client_idle_timeout %q: %v", d.Val(), err)
			}
			s.ClientIdleTimeout = caddy.Duration(value)
		case "client_total_timeout":
			if !d.NextArg() {
				return d.ArgErr()
			}
			value, err := time.ParseDuration(d.Val())
			if err != nil {
				return d.Errf("invalid client_total_timeout %q: %v", d.Val(), err)
			}
			s.ClientTotalTimeout = caddy.Duration(value)
		case "reserved_prefix":
			if !d.NextArg() {
				return d.ArgErr()
			}
			s.ReservedPrefix = d.Val()
		default:
			return d.Errf("unrecognized s3static option %q", d.Val())
		}
	}
	return nil
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var m S3Static
	if err := m.UnmarshalCaddyfile(h.Dispenser); err != nil {
		return nil, err
	}
	return &m, nil
}

var (
	_ caddy.Module                = (*S3Static)(nil)
	_ caddy.Provisioner           = (*S3Static)(nil)
	_ caddy.Validator             = (*S3Static)(nil)
	_ caddy.CleanerUpper          = (*S3Static)(nil)
	_ caddyhttp.MiddlewareHandler = (*S3Static)(nil)
	_ caddyfile.Unmarshaler       = (*S3Static)(nil)
)
