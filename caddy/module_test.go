package s3static

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/pageforge/pageforge/internal/handler"
	"github.com/pageforge/pageforge/internal/manifest"
	"github.com/pageforge/pageforge/internal/snapshot"
)

func TestServeHTTPWithoutProvisioning(t *testing.T) {
	h := &S3Static{ReservedPrefix: "/__pageforge"}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/url1/foo.js", nil)

	if err := h.ServeHTTP(rec, req, nil); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 before provisioning", rec.Code)
	}
}

func TestServeHTTPHealth(t *testing.T) {
	h := &S3Static{
		ReservedPrefix: "/__pageforge",
		handler: handler.New(handler.Options{
			Snapshots:      snapshot.NewStore(),
			ReservedPrefix: "/__pageforge",
		}),
	}
	for _, path := range []string{"/__pageforge/healthz", "/__pageforge/readyz"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if err := h.ServeHTTP(rec, req, nil); err != nil {
			t.Fatalf("ServeHTTP %s: %v", path, err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", path, rec.Code)
		}
	}
}

func TestUnmarshalCaddyfile(t *testing.T) {
	d := caddyfile.NewTestDispenser(`s3static {
		control_url http://127.0.0.1:8081
		internal_token_file /etc/pageforge/internal.token
		poll_interval 5s
		cache_dir /var/cache/pageforge
		cache_max_bytes 1048576
		cache_ttl 5m
		cache_evict_ratio 0.7
		manifest_cache_max_bytes 2097152
		manifest_cache_ttl 10m
		upstream_connect_timeout 3s
		upstream_read_timeout 4s
		upstream_idle_timeout 90s
		upstream_total_timeout 30m
		client_idle_timeout 45s
		client_total_timeout 2h
		reserved_prefix /_pf
	}`)

	var m S3Static
	if err := m.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile: %v", err)
	}
	if m.ControlURL != "http://127.0.0.1:8081" {
		t.Fatalf("control_url = %q", m.ControlURL)
	}
	if m.InternalTokenFile != "/etc/pageforge/internal.token" {
		t.Fatalf("internal_token_file = %q", m.InternalTokenFile)
	}
	if m.CacheDir != "/var/cache/pageforge" {
		t.Fatalf("cache_dir = %q", m.CacheDir)
	}
	if m.CacheMaxBytes != 1048576 {
		t.Fatalf("cache_max_bytes = %d", m.CacheMaxBytes)
	}
	if time.Duration(m.CacheTTL) != 5*time.Minute {
		t.Fatalf("cache_ttl = %v", m.CacheTTL)
	}
	if m.CacheEvictRatio != 0.7 {
		t.Fatalf("cache_evict_ratio = %v", m.CacheEvictRatio)
	}
	if m.ManifestCacheMaxBytes != 2097152 {
		t.Fatalf("manifest_cache_max_bytes = %d", m.ManifestCacheMaxBytes)
	}
	if time.Duration(m.ManifestCacheTTL) != 10*time.Minute {
		t.Fatalf("manifest_cache_ttl = %v", m.ManifestCacheTTL)
	}
	if m.ReservedPrefix != "/_pf" {
		t.Fatalf("reserved_prefix = %q", m.ReservedPrefix)
	}
	if time.Duration(m.PollInterval) != 5*time.Second {
		t.Fatalf("poll_interval = %v", m.PollInterval)
	}
	if time.Duration(m.UpstreamConnectTimeout) != 3*time.Second {
		t.Fatalf("upstream_connect_timeout = %v", m.UpstreamConnectTimeout)
	}
	if time.Duration(m.UpstreamReadTimeout) != 4*time.Second {
		t.Fatalf("upstream_read_timeout = %v", m.UpstreamReadTimeout)
	}
	if time.Duration(m.UpstreamIdleTimeout) != 90*time.Second {
		t.Fatalf("upstream_idle_timeout = %v", m.UpstreamIdleTimeout)
	}
	if time.Duration(m.UpstreamTotalTimeout) != 30*time.Minute {
		t.Fatalf("upstream_total_timeout = %v", m.UpstreamTotalTimeout)
	}
	if time.Duration(m.ClientIdleTimeout) != 45*time.Second {
		t.Fatalf("client_idle_timeout = %v", m.ClientIdleTimeout)
	}
	if time.Duration(m.ClientTotalTimeout) != 2*time.Hour {
		t.Fatalf("client_total_timeout = %v", m.ClientTotalTimeout)
	}
}

