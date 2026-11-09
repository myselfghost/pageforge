// Package config loads the control-plane configuration from YAML and environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultListen         = "127.0.0.1:8080"
	DefaultInternalListen = "127.0.0.1:8081"
	DefaultReservedPrefix = "/__pageforge"
	DefaultDBDriver       = "sqlite"
	DefaultDBDSN          = "pageforge.db"
	DefaultLogLevel       = "info"
)

// Config is the control-plane configuration.
type Config struct {
	Server     Server     `yaml:"server"`
	Database   Database   `yaml:"database"`
	Encryption Encryption `yaml:"encryption"`
	Storage    Storage    `yaml:"storage"`
	Publish    Publish    `yaml:"publish"`
	GC         GC         `yaml:"gc"`
	Log        Log        `yaml:"log"`
}

// Log controls where the control plane writes its structured logs.
type Log struct {
	// File is the main log output path (everything except HTTP request logs).
	// When empty, logs go to stdout only.
	File string `yaml:"file"`
	// AccessFile is the HTTP request log output path. When empty, request logs
	// go to stdout only.
	AccessFile string `yaml:"access_file"`
	// Level is the minimum level written: debug, info, warn or error.
	Level string `yaml:"level"`
	// MaxAgeDays is how many daily rotated logs to keep. Zero keeps forever.
	MaxAgeDays int `yaml:"max_age_days"`
}

// Storage controls where manifests and packages are written.
type Storage struct {
	ManifestPrefix string `yaml:"manifest_prefix"`
	PackagePrefix  string `yaml:"package_prefix"`
	StagingPrefix  string `yaml:"staging_prefix"`
}

// Publish controls archive limits, staging and manifest sharding.
type Publish struct {
	MaxFiles        int    `yaml:"max_files"`
	MaxFileBytes    int64  `yaml:"max_file_bytes"`
	MaxTotalBytes   int64  `yaml:"max_total_bytes"`
	MaxPackageBytes int64  `yaml:"max_package_bytes"`
	IdempotencyTTL  string `yaml:"idempotency_ttl"`
	Workers         int    `yaml:"workers"`
	TempDir         string `yaml:"temp_dir"`
	ShardFiles      int    `yaml:"shard_files"`
	ShardBytes      int    `yaml:"shard_bytes"`
}

// GC controls background retention.
type GC struct {
	Enabled      bool   `yaml:"enabled"`
	Interval     string `yaml:"interval"`
	OrphanGrace  string `yaml:"orphan_grace"`
	KeepVersions int    `yaml:"keep_versions"`
	KeepDays     int    `yaml:"keep_days"`
}

type Server struct {
	Listen            string `yaml:"listen"`
	InternalListen    string `yaml:"internal_listen"`
	AdminToken        string `yaml:"admin_token"`
	InternalToken     string `yaml:"internal_token"`
	ReservedPrefix    string `yaml:"reserved_prefix"`
	SnapshotWarnBytes int    `yaml:"snapshot_warn_bytes"`
}

type Database struct {
	Driver string `yaml:"driver"`
	DSN    string `yaml:"dsn"`
}

type Encryption struct {
	// MasterKey is a base64-encoded 32-byte AES key. PAGEFORGE_MASTER_KEY overrides it.
	MasterKey string `yaml:"master_key"`
}

// Default returns the documented defaults.
func Default() Config {
	return Config{
		Server: Server{
			Listen:            DefaultListen,
			InternalListen:    DefaultInternalListen,
			ReservedPrefix:    DefaultReservedPrefix,
			SnapshotWarnBytes: 32 << 20,
		},
		Database: Database{
			Driver: DefaultDBDriver,
			DSN:    DefaultDBDSN,
		},
		Storage: Storage{
			ManifestPrefix: "_system/manifests",
			PackagePrefix:  "_system/packages",
			StagingPrefix:  "_system/staging",
		},
		Publish: Publish{
			MaxFiles:        200000,
			MaxFileBytes:    2 << 30,
			MaxTotalBytes:   50 << 30,
			MaxPackageBytes: 4 << 30,
			IdempotencyTTL:  "24h",
			Workers:         32,
			ShardFiles:      20000,
			ShardBytes:      2 << 20,
		},
		GC: GC{
			Enabled:      true,
			Interval:     "1h",
			OrphanGrace:  "24h",
			KeepVersions: 20,
		},
		Log: Log{
			File:       "./logs/control.log",
			AccessFile: "./logs/control-access.log",
			Level:      DefaultLogLevel,
			MaxAgeDays: 30,
		},
	}
}

