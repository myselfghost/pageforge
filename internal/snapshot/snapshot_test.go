package snapshot_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pageforge/pageforge/internal/snapshot"
	"github.com/pageforge/pageforge/internal/store"
	"github.com/pageforge/pageforge/internal/testutil"
)

const testBucket = "bucket-a"

func seedSite(t *testing.T, db *store.Store) {
	t.Helper()
	now := time.Now().UTC()
	mustExec(t, db, `INSERT INTO aliases (id, name, endpoint, region, access_key, secret_cipher,
		path_style, bucket_allowlist, tls_skip_verify, credential_ver, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"ali_1", "primary", "s3.example.com", "us-east-1", "AKIA", []byte{1, 2, 3}, true,
		`["bucket-a"]`, false, 9, now, now)
	mustExec(t, db, `INSERT INTO projects (id, name, description, alias_id, bucket, current_version_id,
		index_doc, error_doc, try_files_enabled, package_retention, spa_config, cache_policy,
		response_headers, status, created_at, updated_at)
		VALUES (?, ?, '', ?, ?, ?, ?, ?, ?, TRUE, ?, ?, ?, 'active', ?, ?)`,
		"prj_1", "site", "ali_1", testBucket, "ver_1", "index.html", "404.html", true,
		`{"enabled":true}`, `{"html":"no-cache","assets":"public, max-age=60"}`, `[{"name":"X-Site","value":"a"}]`, now, now)
	mustExec(t, db, `INSERT INTO bindings (id, host, path_prefix, project_id, pinned_version_id,
		auth_policy_id, response_headers, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, NULL, '', '[]', ?, ?, ?)`,
		"bnd_1", "example.com", "/site", "prj_1", true, now, now)
	mustExec(t, db, `INSERT INTO auth_policies (id, name, mode, config, secret_version, created_at, updated_at)
		VALUES (?, ?, ?, '{}', ?, ?, ?)`, "pol_1", "basic", "basic", 3, now, now)
}

func mustExec(t *testing.T, db *store.Store, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func TestGeneratorBumpsOnlyOnChange(t *testing.T) {
	db := testutil.NewDB(t)
	seedSite(t, db)
	gen := snapshot.NewGenerator(db, 0, nil)

	ctx := context.Background()
	first, body, err := gen.Current(ctx)
	if err != nil {
		t.Fatalf("generate snapshot: %v", err)
	}
	if first.Generation == 0 {
		t.Fatal("generation must be positive")
	}
	if _, ok := first.Aliases["ali_1"]; !ok {
		t.Fatal("alias missing from snapshot")
	}
	if first.Aliases["ali_1"].CredentialVer != 9 {
		t.Fatalf("credential version = %d, want 9", first.Aliases["ali_1"].CredentialVer)
	}
	project, ok := first.Projects["prj_1"]
	if !ok {
		t.Fatal("project missing from snapshot")
	}
	if !project.TryFilesEnabled || project.CurrentVersionID != "ver_1" || !project.SPA.Enabled {
		t.Fatalf("unexpected project view: %+v", project)
	}
	if len(project.ResponseHeaders) != 1 || project.ResponseHeaders[0].Name != "X-Site" {
		t.Fatalf("response headers not carried: %+v", project.ResponseHeaders)
	}
	if len(first.Bindings) != 1 || first.Bindings[0].Host != "example.com" {
		t.Fatalf("unexpected bindings: %+v", first.Bindings)
	}
	if _, ok := first.AuthPolicies["pol_1"]; !ok {
		t.Fatal("auth policy missing from snapshot")
	}
	if strings.Contains(string(body), "AKIA") || strings.Contains(string(body), "secret_cipher") {
		t.Fatal("snapshot must not contain credentials")
	}

	again, againBody, err := gen.Current(ctx)
	if err != nil {
		t.Fatalf("regenerate snapshot: %v", err)
	}
	if again.Generation != first.Generation {
		t.Fatalf("generation changed without content change: %d -> %d", first.Generation, again.Generation)
	}
	if string(againBody) != string(body) {
		t.Fatal("snapshot body changed without content change")
	}

	mustExec(t, db, `UPDATE bindings SET enabled = FALSE, updated_at = ? WHERE id = ?`, time.Now().UTC(), "bnd_1")
	changed, _, err := gen.Current(ctx)
	if err != nil {
		t.Fatalf("regenerate after change: %v", err)
	}
	if changed.Generation != first.Generation+1 {
		t.Fatalf("generation = %d, want %d", changed.Generation, first.Generation+1)
	}
	if changed.Bindings[0].Enabled {
		t.Fatal("disabled binding should be reflected in the snapshot")
	}

	reloaded, _, err := gen.Reload(ctx)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Generation != changed.Generation+1 {
		t.Fatalf("reload generation = %d, want %d", reloaded.Generation, changed.Generation+1)
	}
}

func TestValidateRejectsBadSnapshots(t *testing.T) {
	if err := (&snapshot.Snapshot{Generation: 0}).Validate(); err == nil {
		t.Fatal("generation 0 must be rejected")
	}
	bad := &snapshot.Snapshot{
		Generation: 1,
		Bindings: []snapshot.BindingView{
			{ID: "a", Host: "h", PathPrefix: "/x"},
			{ID: "a", Host: "h", PathPrefix: "/y"},
		},
	}
	if err := bad.Validate(); err == nil {
		t.Fatal("duplicate binding ids must be rejected")
	}
	bad = &snapshot.Snapshot{
		Generation: 1,
		Bindings:   []snapshot.BindingView{{ID: "a", Host: "h", PathPrefix: "x"}},
	}
	if err := bad.Validate(); err == nil {
		t.Fatal("relative path prefixes must be rejected")
	}
	valid := &snapshot.Snapshot{Generation: 1, Bindings: []snapshot.BindingView{{ID: "a", Host: "h", PathPrefix: "/"}}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid snapshot rejected: %v", err)
	}
}

func TestSnapshotJSONShape(t *testing.T) {
	snap := snapshot.Snapshot{Generation: 1, Bindings: []snapshot.BindingView{}}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{"generation", "published_at", "aliases", "projects", "bindings", "auth_policies"} {
		if !strings.Contains(string(raw), `"`+field+`"`) {
			t.Fatalf("snapshot JSON missing %q: %s", field, raw)
		}
	}
}