func TestUnmarshalCaddyfileRejectsNonPositiveCacheTTL(t *testing.T) {
	for _, value := range []string{"0", "-1s"} {
		d := caddyfile.NewTestDispenser(`s3static {
			cache_ttl ` + value + `
		}`)
		var m S3Static
		if err := m.UnmarshalCaddyfile(d); err == nil {
			t.Fatalf("cache_ttl %q must be rejected", value)
		}
	}
}

// testMasterKey is a valid 32-byte AES key in base64, matching testutil.
const testMasterKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

func TestProvisionRequiresMasterKey(t *testing.T) {
	m := S3Static{ControlURL: "http://127.0.0.1:8081", ReservedPrefix: "/__pageforge"}
	if err := m.Provision(caddy.Context{Context: context.Background()}); err == nil {
		t.Fatal("Provision must fail without master_key: sealed secrets cannot be decrypted")
	}
	defer func() { _ = m.Cleanup() }()

	bad := S3Static{ControlURL: "http://127.0.0.1:8081", ReservedPrefix: "/__pageforge", MasterKey: "not-base64"}
	if err := bad.Provision(caddy.Context{Context: context.Background()}); err == nil {
		t.Fatal("Provision must reject a malformed master_key")
	}
	defer func() { _ = bad.Cleanup() }()
}

