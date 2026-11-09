package version_test

import (
	"context"
	"testing"

	"github.com/pageforge/pageforge/internal/alias"
	"github.com/pageforge/pageforge/internal/binding"
	"github.com/pageforge/pageforge/internal/project"
	"github.com/pageforge/pageforge/internal/store"
	"github.com/pageforge/pageforge/internal/testutil"
	"github.com/pageforge/pageforge/internal/version"
)

func setup(t *testing.T) (*version.Repo, *project.Repo, string, string) {
	versions, projects, _, projectID, otherID := setupDB(t)
	return versions, projects, projectID, otherID
}

func setupDB(t *testing.T) (*version.Repo, *project.Repo, *store.Store, string, string) {
	t.Helper()
	db := testutil.NewDB(t)
	ctx := context.Background()
	aliases := alias.NewRepo(db, testutil.NewCipher(t))
	createdAlias, err := aliases.Create(ctx, alias.CreateInput{
		Name: "minio", Endpoint: "https://minio.example.com", AccessKey: "ak", SecretKey: "sk",
	})
	if err != nil {
		t.Fatalf("create alias: %v", err)
	}
	projects := project.NewRepo(db)
	createdProject, err := projects.Create(ctx, project.CreateInput{
		Name: "portal", AliasID: createdAlias.ID, Bucket: "static",
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	otherProject, err := projects.Create(ctx, project.CreateInput{
		Name: "other", AliasID: createdAlias.ID, Bucket: "static",
	})
	if err != nil {
		t.Fatalf("create other project: %v", err)
	}
	return version.NewRepo(db), projects, db, createdProject.ID, otherProject.ID
}

func TestCreateVersionStates(t *testing.T) {
	versions, _, projectID, _ := setup(t)
	ctx := context.Background()

	created, err := versions.Create(ctx, projectID, "dep_1", "sha-1", "_system/manifests/prj/ver")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Status != version.StatusUploading {
		t.Fatalf("status = %q, want uploading", created.Status)
	}
	if created.Label != "sha-1" || created.DeploymentID != "dep_1" {
		t.Fatalf("version = %+v", created)
	}

	if _, err := versions.Create(ctx, projectID, "dep_2", "sha-1", ""); err == nil {
		t.Fatal("duplicate label accepted")
	} else {
		testutil.RequireCode(t, err, "VERSION_LABEL_TAKEN")
	}

	for i := 0; i < 2; i++ {
		if _, err := versions.Create(ctx, projectID, "", "", ""); err != nil {
			t.Fatalf("empty label should be allowed multiple times: %v", err)
		}
	}

	if _, err := versions.Create(ctx, "prj_missing", "", "", ""); err == nil {
		t.Fatal("version for unknown project accepted")
	} else {
		testutil.RequireCode(t, err, "PROJECT_NOT_FOUND")
	}
}

func TestDeleteVersion(t *testing.T) {
	versions, _, db, projectID, _ := setupDB(t)
	ctx := context.Background()

	// 上传中版本不可删除
	uploading, err := versions.Create(ctx, projectID, "dep_u", "", "_system/manifests/p/u")
	if err != nil {
		t.Fatalf("create uploading: %v", err)
	}
	if err := versions.Delete(ctx, projectID, uploading.ID); err != nil {
		testutil.RequireCode(t, err, "VERSION_NOT_DELETABLE")
	} else {
		t.Fatal("uploading version deleted")
	}

	// 失败版本可删除
	if err := versions.MarkFailed(ctx, projectID, uploading.ID, "boom"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	if err := versions.Delete(ctx, projectID, uploading.ID); err != nil {
		t.Fatalf("delete failed version: %v", err)
	}
	if _, err := versions.Get(ctx, projectID, uploading.ID); err != nil {
		testutil.RequireCode(t, err, "VERSION_NOT_FOUND")
	} else {
		t.Fatal("deleted version still visible")
	}

	// ready 且非当前版本可删除；删除后不再出现在列表
	v1, err := versions.Create(ctx, projectID, "dep_v1", "", "_system/manifests/p/v1")
	if err != nil {
		t.Fatalf("create v1: %v", err)
	}
	if _, err := versions.MarkReady(ctx, projectID, v1.ID, "sha", 1, 1); err != nil {
		t.Fatalf("mark ready v1: %v", err)
	}
	if err := versions.Delete(ctx, projectID, v1.ID); err != nil {
		t.Fatalf("delete ready version: %v", err)
	}
	list, err := versions.List(ctx, projectID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, v := range list {
		if v.ID == v1.ID {
			t.Fatal("deleted version still listed")
		}
	}

	// 当前指针指向的版本不可删除
	v2, err := versions.Create(ctx, projectID, "dep_v2", "", "_system/manifests/p/v2")
	if err != nil {
		t.Fatalf("create v2: %v", err)
	}
	if _, err := versions.MarkReady(ctx, projectID, v2.ID, "sha", 1, 1); err != nil {
		t.Fatalf("mark ready v2: %v", err)
	}
	if _, err := versions.SetPointer(ctx, projectID, v2.ID, ""); err != nil {
		t.Fatalf("set pointer: %v", err)
	}
	if err := versions.Delete(ctx, projectID, v2.ID); err != nil {
		testutil.RequireCode(t, err, "VERSION_IN_USE")
	} else {
		t.Fatal("current version deleted")
	}

	// 受保护版本不可删除，取消保护后可删除
	v3, err := versions.Create(ctx, projectID, "dep_v3", "", "_system/manifests/p/v3")
	if err != nil {
		t.Fatalf("create v3: %v", err)
	}
	if _, err := versions.MarkReady(ctx, projectID, v3.ID, "sha", 1, 1); err != nil {
		t.Fatalf("mark ready v3: %v", err)
	}
	if _, err := versions.SetProtected(ctx, projectID, v3.ID, true); err != nil {
		t.Fatalf("protect: %v", err)
	}
	if err := versions.Delete(ctx, projectID, v3.ID); err != nil {
		testutil.RequireCode(t, err, "VERSION_PROTECTED")
	} else {
		t.Fatal("protected version deleted")
	}
	if _, err := versions.SetProtected(ctx, projectID, v3.ID, false); err != nil {
		t.Fatalf("unprotect: %v", err)
	}
	if err := versions.Delete(ctx, projectID, v3.ID); err != nil {
		t.Fatalf("delete after unprotect: %v", err)
	}

	// 被 binding 固定的版本不可删除
	v4, err := versions.Create(ctx, projectID, "dep_v4", "", "_system/manifests/p/v4")
	if err != nil {
		t.Fatalf("create v4: %v", err)
	}
	if _, err := versions.MarkReady(ctx, projectID, v4.ID, "sha", 1, 1); err != nil {
		t.Fatalf("mark ready v4: %v", err)
	}
	bindings := binding.NewRepo(db, "/__pageforge")
	b, err := bindings.Create(ctx, binding.CreateInput{PathPrefix: "/pinned", ProjectID: projectID, PinnedVersionID: v4.ID})
	if err != nil {
		t.Fatalf("create binding: %v", err)
	}
	if err := versions.Delete(ctx, projectID, v4.ID); err != nil {
		testutil.RequireCode(t, err, "VERSION_IN_USE")
	} else {
		t.Fatal("pinned version deleted")
	}
	if err := bindings.Delete(ctx, b.ID); err != nil {
		t.Fatalf("delete binding: %v", err)
	}
	if err := versions.Delete(ctx, projectID, v4.ID); err != nil {
		t.Fatalf("delete after unpin: %v", err)
	}
}

func TestMarkReadyAndFailed(t *testing.T) {
	versions, _, projectID, _ := setup(t)
	ctx := context.Background()

	ready, err := versions.Create(ctx, projectID, "", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	updated, err := versions.MarkReady(ctx, projectID, ready.ID, "abc", 3, 1024)
	if err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	if updated.Status != version.StatusReady || updated.ManifestSHA256 != "abc" ||
		updated.FileCount != 3 || updated.TotalBytes != 1024 {
		t.Fatalf("ready version = %+v", updated)
	}
	if updated.ReadyAt == nil || updated.PublishedAt == nil {
		t.Fatal("ready_at/published_at were not set")
	}
	if _, err := versions.MarkReady(ctx, projectID, ready.ID, "x", 0, 0); err == nil {
		t.Fatal("marking a ready version ready again succeeded")
	} else {
		testutil.RequireCode(t, err, "VERSION_NOT_AVAILABLE")
	}

	failed, err := versions.Create(ctx, projectID, "", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := versions.MarkFailed(ctx, projectID, failed.ID, "zip slip"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	got, err := versions.Get(ctx, projectID, failed.ID)
	if err != nil {
		t.Fatalf("get failed version: %v", err)
	}
	if got.Status != version.StatusFailed || got.ErrorMessage != "zip slip" {
		t.Fatalf("failed version = %+v", got)
	}
}

func TestSetPointerRules(t *testing.T) {
	versions, projects, projectID, otherProjectID := setup(t)
	ctx := context.Background()

	uploading, err := versions.Create(ctx, projectID, "", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := versions.SetPointer(ctx, projectID, uploading.ID, ""); err == nil {
		t.Fatal("pointer to uploading version accepted")
	} else {
		testutil.RequireCode(t, err, "VERSION_NOT_AVAILABLE")
	}

	ready, err := versions.Create(ctx, projectID, "", "", "")
	if err != nil {
		t.Fatalf("create ready: %v", err)
	}
	if _, err := versions.MarkReady(ctx, projectID, ready.ID, "sha", 1, 1); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	otherReady, err := versions.Create(ctx, otherProjectID, "", "", "")
	if err != nil {
		t.Fatalf("create other ready: %v", err)
	}
	if _, err := versions.MarkReady(ctx, otherProjectID, otherReady.ID, "sha", 1, 1); err != nil {
		t.Fatalf("mark other ready: %v", err)
	}

	if _, err := versions.SetPointer(ctx, projectID, otherReady.ID, ""); err == nil {
		t.Fatal("pointer to a version of another project accepted")
	} else {
		testutil.RequireCode(t, err, "VERSION_NOT_AVAILABLE")
	}

	if _, err := versions.SetPointer(ctx, projectID, ready.ID, ""); err != nil {
		t.Fatalf("set pointer: %v", err)
	}
	updatedProject, err := projects.Get(ctx, projectID)
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if updatedProject.CurrentVersionID != ready.ID {
		t.Fatalf("current_version_id = %q, want %q", updatedProject.CurrentVersionID, ready.ID)
	}

	if _, err := versions.SetPointer(ctx, projectID, uploading.ID, "ver_other"); err == nil {
		t.Fatal("mismatched CAS accepted")
	} else {
		testutil.RequireCode(t, err, "POINTER_CONFLICT")
	}
	if _, err := versions.SetPointer(ctx, projectID, ready.ID, ready.ID); err != nil {
		t.Fatalf("matching CAS: %v", err)
	}

	if _, err := versions.SetPointer(ctx, "prj_missing", ready.ID, ""); err == nil {
		t.Fatal("pointer on unknown project accepted")
	} else {
		testutil.RequireCode(t, err, "PROJECT_NOT_FOUND")
	}
}

func TestProtection(t *testing.T) {
	versions, _, projectID, _ := setup(t)
	ctx := context.Background()
	created, err := versions.Create(ctx, projectID, "", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	protected, err := versions.SetProtected(ctx, projectID, created.ID, true)
	if err != nil {
		t.Fatalf("protect: %v", err)
	}
	if !protected.Protected {
		t.Fatal("protected = false after enabling")
	}
	unprotected, err := versions.SetProtected(ctx, projectID, created.ID, false)
	if err != nil {
		t.Fatalf("unprotect: %v", err)
	}
	if unprotected.Protected {
		t.Fatal("protected = true after disabling")
	}
	if _, err := versions.SetProtected(ctx, projectID, "ver_missing", true); err == nil {
		t.Fatal("protect on unknown version accepted")
	} else {
		testutil.RequireCode(t, err, "VERSION_NOT_FOUND")
	}
}

func TestListVersions(t *testing.T) {
	versions, _, projectID, _ := setup(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := versions.Create(ctx, projectID, "", "", ""); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	items, err := versions.List(ctx, projectID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("list = %d versions, want 3", len(items))
	}
}
