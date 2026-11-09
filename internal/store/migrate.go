package store

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/pageforge/pageforge/migrations"
)

const createMigrationsTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version TEXT PRIMARY KEY,
	applied_at TEXT NOT NULL
)`

// Up applies all pending migrations in version order and returns their versions.
func (s *Store) Up(ctx context.Context) ([]string, error) {
	if _, err := s.DB.ExecContext(ctx, createMigrationsTable); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}

	files, err := fs.Glob(migrations.FS, string(s.dialect)+"/*.up.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(files)

	var applied []string
	for _, file := range files {
		version := strings.TrimSuffix(path.Base(file), ".up.sql")
		ok, err := s.isApplied(ctx, version)
		if err != nil {
			return applied, err
		}
		if ok {
			continue
		}
		if err := s.applyFile(ctx, file, version, true); err != nil {
			return applied, fmt.Errorf("migrate up %s: %w", version, err)
		}
		applied = append(applied, version)
	}
	return applied, nil
}

// Down reverts the most recent steps migrations. steps <= 0 reverts all.
func (s *Store) Down(ctx context.Context, steps int) ([]string, error) {
	if _, err := s.DB.ExecContext(ctx, createMigrationsTable); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}
	versions, err := s.appliedVersions(ctx)
	if err != nil {
		return nil, err
	}
	if steps <= 0 || steps > len(versions) {
		steps = len(versions)
	}

	var reverted []string
	for i := 0; i < steps; i++ {
		version := versions[len(versions)-1-i]
		file := string(s.dialect) + "/" + version + ".down.sql"
		if err := s.applyFile(ctx, file, version, false); err != nil {
			return reverted, fmt.Errorf("migrate down %s: %w", version, err)
		}
		reverted = append(reverted, version)
	}
	return reverted, nil
}

func (s *Store) applyFile(ctx context.Context, file, version string, up bool) error {
	body, err := migrations.FS.ReadFile(file)
	if err != nil {
		return fmt.Errorf("read %s: %w", file, err)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, string(body)); err != nil {
		return err
	}
	if up {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, applied_at) VALUES (`+s.placeholder(1)+`, `+s.placeholder(2)+`)`,
			version, time.Now().UTC().Format(time.RFC3339Nano))
	} else {
		_, err = tx.ExecContext(ctx,
			`DELETE FROM schema_migrations WHERE version = `+s.placeholder(1), version)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) isApplied(ctx context.Context, version string) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = `+s.placeholder(1), version).Scan(&n)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	return n > 0, nil
}

func (s *Store) appliedVersions(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT version FROM schema_migrations ORDER BY version ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var versions []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		versions = append(versions, v)
	}
	return versions, rows.Err()
}
