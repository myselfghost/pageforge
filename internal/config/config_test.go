package config

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"
)

const testMasterKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

func validConfig() Config {
	cfg := Default()
	cfg.Encryption.MasterKey = testMasterKey
	return cfg
}

func TestDefault(t *testing.T) {
	cfg := Default()
	if cfg.Server.Listen != DefaultListen {
		t.Fatalf("listen = %q, want %q", cfg.Server.Listen, DefaultListen)
	}
	if cfg.Server.InternalListen != DefaultInternalListen {
		t.Fatalf("internal listen = %q, want %q", cfg.Server.InternalListen, DefaultInternalListen)
	}
	if cfg.Server.ReservedPrefix != DefaultReservedPrefix {
		t.Fatalf("reserved prefix = %q, want %q", cfg.Server.ReservedPrefix, DefaultReservedPrefix)
	}
}

func TestLoadEnvOverride(t *testing.T) {
	t.Setenv("PAGEFORGE_CONTROL_LISTEN", "127.0.0.1:9080")
	t.Setenv("PAGEFORGE_INTERNAL_LISTEN", "127.0.0.1:9081")
	t.Setenv("PAGEFORGE_RESERVED_PREFIX", "/_pf")
	t.Setenv("PAGEFORGE_MASTER_KEY", testMasterKey)
	t.Setenv("PAGEFORGE_DATABASE_DSN", "/tmp/test.db")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Listen != "127.0.0.1:9080" {
		t.Fatalf("listen = %q", cfg.Server.Listen)
	}
	if cfg.Server.InternalListen != "127.0.0.1:9081" {
		t.Fatalf("internal listen = %q", cfg.Server.InternalListen)
	}
	if cfg.Server.ReservedPrefix != "/_pf" {
		t.Fatalf("reserved prefix = %q", cfg.Server.ReservedPrefix)
	}
	if cfg.Database.DSN != "/tmp/test.db" {
		t.Fatalf("dsn = %q", cfg.Database.DSN)
	}
}

func TestLoadFile(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	content := []byte("server:\n  listen: 127.0.0.1:7080\n  internal_listen: 127.0.0.1:7081\n  reserved_prefix: /_sys\ndatabase:\n  driver: sqlite\n  dsn: /tmp/control.db\nencryption:\n  master_key: ${PAGEFORGE_TEST_KEY}\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("PAGEFORGE_TEST_KEY", testMasterKey)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Listen != "127.0.0.1:7080" || cfg.Server.ReservedPrefix != "/_sys" {
		t.Fatalf("unexpected config: %+v", cfg.Server)
	}
	if cfg.Database.DSN != "/tmp/control.db" {
		t.Fatalf("dsn = %q", cfg.Database.DSN)
	}
	if cfg.Encryption.MasterKey != testMasterKey {
		t.Fatalf("master key = %q, want env reference to resolve", cfg.Encryption.MasterKey)
	}
}

func TestValidateRejectsInvalidPrefix(t *testing.T) {
	cases := []string{"__pageforge", "/", "/_pf/", ""}
	for _, prefix := range cases {
		cfg := validConfig()
		cfg.Server.ReservedPrefix = prefix
		if err := cfg.Validate(); err == nil {
			t.Fatalf("prefix %q should be rejected", prefix)
		}
	}
}

func TestValidateRejectsSameListeners(t *testing.T) {
	cfg := validConfig()
	cfg.Server.InternalListen = cfg.Server.Listen
	if err := cfg.Validate(); err == nil {
		t.Fatal("identical listeners should be rejected")
	}
}

func TestLogLevelDefaultAndOverride(t *testing.T) {
	if cfg := Default(); cfg.Log.Level != DefaultLogLevel {
		t.Fatalf("default log level = %q, want %q", cfg.Log.Level, DefaultLogLevel)
	}

	t.Setenv("PAGEFORGE_MASTER_KEY", testMasterKey)
	t.Setenv("PAGEFORGE_LOG_LEVEL", "debug")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Log.Level != "debug" {
		t.Fatalf("log level = %q, want env override debug", cfg.Log.Level)
	}
}

func TestValidateRejectsUnknownLogLevel(t *testing.T) {
	cfg := validConfig()
	cfg.Log.Level = "verbose"
	if err := cfg.Validate(); err == nil {
		t.Fatal("unknown log level should be rejected")
	}
	cfg.Log.Level = "WARN"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("uppercase log level should be accepted: %v", err)
	}
}

func TestLogAccessFileDefaultAndOverride(t *testing.T) {
	cfg := Default()
	if cfg.Log.AccessFile != "./logs/control-access.log" {
		t.Fatalf("default access_file = %q", cfg.Log.AccessFile)
	}

	t.Setenv("PAGEFORGE_MASTER_KEY", testMasterKey)
	t.Setenv("PAGEFORGE_LOG_ACCESS_FILE", "./logs/req.log")
	loaded, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Log.AccessFile != "./logs/req.log" {
		t.Fatalf("access_file = %q, want env override", loaded.Log.AccessFile)
	}
}

func TestValidateRequiresMasterKeyAndValidDriver(t *testing.T) {
	cfg := validConfig()
	cfg.Encryption.MasterKey = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("missing master key should be rejected")
	}

	cfg = validConfig()
	cfg.Database.Driver = "mysql"
	if err := cfg.Validate(); err == nil {
		t.Fatal("unsupported driver should be rejected")
	}

	cfg = validConfig()
	cfg.Database.DSN = " "
	if err := cfg.Validate(); err == nil {
		t.Fatal("empty dsn should be rejected")
	}
}

func TestTestMasterKeyIsValidBase64(t *testing.T) {
	key, err := base64.StdEncoding.DecodeString(testMasterKey)
	if err != nil {
		t.Fatalf("test master key is not base64: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("test master key is %d bytes, want 32", len(key))
	}
	if strings.TrimSpace(testMasterKey) != testMasterKey {
		t.Fatal("test master key contains whitespace")
	}
}
