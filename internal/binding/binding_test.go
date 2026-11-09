package binding_test

import (
	"context"
	"testing"

	"github.com/pageforge/pageforge/internal/alias"
	"github.com/pageforge/pageforge/internal/binding"
	"github.com/pageforge/pageforge/internal/project"
	"github.com/pageforge/pageforge/internal/testutil"
	"github.com/pageforge/pageforge/internal/version"
)

type fixture struct {
	repo      *binding.Repo
	versions  *version.Repo
	projectID string
}

func newFixture(t *testing.T) fixture {
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
	return fixture{
		repo:      binding.NewRepo(db, "/__pageforge"),
		versions:  version.NewRepo(db),
		projectID: createdProject.ID,
	}
}

func TestCreateBindingNormalizes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	created, err := f.repo.Create(ctx, binding.CreateInput{
		Host: "Example.COM:8080", PathPrefix: "/url1/", ProjectID: f.projectID,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Host != "example.com" || created.PathPrefix != "/url1" {
		t.Fatalf("normalized = (%q, %q)", created.Host, created.PathPrefix)
	}
	if !created.Enabled {
		t.Fatal("enabled should default to true")
	}
	if created.ResponseHeaders == nil {
		t.Fatal("response_headers should be an empty slice")
	}

	root, err := f.repo.Create(ctx, binding.CreateInput{PathPrefix: "/", ProjectID: f.projectID})
	if err != nil {
		t.Fatalf("create root binding: %v", err)
	}
	if root.Host != "*" || root.PathPrefix != "/" {
		t.Fatalf("root binding = (%q, %q)", root.Host, root.PathPrefix)
	}
}

func TestBindingConflicts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.repo.Create(ctx, binding.CreateInput{PathPrefix: "/url1", ProjectID: f.projectID}); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err := f.repo.Create(ctx, binding.CreateInput{Host: "*", PathPrefix: "/url1", ProjectID: f.projectID})
	testutil.RequireCode(t, err, "BINDING_CONFLICT")

	_, err = f.repo.Create(ctx, binding.CreateInput{PathPrefix: "/__pageforge", ProjectID: f.projectID})
	testutil.RequireCode(t, err, "RESERVED_PREFIX_CONFLICT")

	_, err = f.repo.Create(ctx, binding.CreateInput{PathPrefix: "/__pageforge/x", ProjectID: f.projectID})
	testutil.RequireCode(t, err, "RESERVED_PREFIX_CONFLICT")

	if _, err := f.repo.Create(ctx, binding.CreateInput{PathPrefix: "/__pageforge2", ProjectID: f.projectID}); err != nil {
		t.Fatalf("prefix /__pageforge2 should be allowed: %v", err)
	}
}

func TestBindingValidation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	_, err := f.repo.Create(ctx, binding.CreateInput{PathPrefix: "url1", ProjectID: f.projectID})
	testutil.RequireCode(t, err, "INVALID_PATH_PREFIX")

	_, err = f.repo.Create(ctx, binding.CreateInput{PathPrefix: "/url1", ProjectID: "prj_missing"})
	testutil.RequireCode(t, err, "PROJECT_NOT_FOUND")

	uploading, err := f.versions.Create(ctx, f.projectID, "", "", "")
	if err != nil {
		t.Fatalf("create version: %v", err)
	}
	_, err = f.repo.Create(ctx, binding.CreateInput{
		PathPrefix: "/url1", ProjectID: f.projectID, PinnedVersionID: uploading.ID,
	})
	testutil.RequireCode(t, err, "VERSION_NOT_AVAILABLE")

	if _, err := f.versions.MarkReady(ctx, f.projectID, uploading.ID, "sha", 1, 10); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	pinned, err := f.repo.Create(ctx, binding.CreateInput{
		PathPrefix: "/url1", ProjectID: f.projectID, PinnedVersionID: uploading.ID,
	})
	if err != nil {
		t.Fatalf("pin ready version: %v", err)
	}
	if pinned.PinnedVersionID != uploading.ID {
		t.Fatalf("pinned_version_id = %q", pinned.PinnedVersionID)
	}

	_, err = f.repo.Create(ctx, binding.CreateInput{
		PathPrefix: "/url2", ProjectID: f.projectID, AuthPolicyID: "pol_missing",
	})
	testutil.RequireCode(t, err, "AUTH_POLICY_NOT_FOUND")
}

func TestValidateDoesNotPersist(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	checked, err := f.repo.Validate(ctx, binding.CreateInput{
		Host: "example.com", PathPrefix: "/url1", ProjectID: f.projectID,
	})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if checked.Host != "example.com" || checked.PathPrefix != "/url1" {
		t.Fatalf("validated = %+v", checked)
	}
	items, err := f.repo.List(ctx, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("validate persisted %d bindings", len(items))
	}
}

func TestUpdateAndDeleteBinding(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	first, err := f.repo.Create(ctx, binding.CreateInput{PathPrefix: "/url1", ProjectID: f.projectID})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	second, err := f.repo.Create(ctx, binding.CreateInput{PathPrefix: "/url2", ProjectID: f.projectID})
	if err != nil {
		t.Fatalf("create second: %v", err)
	}

	prefix := "/url2"
	_, err = f.repo.Update(ctx, first.ID, binding.UpdateInput{PathPrefix: &prefix})
	testutil.RequireCode(t, err, "BINDING_CONFLICT")

	host := "example.com"
	enabled := false
	updated, err := f.repo.Update(ctx, first.ID, binding.UpdateInput{Host: &host, Enabled: &enabled})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Host != "example.com" || updated.Enabled {
		t.Fatalf("updated = %+v", updated)
	}

	if err := f.repo.Delete(ctx, second.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// The soft-deleted binding no longer occupies its path.
	if _, err := f.repo.Create(ctx, binding.CreateInput{PathPrefix: "/url2", ProjectID: f.projectID}); err != nil {
		t.Fatalf("re-create after delete: %v", err)
	}
	if _, err := f.repo.Get(ctx, second.ID); err == nil {
		t.Fatal("deleted binding is still readable")
	} else {
		testutil.RequireCode(t, err, "BINDING_NOT_FOUND")
	}
}

func TestListBindingsByProject(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.repo.Create(ctx, binding.CreateInput{PathPrefix: "/url1", ProjectID: f.projectID}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.repo.Create(ctx, binding.CreateInput{PathPrefix: "/url2", ProjectID: f.projectID}); err != nil {
		t.Fatalf("create: %v", err)
	}
	all, err := f.repo.List(ctx, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("list all = %d, err = %v", len(all), err)
	}
	filtered, err := f.repo.List(ctx, f.projectID)
	if err != nil || len(filtered) != 2 {
		t.Fatalf("list by project = %d, err = %v", len(filtered), err)
	}
	none, err := f.repo.List(ctx, "prj_missing")
	if err != nil || len(none) != 0 {
		t.Fatalf("list for unknown project = %d, err = %v", len(none), err)
	}
}
