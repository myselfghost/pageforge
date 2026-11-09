// Package testutil provides shared helpers for repository and API tests.
package testutil

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // pgx driver for the optional Postgres test path

	"github.com/pageforge/pageforge/internal/crypto"
	"github.com/pageforge/pageforge/internal/httpx"
	"github.com/pageforge/pageforge/internal/store"
)

const masterKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

// PostgresDSNEnv points tests at a Postgres server. When set, every test gets
// its own temporary database instead of a SQLite file, so both dialects run
// against the same test suite. The DSN must be a postgres:// URL with a
// database the user may connect to (for example the default "postgres" one).
const PostgresDSNEnv = "PAGEFORGE_TEST_POSTGRES_DSN"

// NewDB opens a migrated database: SQLite in a temporary directory by default,
// or an isolated Postgres database when PAGEFORGE_TEST_POSTGRES_DSN is set.
func NewDB(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	if dsn := strings.TrimSpace(os.Getenv(PostgresDSNEnv)); dsn != "" {
		testDSN, cleanup := createPostgresDatabase(t, dsn)
		db, err := store.Open(ctx, store.Postgres, testDSN)
		if err != nil {
			cleanup()
			t.Fatalf("open postgres test database: %v", err)
		}
		t.Cleanup(func() {
			_ = db.Close()
			cleanup()
		})
		if _, err := db.Up(ctx); err != nil {
			t.Fatalf("migrate postgres test database: %v", err)
		}
		return db
	}
	db, err := store.Open(ctx, store.SQLite, filepath.Join(t.TempDir(), "pageforge.db"))
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Up(ctx); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	return db
}

// createPostgresDatabase creates an isolated database on the server addressed
// by dsn and returns the DSN targeting it plus a cleanup that drops it.
func createPostgresDatabase(t *testing.T, dsn string) (string, func()) {
	t.Helper()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres admin connection: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		_ = admin.Close()
		t.Fatalf("ping postgres server: %v", err)
	}

	name := "pageforge_test_" + randomSuffix(t)
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		_ = admin.Close()
		t.Fatalf("create postgres test database %s: %v", name, err)
	}
	testDSN, err := rewriteDSNDatabase(dsn, name)
	if err != nil {
		dropPostgresDatabase(admin, name)
		_ = admin.Close()
		t.Fatalf("rewrite postgres test dsn: %v", err)
	}
	cleanup := func() {
		dropPostgresDatabase(admin, name)
		_ = admin.Close()
	}
	return testDSN, cleanup
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("generate test database name: %v", err)
	}
	return hex.EncodeToString(buf)
}

func rewriteDSNDatabase(dsn, name string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "", fmt.Errorf("%s must be a postgres:// URL, got %q", PostgresDSNEnv, dsn)
	}
	u.Path = "/" + name
	return u.String(), nil
}

func dropPostgresDatabase(admin *sql.DB, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name); err != nil {
		// Cleanup failures must not mask the test result; the database name is
		// unique, so leftovers are harmless.
		fmt.Fprintf(os.Stderr, "drop test database %s: %v\n", name, err)
	}
}

// NewCipher returns a cipher built from the shared test master key.
func NewCipher(t *testing.T) *crypto.Cipher {
	t.Helper()
	c, err := crypto.NewFromBase64(masterKey)
	if err != nil {
		t.Fatalf("build test cipher: %v", err)
	}
	return c
}

// RequireCode asserts that err is an API error with the given code.
func RequireCode(t *testing.T, err error, code string) {
	t.Helper()
	var apiErr *httpx.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want API error with code %s", err, code)
	}
	if apiErr.Code != code {
		t.Fatalf("error code = %s, want %s (message: %s)", apiErr.Code, code, apiErr.Message)
	}
}
