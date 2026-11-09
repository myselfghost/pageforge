package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestMigrateUpDownSQLite(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "pageforge.db")
	st, err := Open(ctx, SQLite, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	applied, err := st.Up(ctx)
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	want := []string{"0001_init", "0002_core", "0003_snapshot", "0004_share_links", "0005_project_name_unique", "0006_deploy_auto_publish", "0007_project_retention", "0008_version_objects_purged"}
	if len(applied) != len(want) {
		t.Fatalf("applied = %v, want %v", applied, want)
	}
	for i := range want {
		if applied[i] != want[i] {
			t.Fatalf("applied = %v, want %v", applied, want)
		}
	}

	for _, table := range []string{"aliases", "projects", "versions", "bindings", "deployments", "audit_logs", "snapshot_state", "share_links"} {
		var n int
		if err := st.DB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if n != 1 {
			t.Fatalf("table %s was not created", table)
		}
	}

	again, err := st.Up(ctx)
	if err != nil {
		t.Fatalf("up again: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second up should be a no-op, applied = %v", again)
	}

	reverted, err := st.Down(ctx, 8)
	if err != nil {
		t.Fatalf("down: %v", err)
	}
	if len(reverted) != 8 || reverted[0] != "0008_version_objects_purged" ||
		reverted[1] != "0007_project_retention" || reverted[2] != "0006_deploy_auto_publish" ||
		reverted[3] != "0005_project_name_unique" || reverted[4] != "0004_share_links" ||
		reverted[5] != "0003_snapshot" || reverted[6] != "0002_core" || reverted[7] != "0001_init" {
		t.Fatalf("reverted = %v", reverted)
	}

	var n int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("schema_migrations rows = %d, want 0", n)
	}
}

func TestTimestampsRoundTripSQLite(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, SQLite, filepath.Join(t.TempDir(), "pageforge.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if _, err := st.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}

	allowJSON, _ := json.Marshal([]string{"static"})
	_, err = st.ExecContext(ctx, `INSERT INTO aliases
		(id, name, endpoint, region, access_key, secret_cipher, path_style, bucket_allowlist,
		 tls_skip_verify, credential_ver, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		"als_1", "minio", "https://minio.example.com", "us-east-1", "ak", []byte("sealed"), true,
		string(allowJSON), false, 1)
	if err != nil {
		t.Fatalf("insert alias: %v", err)
	}

	var allowlist string
	var createdAt string
	if err := st.QueryRowContext(ctx,
		`SELECT bucket_allowlist, created_at FROM aliases WHERE id = ?`, "als_1").
		Scan(&allowlist, &createdAt); err != nil {
		t.Fatalf("select alias: %v", err)
	}
	if allowlist != `["static"]` {
		t.Fatalf("bucket_allowlist = %q", allowlist)
	}
	if createdAt == "" {
		t.Fatal("created_at was not stored")
	}
}

func TestRebind(t *testing.T) {
	query := `SELECT a, b FROM t WHERE a = ? AND b = ?`
	if got := rebind(query, SQLite); got != query {
		t.Fatalf("rebind(sqlite) = %q", got)
	}
	want := `SELECT a, b FROM t WHERE a = $1 AND b = $2`
	if got := rebind(query, Postgres); got != want {
		t.Fatalf("rebind(postgres) = %q, want %q", got, want)
	}
}