// Load reads path when non-empty, then applies PAGEFORGE_* overrides and validates.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("read config %q: %w", path, err)
		}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse config %q: %w", path, err)
		}
	}
	cfg.applyEnv()
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) applyEnv() {
	if v, ok := os.LookupEnv("PAGEFORGE_CONTROL_LISTEN"); ok {
		c.Server.Listen = v
	}
	if v, ok := os.LookupEnv("PAGEFORGE_INTERNAL_LISTEN"); ok {
		c.Server.InternalListen = v
	}
	if v, ok := os.LookupEnv("PAGEFORGE_ADMIN_TOKEN"); ok {
		c.Server.AdminToken = v
	}
	if v, ok := os.LookupEnv("PAGEFORGE_INTERNAL_TOKEN"); ok {
		c.Server.InternalToken = v
	}
	if v, ok := os.LookupEnv("PAGEFORGE_RESERVED_PREFIX"); ok {
		c.Server.ReservedPrefix = v
	}
	if v, ok := os.LookupEnv("PAGEFORGE_DATABASE_DRIVER"); ok {
		c.Database.Driver = v
	}
	if v, ok := os.LookupEnv("PAGEFORGE_DATABASE_DSN"); ok {
		c.Database.DSN = v
	}
	if v, ok := os.LookupEnv("PAGEFORGE_MASTER_KEY"); ok {
		c.Encryption.MasterKey = v
	}
	if v, ok := os.LookupEnv("PAGEFORGE_PUBLISH_TEMP_DIR"); ok {
		c.Publish.TempDir = v
	}
	if v, ok := os.LookupEnv("PAGEFORGE_LOG_FILE"); ok {
		c.Log.File = v
	}
	if v, ok := os.LookupEnv("PAGEFORGE_LOG_ACCESS_FILE"); ok {
		c.Log.AccessFile = v
	}
	if v, ok := os.LookupEnv("PAGEFORGE_LOG_LEVEL"); ok {
		c.Log.Level = v
	}
	if v, ok := os.LookupEnv("PAGEFORGE_LOG_MAX_AGE_DAYS"); ok {
		if d, err := strconv.Atoi(v); err == nil {
			c.Log.MaxAgeDays = d
		}
	}
	if strings.TrimSpace(c.Log.Level) == "" {
		c.Log.Level = DefaultLogLevel
	}
	c.Encryption.MasterKey = expandEnvRef(c.Encryption.MasterKey)
}

// expandEnvRef resolves a single "${VAR}" reference, matching the documented YAML style.
func expandEnvRef(value string) string {
	if strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") {
		return os.Getenv(value[2 : len(value)-1])
	}
	return value
}

// Validate checks the fields the control plane cannot run without.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Server.Listen) == "" {
		return fmt.Errorf("server.listen must not be empty")
	}
	if strings.TrimSpace(c.Server.InternalListen) == "" {
		return fmt.Errorf("server.internal_listen must not be empty")
	}
	if c.Server.Listen == c.Server.InternalListen {
		return fmt.Errorf("server.listen and server.internal_listen must differ")
	}
	prefix := c.Server.ReservedPrefix
	if !strings.HasPrefix(prefix, "/") {
		return fmt.Errorf("server.reserved_prefix %q must start with /", prefix)
	}
	if len(prefix) < 2 || strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("server.reserved_prefix %q must be a non-root path without trailing slash", prefix)
	}
	switch c.Database.Driver {
	case "sqlite", "postgres":
	default:
		return fmt.Errorf("database.driver %q must be sqlite or postgres", c.Database.Driver)
	}
	if strings.TrimSpace(c.Database.DSN) == "" {
		return fmt.Errorf("database.dsn must not be empty")
	}
	if c.Encryption.MasterKey == "" {
		return fmt.Errorf("encryption.master_key (or PAGEFORGE_MASTER_KEY) must be set")
	}
	if strings.TrimSpace(c.Storage.ManifestPrefix) == "" {
		return fmt.Errorf("storage.manifest_prefix must not be empty")
	}
	if strings.TrimSpace(c.Storage.PackagePrefix) == "" {
		return fmt.Errorf("storage.package_prefix must not be empty")
	}
	if c.Publish.MaxFiles <= 0 || c.Publish.MaxFileBytes <= 0 ||
		c.Publish.MaxTotalBytes <= 0 || c.Publish.MaxPackageBytes <= 0 {
		return fmt.Errorf("publish limits must be positive")
	}
	if c.Publish.Workers <= 0 {
		return fmt.Errorf("publish.workers must be positive")
	}
	if c.Publish.ShardFiles <= 0 || c.Publish.ShardBytes <= 0 {
		return fmt.Errorf("publish shard thresholds must be positive")
	}
	if _, err := c.Publish.IdempotencyWindow(); err != nil {
		return err
	}
	if _, err := c.GC.IntervalDuration(); err != nil {
		return err
	}
	if _, err := c.GC.OrphanGraceDuration(); err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(c.Log.Level)) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level %q must be debug, info, warn or error", c.Log.Level)
	}
	return nil
}

// IdempotencyWindow parses publish.idempotency_ttl.
func (p Publish) IdempotencyWindow() (time.Duration, error) {
	return parseDuration("publish.idempotency_ttl", p.IdempotencyTTL)
}

// IntervalDuration parses gc.interval.
func (g GC) IntervalDuration() (time.Duration, error) {
	return parseDuration("gc.interval", g.Interval)
}

// OrphanGraceDuration parses gc.orphan_grace.
func (g GC) OrphanGraceDuration() (time.Duration, error) {
	return parseDuration("gc.orphan_grace", g.OrphanGrace)
}

func parseDuration(name, value string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a valid duration: %w", name, value, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive", name)
	}
	return d, nil
}
