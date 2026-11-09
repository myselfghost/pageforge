package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// Tx is a transaction with dialect-aware placeholder rewriting.
type Tx struct {
	*sql.Tx
	dialect Dialect
}

// Begin starts a transaction.
func (s *Store) Begin(ctx context.Context) (*Tx, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &Tx{Tx: tx, dialect: s.dialect}, nil
}

// QueryContext runs a query written with "?" placeholders.
func (s *Store) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.DB.QueryContext(ctx, rebind(query, s.dialect), args...)
}

// QueryRowContext runs a query written with "?" placeholders.
func (s *Store) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return s.DB.QueryRowContext(ctx, rebind(query, s.dialect), args...)
}

// ExecContext runs a statement written with "?" placeholders.
func (s *Store) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.DB.ExecContext(ctx, rebind(query, s.dialect), args...)
}

// WithTx runs fn inside a transaction, committing on success and rolling back on error.
func (s *Store) WithTx(ctx context.Context, fn func(*Tx) error) error {
	tx, err := s.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// QueryContext runs a query written with "?" placeholders.
func (t *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.Tx.QueryContext(ctx, rebind(query, t.dialect), args...)
}

// QueryRowContext runs a query written with "?" placeholders.
func (t *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.Tx.QueryRowContext(ctx, rebind(query, t.dialect), args...)
}

// ExecContext runs a statement written with "?" placeholders.
func (t *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.Tx.ExecContext(ctx, rebind(query, t.dialect), args...)
}

// rebind rewrites "?" placeholders to "$n" for Postgres. Queries in this
// repository must not contain "?" inside string literals.
func rebind(query string, dialect Dialect) string {
	if dialect != Postgres || !strings.Contains(query, "?") {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(query[i])
	}
	return b.String()
}

// IsUniqueViolation reports whether err is a unique-constraint failure.
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint failed") || strings.Contains(msg, "duplicate key")
}