func TestProvisionDefaultsCacheTTL(t *testing.T) {
	m := S3Static{ControlURL: "http://127.0.0.1:8081", ReservedPrefix: "/__pageforge", MasterKey: testMasterKey}
	if err := m.Provision(caddy.Context{Context: context.Background()}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer func() { _ = m.Cleanup() }()
	if time.Duration(m.CacheTTL) != 24*time.Hour {
		t.Fatalf("default cache_ttl = %v, want 24h", m.CacheTTL)
	}
	if m.CacheEvictRatio != DefaultCacheEvictRatio {
		t.Fatalf("default cache_evict_ratio = %v, want %v", m.CacheEvictRatio, DefaultCacheEvictRatio)
	}
}

func TestProvisionDefaultsCacheBudget(t *testing.T) {
	m := S3Static{
		ControlURL:     "http://127.0.0.1:8081",
		ReservedPrefix: "/__pageforge",
		MasterKey:      testMasterKey,
		CacheDir:       t.TempDir(),
	}
	if err := m.Provision(caddy.Context{Context: context.Background()}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer func() { _ = m.Cleanup() }()
	if m.CacheMaxBytes != DefaultCacheMaxBytes {
		t.Fatalf("default cache_max_bytes = %d, want %d", m.CacheMaxBytes, DefaultCacheMaxBytes)
	}
}

func TestUnmarshalCaddyfileRejectsBadEvictRatio(t *testing.T) {
	for _, value := range []string{"0", "1", "1.2", "-0.2"} {
		d := caddyfile.NewTestDispenser(`s3static {
			cache_evict_ratio ` + value + `
		}`)
		var m S3Static
		if err := m.UnmarshalCaddyfile(d); err == nil {
			t.Fatalf("cache_evict_ratio %q must be rejected", value)
		}
	}
}

func TestProvisionDefaultsManifestCache(t *testing.T) {
	m := S3Static{ControlURL: "http://127.0.0.1:8081", ReservedPrefix: "/__pageforge", MasterKey: testMasterKey}
	if err := m.Provision(caddy.Context{Context: context.Background()}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer func() { _ = m.Cleanup() }()
	if m.ManifestCacheMaxBytes != manifest.DefaultMaxBytes {
		t.Fatalf("default manifest_cache_max_bytes = %d, want %d", m.ManifestCacheMaxBytes, manifest.DefaultMaxBytes)
	}
	if m.ManifestCacheTTL != 0 {
		t.Fatalf("default manifest_cache_ttl = %v, want 0", m.ManifestCacheTTL)
	}
}

func TestUnmarshalCaddyfileRejectsBadManifestCache(t *testing.T) {
	blocks := []string{
		"manifest_cache_max_bytes -1",
		"manifest_cache_ttl -1s",
	}
	for _, block := range blocks {
		d := caddyfile.NewTestDispenser(`s3static {
			` + block + `
		}`)
		var m S3Static
		if err := m.UnmarshalCaddyfile(d); err == nil {
			t.Fatalf("%q must be rejected", block)
		}
	}
}

func TestValidateRejectsNegativeManifestCache(t *testing.T) {
	for _, m := range []S3Static{
		{ReservedPrefix: "/__pageforge", ControlURL: "http://127.0.0.1:8081", ManifestCacheMaxBytes: -1},
		{ReservedPrefix: "/__pageforge", ControlURL: "http://127.0.0.1:8081", ManifestCacheTTL: -1},
	} {
		if err := m.Validate(); err == nil {
			t.Fatal("negative manifest cache settings must fail validation")
		}
	}
}

func TestUnmarshalCaddyfileRejectsUnknownOption(t *testing.T) {
	d := caddyfile.NewTestDispenser(`s3static {
		unknown_option value
	}`)
	var m S3Static
	if err := m.UnmarshalCaddyfile(d); err == nil {
		t.Fatal("unknown option must be rejected")
	}
}

func TestValidateRejectsBadPrefix(t *testing.T) {
	for _, prefix := range []string{"_pf", "/"} {
		m := S3Static{ReservedPrefix: prefix, ControlURL: "http://127.0.0.1:8081"}
		if err := m.Validate(); err == nil {
			t.Fatalf("prefix %q should fail validation", prefix)
		}
	}
}

func TestValidateRequiresControlURL(t *testing.T) {
	m := S3Static{ReservedPrefix: "/__pageforge"}
	if err := m.Validate(); err == nil {
		t.Fatal("missing control_url must fail validation")
	}
	m.ControlURL = "http://127.0.0.1:8081"
	if err := m.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if err := (&S3Static{ReservedPrefix: "/__pageforge", ControlURL: "http://127.0.0.1:8081", CacheMaxBytes: -1}).Validate(); err == nil {
		t.Fatal("negative cache_max_bytes must fail validation")
	}
}

func TestProvisionDoesNotDeriveSnapshotCacheDirFromObjectCache(t *testing.T) {
	dir := t.TempDir()
	m := S3Static{
		ControlURL:     "http://127.0.0.1:8081",
		ReservedPrefix: "/__pageforge",
		MasterKey:      testMasterKey,
		CacheDir:       dir,
	}
	if err := m.Provision(caddy.Context{Context: context.Background()}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer func() { _ = m.Cleanup() }()
	if m.SnapshotCacheDir != "" {
		t.Fatalf("snapshot_cache_dir = %q, want empty even with cache_dir set: mirrors must not be written without an explicit path", m.SnapshotCacheDir)
	}
	if entries, err := os.ReadDir(dir); err != nil {
		t.Fatalf("read cache dir: %v", err)
	} else {
		for _, entry := range entries {
			if entry.Name() == ".snapshot" {
				t.Fatal("no snapshot mirror directory must be created under cache_dir")
			}
		}
	}

	explicit := S3Static{
		ControlURL:       "http://127.0.0.1:8081",
		ReservedPrefix:   "/__pageforge",
		MasterKey:        testMasterKey,
		CacheDir:         dir,
		SnapshotCacheDir: t.TempDir(),
	}
	if err := explicit.Provision(caddy.Context{Context: context.Background()}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer func() { _ = explicit.Cleanup() }()
	if explicit.SnapshotCacheDir == "" {
		t.Fatal("explicit snapshot_cache_dir must be kept")
	}
}

func TestProvisionLeavesSnapshotCacheDisabledWithoutCacheDir(t *testing.T) {
	m := S3Static{ControlURL: "http://127.0.0.1:8081", ReservedPrefix: "/__pageforge", MasterKey: testMasterKey}
	if err := m.Provision(caddy.Context{Context: context.Background()}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer func() { _ = m.Cleanup() }()
	if m.SnapshotCacheDir != "" {
		t.Fatalf("snapshot_cache_dir = %q, want empty", m.SnapshotCacheDir)
	}
}
