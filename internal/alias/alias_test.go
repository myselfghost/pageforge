package alias_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/pageforge/pageforge/internal/alias"
	"github.com/pageforge/pageforge/internal/testutil"
)

func boolPtr(v bool) *bool    { return &v }
func strPtr(v string) *string { return &v }

func newRepo(t *testing.T) (*alias.Repo, *alias.CreateInput) {
	t.Helper()
	repo := alias.NewRepo(testutil.NewDB(t), testutil.NewCipher(t))
	in := &alias.CreateInput{
		Name:            "minio-prod",
		Endpoint:        "https://minio.example.com/",
		AccessKey:       "ak",
		SecretKey:       "sk",
		BucketAllowlist: []string{"static", "static", "assets"},
	}
	return repo, in
}

func TestCreateAliasDefaultsAndSecret(t *testing.T) {
	repo, in := newRepo(t)
	ctx := context.Background()

	created, err := repo.Create(ctx, *in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(created.ID, "als_") {
		t.Fatalf("id = %q, want als_ prefix", created.ID)
	}
	if created.Region != "us-east-1" {
		t.Fatalf("region = %q, want default us-east-1", created.Region)
	}
	if created.Endpoint != "https://minio.example.com" {
		t.Fatalf("endpoint = %q, want trailing slash removed", created.Endpoint)
	}
	if !created.PathStyle {
		t.Fatal("path_style should default to true")
	}
	if len(created.BucketAllowlist) != 2 {
		t.Fatalf("allowlist = %v, want deduplicated entries", created.BucketAllowlist)
	}
	if bytes.Contains(created.SecretCipher, []byte("sk")) {
		t.Fatal("secret ciphertext contains the plaintext secret")
	}

	view := created.View()
	if !view.SecretKeySet {
		t.Fatal("view.SecretKeySet = false, want true")
	}
	if view.CredentialVersion != 1 {
		t.Fatalf("credential version = %d, want 1", view.CredentialVersion)
	}

	accessKey, secret, err := repo.Secret(ctx, created.ID)
	if err != nil {
		t.Fatalf("secret: %v", err)
	}
	if accessKey != "ak" || secret != "sk" {
		t.Fatalf("secret = (%q, %q), want (ak, sk)", accessKey, secret)
	}
}

func TestAliasValidation(t *testing.T) {
	repo, base := newRepo(t)
	ctx := context.Background()

	cases := map[string]alias.CreateInput{
		"empty name":        {Endpoint: "https://minio.example.com", AccessKey: "ak", SecretKey: "sk"},
		"bad scheme":        {Name: "x", Endpoint: "ftp://minio.example.com", AccessKey: "ak", SecretKey: "sk"},
		"relative endpoint": {Name: "x", Endpoint: "minio.example.com", AccessKey: "ak", SecretKey: "sk"},
		"missing secret":    {Name: "x", Endpoint: "https://minio.example.com", AccessKey: "ak"},
		"bad allowlist":     {Name: "x", Endpoint: "https://minio.example.com", AccessKey: "ak", SecretKey: "sk", BucketAllowlist: []string{"a/b"}},
	}
	for name, in := range cases {
		if _, err := repo.Create(ctx, in); err == nil {
			t.Fatalf("%s: Create() = nil error, want error", name)
		} else {
			testutil.RequireCode(t, err, "INVALID_ALIAS")
		}
	}

	if _, err := repo.Create(ctx, *base); err != nil {
		t.Fatalf("create baseline: %v", err)
	}
	dup := *base
	if _, err := repo.Create(ctx, dup); err == nil {
		t.Fatal("duplicate name accepted")
	} else {
		testutil.RequireCode(t, err, "ALIAS_NAME_TAKEN")
	}
}

func TestAliasUpdateBumpsCredentialVersion(t *testing.T) {
	repo, in := newRepo(t)
	ctx := context.Background()
	created, err := repo.Create(ctx, *in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	renamed, err := repo.Update(ctx, created.ID, alias.UpdateInput{Name: strPtr("renamed")})
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if renamed.Name != "renamed" || renamed.CredentialVer != 1 {
		t.Fatalf("updated = (%q, %d), want (renamed, 1)", renamed.Name, renamed.CredentialVer)
	}

	rotated, err := repo.Update(ctx, created.ID, alias.UpdateInput{SecretKey: strPtr("sk2")})
	if err != nil {
		t.Fatalf("rotate secret: %v", err)
	}
	if rotated.CredentialVer != 2 {
		t.Fatalf("credential version = %d, want 2", rotated.CredentialVer)
	}
	if _, secret, _ := repo.Secret(ctx, created.ID); secret != "sk2" {
		t.Fatalf("secret = %q, want sk2", secret)
	}

	if _, err := repo.Update(ctx, created.ID, alias.UpdateInput{PathStyle: boolPtr(false)}); err != nil {
		t.Fatalf("toggle path style: %v", err)
	}
	if got, _ := repo.Get(ctx, created.ID); got.PathStyle {
		t.Fatal("path_style was not updated")
	}
}

func TestAliasDeleteRequiresNoProjects(t *testing.T) {
	db := testutil.NewDB(t)
	repo := alias.NewRepo(db, testutil.NewCipher(t))
	ctx := context.Background()
	created, err := repo.Create(ctx, alias.CreateInput{
		Name: "minio", Endpoint: "https://minio.example.com", AccessKey: "ak", SecretKey: "sk",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := db.ExecContext(ctx, `INSERT INTO projects
		(id, name, alias_id, bucket, created_at, updated_at)
		VALUES ('prj_1', 'portal', ?, 'static', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, created.ID); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if err := repo.Delete(ctx, created.ID); err == nil {
		t.Fatal("delete with live project succeeded")
	} else {
		testutil.RequireCode(t, err, "ALIAS_IN_USE")
	}

	if _, err := db.ExecContext(ctx, `UPDATE projects SET deleted_at = CURRENT_TIMESTAMP WHERE id = 'prj_1'`); err != nil {
		t.Fatalf("soft delete project: %v", err)
	}
	if err := repo.Delete(ctx, created.ID); err != nil {
		t.Fatalf("delete after project removal: %v", err)
	}
	if _, err := repo.Get(ctx, created.ID); err == nil {
		t.Fatal("deleted alias is still readable")
	} else {
		testutil.RequireCode(t, err, "ALIAS_NOT_FOUND")
	}
}

func TestAliasList(t *testing.T) {
	repo, base := newRepo(t)
	ctx := context.Background()
	for _, name := range []string{"b-alias", "a-alias"} {
		in := *base
		in.Name = name
		if _, err := repo.Create(ctx, in); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	items, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 2 || items[0].Name != "a-alias" || items[1].Name != "b-alias" {
		t.Fatalf("list order = %v", []string{items[0].Name, items[1].Name})
	}
}
