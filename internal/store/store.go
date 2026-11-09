// Package store owns database connections and schema migrations.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // pgx driver
	_ "modernc.org/sqlite"             // pure-Go sqlite driver
)

type Dialect string

const (
	SQLite   Dialect = "sqlite"
	Postgres Dialect = "postgres"
)

// Store wraps *sql.DB with the dialect used for driver and migration selection.
type Store struct {
	DB      *sql.DB
	dialect Dialect
}

func driverFor(d Dialect) (string, error) {
	switch d {
	case SQLite:
		return "sqlite", nil
	case Postgres:
		return "pgx", nil
	default:
		return "", fmt.Errorf("unsupported dialect %q", d)
	}
}

// Open opens and pings a database connection.
func Open(ctx context.Context, dialect Dialect, dsn string) (*Store, error) {
	driver, err := driverFor(dialect)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dialect, err)
	}
	if dialect == SQLite {
		// A single connection avoids writer lock contention for the embedded deployment.
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(10)
		db.SetMaxIdleConns(5)
		db.SetConnMaxLifetime(time.Hour)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping %s: %w", dialect, err)
	}
	return &Store{DB: db, dialect: dialect}, nil
}

func (s *Store) Close() error {
	return s.DB.Close()
}

func (s *Store) Dialect() Dialect {
	return s.dialect
}

func (s *Store) placeholder(n int) string {
	if s.dialect == Postgres {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}
