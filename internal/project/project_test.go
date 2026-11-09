package project_test

import (
	"context"
	"testing"

	"github.com/pageforge/pageforge/internal/alias"
	"github.com/pageforge/pageforge/internal/binding"
	"github.com/pageforge/pageforge/internal/model"
	"github.com/pageforge/pageforge/internal/project"
	"github.com/pageforge/pageforge/internal/testutil"
)

func setup(t *testing.T) (*project.Repo, string) {
	t.Helper()
	db := testutil.NewDB(t)
	aliases := alias.NewRepo(db, testutil.NewCipher(t))
	created, err := aliases.Create(context.Background(), alias.CreateInput{
		Name: "minio", Endpoint: "https://minio.example.com", AccessKey: "ak", SecretKey: "sk",
		BucketAllowlist: []string{"static", "assets"},
	})
	if err != nil {
		t.Fatalf("create alias: %v", err)
	}
	return project.NewRepo(db), created.ID
}

func TestCreateProjectDefaults(t *testing.T) {
	repo, aliasID := setup(t)
	created, err := repo.Create(context.Background(), project.CreateInput{
		Name: "portal", AliasID: aliasID, Bucket: "static",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.IndexDoc != "index.html" {
		t.Fatalf("index_doc = %q, want index.html", created.IndexDoc)
	}
	if created.Status != model.ProjectActive {
		t.Fatalf("status = %q, want active", created.Status)
	}
	if !created.PackageRetention {
		t.Fatal("package_retention should default to true")
	}
	if created.Cache.HTML != "no-cache" || created.Cache.Assets != "public, max-age=300" {
		t.Fatalf("cache defaults = %+v", created.Cache)
	}
	if created.Cache.ImmutableMaxAge != 31536000 {
		t.Fatalf("immutable_max_age = %d", created.Cache.ImmutableMaxAge)
	}
	if created.ResponseHeaders == nil {
		t.Fatal("response_headers should be an empty slice, not nil")
	}
	if created.CurrentVersionID != "" {
		t.Fatalf("current_version_id = %q, want empty", created.CurrentVersionID)
	}
}

func TestProjectValidation(t *testing.T) {
	repo, aliasID := setup(t)
	ctx := context.Background()

	_, err := repo.Create(ctx, project.CreateInput{Name: "x", AliasID: "als_missing", Bucket: "static"})
	testutil.RequireCode(t, err, "ALIAS_NOT_FOUND")

	_, err = repo.Create(ctx, project.CreateInput{Name: "x", AliasID: aliasID, Bucket: "other"})
	testutil.RequireCode(t, err, "BUCKET_NOT_ALLOWED")

	_, err = repo.Create(ctx, project.CreateInput{Name: "x", AliasID: aliasID, Bucket: "static", IndexDoc: "../index.html"})
	testutil.RequireCode(t, err, "INVALID_PATH")

	_, err = repo.Create(ctx, project.CreateInput{Name: "x", AliasID: aliasID, Bucket: "static",
		ResponseHeaders: []model.HeaderRule{{Name: "Content-Length", Value: "1"}}})
	testutil.RequireCode(t, err, "INVALID_RESPONSE_HEADER")

	_, err = repo.Create(ctx, project.CreateInput{Name: "", AliasID: aliasID, Bucket: "static"})
	testutil.RequireCode(t, err, "INVALID_PROJECT")
}

func TestSPARequiresTryFiles(t *testing.T) {
	repo, aliasID := setup(t)
	ctx := context.Background()

	// SPA 开启但 try_files 关闭：创建被拒绝。
	_, err := repo.Create(ctx, project.CreateInput{
		Name: "spa-invalid", AliasID: aliasID, Bucket: "static",
		SPA: model.SPAConfig{Enabled: true},
	})
	testutil.RequireCode(t, err, "INVALID_PROJECT")

	// 依赖成立时可以创建。
	created, err := repo.Create(ctx, project.CreateInput{
		Name: "spa-valid", AliasID: aliasID, Bucket: "static",
		TryFilesEnabled: true, SPA: model.SPAConfig{Enabled: true},
	})
	if err != nil {
		t.Fatalf("create valid spa project: %v", err)
	}

	// 只关闭 try_files 会破坏依赖，被拒绝。
	disabled := false
	if _, err := repo.Update(ctx, created.ID, project.UpdateInput{TryFilesEnabled: &disabled}); err == nil {
		t.Fatal("closing try_files while spa enabled was accepted")
	} else {
		testutil.RequireCode(t, err, "INVALID_PROJECT")
	}

	// 单独开启 SPA（try_files 仍关闭）也被拒绝。
	plain, err := repo.Create(ctx, project.CreateInput{
		Name: "spa-plain", AliasID: aliasID, Bucket: "static",
	})
	if err != nil {
		t.Fatalf("create plain project: %v", err)
	}
	spaOn := model.SPAConfig{Enabled: true}
	if _, err := repo.Update(ctx, plain.ID, project.UpdateInput{SPA: &spaOn}); err == nil {
		t.Fatal("enabling spa while try_files disabled was accepted")
	} else {
		testutil.RequireCode(t, err, "INVALID_PROJECT")
	}

	// 同时关闭两项是合法组合。
	if _, err := repo.Update(ctx, created.ID, project.UpdateInput{
		TryFilesEnabled: &disabled, SPA: &model.SPAConfig{Enabled: false},
	}); err != nil {
		t.Fatalf("closing spa and try_files together: %v", err)
	}
}

func TestProjectUpdateAndDisable(t *testing.T) {
	repo, aliasID := setup(t)
	ctx := context.Background()
	created, err := repo.Create(ctx, project.CreateInput{Name: "portal", AliasID: aliasID, Bucket: "static"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	name := "portal-renamed"
	status := model.ProjectDisabled
	errorDoc := "404.html"
	tryFiles := true
	updated, err := repo.Update(ctx, created.ID, project.UpdateInput{
		Name: &name, Status: &status, ErrorDoc: &errorDoc, TryFilesEnabled: &tryFiles,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != name || updated.Status != status || updated.ErrorDoc != errorDoc || !updated.TryFilesEnabled {
		t.Fatalf("updated = %+v", updated)
	}
	if updated.ID != created.ID {
		t.Fatal("project id must not change")
	}

	badStatus := "archived"
	if _, err := repo.Update(ctx, created.ID, project.UpdateInput{Status: &badStatus}); err == nil {
		t.Fatal("invalid status accepted")
	} else {
		testutil.RequireCode(t, err, "INVALID_PROJECT")
	}

	badDoc := "docs/"
	if _, err := repo.Update(ctx, created.ID, project.UpdateInput{ErrorDoc: &badDoc}); err == nil {
		t.Fatal("directory error_doc accepted")
	}
}

func TestProjectNameRules(t *testing.T) {
	repo, aliasID := setup(t)
	ctx := context.Background()

	// 合法：字母、数字、-、_、.，字母或数字开头
	p, err := repo.Create(ctx, project.CreateInput{Name: "my-site_v1.2", AliasID: aliasID, Bucket: "static"})
	if err != nil {
		t.Fatalf("create with charset name: %v", err)
	}

	// 非法：空、空格、中文、斜杠、其他符号、符号开头
	for _, name := range []string{"", "my site", "门户", "a/b", "a!b", "-abc", ".abc", "_abc"} {
		if _, err := repo.Create(ctx, project.CreateInput{Name: name, AliasID: aliasID, Bucket: "static"}); err == nil {
			t.Fatalf("name %q accepted", name)
		} else {
			testutil.RequireCode(t, err, "INVALID_PROJECT")
		}
	}

	// 全局唯一：同名创建冲突
	if _, err := repo.Create(ctx, project.CreateInput{Name: "my-site_v1.2", AliasID: aliasID, Bucket: "static"}); err != nil {
		testutil.RequireCode(t, err, "PROJECT_NAME_TAKEN")
	} else {
		t.Fatal("duplicate name accepted")
	}

	// 改名为已占用名字也冲突
	p2, err := repo.Create(ctx, project.CreateInput{Name: "other", AliasID: aliasID, Bucket: "static"})
	if err != nil {
		t.Fatalf("create other: %v", err)
	}
	taken := "my-site_v1.2"
	if _, err := repo.Update(ctx, p2.ID, project.UpdateInput{Name: &taken}); err != nil {
		testutil.RequireCode(t, err, "PROJECT_NAME_TAKEN")
	} else {
		t.Fatal("rename to taken name accepted")
	}

	// 软删除后名字可复用
	if err := repo.Delete(ctx, p.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	reused, err := repo.Create(ctx, project.CreateInput{Name: "my-site_v1.2", AliasID: aliasID, Bucket: "static"})
	if err != nil {
		t.Fatalf("recreate after delete: %v", err)
	}

	// 按 name 查找，且找不到返回 PROJECT_NOT_FOUND
	found, err := repo.GetByName(ctx, "my-site_v1.2")
	if err != nil {
		t.Fatalf("get by name: %v", err)
	}
	if found.ID != reused.ID {
		t.Fatalf("GetByName id = %q, want %q", found.ID, reused.ID)
	}
	if _, err := repo.GetByName(ctx, "missing"); err != nil {
		testutil.RequireCode(t, err, "PROJECT_NOT_FOUND")
	} else {
		t.Fatal("GetByName accepted missing name")
	}
}

func TestDeleteProjectCascadesBindings(t *testing.T) {
	db := testutil.NewDB(t)
	aliases := alias.NewRepo(db, testutil.NewCipher(t))
	ctx := context.Background()
	createdAlias, err := aliases.Create(ctx, alias.CreateInput{
		Name: "minio", Endpoint: "https://minio.example.com", AccessKey: "ak", SecretKey: "sk",
	})
	if err != nil {
		t.Fatalf("create alias: %v", err)
	}
	projects := project.NewRepo(db)
	created, err := projects.Create(ctx, project.CreateInput{
		Name: "portal", AliasID: createdAlias.ID, Bucket: "static",
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	bindings := binding.NewRepo(db, "/__pageforge")
	createdBinding, err := bindings.Create(ctx, binding.CreateInput{
		PathPrefix: "/url1", ProjectID: created.ID,
	})
	if err != nil {
		t.Fatalf("create binding: %v", err)
	}

	if err := projects.Delete(ctx, created.ID); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	if _, err := projects.Get(ctx, created.ID); err == nil {
		t.Fatal("deleted project is still readable")
	} else {
		testutil.RequireCode(t, err, "PROJECT_NOT_FOUND")
	}
	if _, err := bindings.Get(ctx, createdBinding.ID); err == nil {
		t.Fatal("binding of a deleted project is still readable")
	} else {
		testutil.RequireCode(t, err, "BINDING_NOT_FOUND")
	}

	// The path prefix is free again after the cascade.
	if _, err := bindings.Create(ctx, binding.CreateInput{
		PathPrefix: "/url1", ProjectID: created.ID,
	}); err == nil {
		t.Fatal("binding to a deleted project was accepted")
	} else {
		testutil.RequireCode(t, err, "PROJECT_NOT_FOUND")
	}
}
