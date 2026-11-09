package share_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pageforge/pageforge/internal/alias"
	"github.com/pageforge/pageforge/internal/httpx"
	"github.com/pageforge/pageforge/internal/project"
	"github.com/pageforge/pageforge/internal/share"
	"github.com/pageforge/pageforge/internal/store"
	"github.com/pageforge/pageforge/internal/testutil"
	"github.com/pageforge/pageforge/internal/version"
)

func setup(t *testing.T) (*share.Repo, *store.Store, string) {
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
	createdProject, err := project.NewRepo(db).Create(ctx, project.CreateInput{
		Name: "portal", AliasID: createdAlias.ID, Bucket: "static",
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	return share.NewRepo(db), db, createdProject.ID
}

func apiCode(err error) string {
	var apiErr *httpx.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return fmt.Sprintf("unexpected: %v", err)
}

func TestCreateListDelete(t *testing.T) {
	repo, _, projectID := setup(t)
	ctx := context.Background()

	created, err := repo.Create(ctx, share.CreateInput{
		ProjectID: projectID,
		Password:  "s3cret",
		MaxViews:  5,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Token == "" || created.ID == "" {
		t.Fatalf("share link ids are empty: %+v", created)
	}
	if !created.PasswordSet {
		t.Fatal("password_set = false")
	}
	if created.Status != share.StatusActive {
		t.Fatalf("status = %q", created.Status)
	}
	if created.Cache.HTML != "no-cache" {
		t.Fatalf("cache defaults not applied: %+v", created.Cache)
	}

	items, err := repo.List(ctx, projectID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 || items[0].ID != created.ID {
		t.Fatalf("list = %+v", items)
	}

	if err := repo.Delete(ctx, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := repo.Get(ctx, created.ID); err == nil {
		t.Fatal("deleted link is still readable")
	} else if apiCode(err) != "SHARE_LINK_NOT_FOUND" {
		t.Fatalf("get after delete: %s", apiCode(err))
	}
}

func TestCreateRejectsPastExpiryAndReadyVersion(t *testing.T) {
	repo, _, projectID := setup(t)
	ctx := context.Background()

	past := time.Now().Add(-time.Hour)
	if _, err := repo.Create(ctx, share.CreateInput{ProjectID: projectID, ExpiresAt: &past}); err == nil {
		t.Fatal("past expiry accepted")
	} else if apiCode(err) != "INVALID_SHARE_LINK" {
		t.Fatalf("past expiry code = %s", apiCode(err))
	}

	if _, err := repo.Create(ctx, share.CreateInput{ProjectID: "prj_missing"}); err == nil {
		t.Fatal("missing project accepted")
	} else if apiCode(err) != "PROJECT_NOT_FOUND" {
		t.Fatalf("missing project code = %s", apiCode(err))
	}
	if _, err := repo.Create(ctx, share.CreateInput{ProjectID: projectID, VersionID: "ver_missing"}); err == nil {
		t.Fatal("unknown version accepted")
	} else if apiCode(err) != "VERSION_NOT_AVAILABLE" {
		t.Fatalf("unknown version code = %s", apiCode(err))
	}
}

func TestAuthorizePasswordViewsAndExpiry(t *testing.T) {
	repo, db, projectID := setup(t)
	ctx := context.Background()

	created, err := repo.Create(ctx, share.CreateInput{
		ProjectID: projectID,
		Password:  "s3cret",
		MaxViews:  2,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := repo.Authorize(ctx, created.Token, "", true); err == nil {
		t.Fatal("empty password accepted")
	} else if apiCode(err) != "SHARE_PASSWORD_REQUIRED" {
		t.Fatalf("code = %s", apiCode(err))
	}

	if _, err := repo.Authorize(ctx, created.Token, "wrong", true); err == nil {
		t.Fatal("wrong password accepted")
	} else if apiCode(err) != "SHARE_PASSWORD_INVALID" {
		t.Fatalf("code = %s", apiCode(err))
	}

	granted, err := repo.Authorize(ctx, created.Token, "s3cret", true)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if granted.Views != 1 {
		t.Fatalf("views = %d, want 1", granted.Views)
	}
	// Asset requests do not consume views.
	_, _ = repo.Authorize(ctx, created.Token, "s3cret", false)

	if _, err := repo.Authorize(ctx, created.Token, "s3cret", true); err != nil {
		t.Fatalf("second view: %v", err)
	}
	if _, err := repo.Authorize(ctx, created.Token, "s3cret", true); err == nil {
		t.Fatal("view limit not enforced")
	} else if apiCode(err) != "SHARE_VIEWS_EXCEEDED" {
		t.Fatalf("code = %s", apiCode(err))
	}

	expired, err := repo.Create(ctx, share.CreateInput{ProjectID: projectID})
	if err != nil {
		t.Fatalf("create expired: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE share_links SET expires_at = ? WHERE id = ?`,
		time.Now().Add(-time.Minute).UTC(), expired.ID); err != nil {
		t.Fatalf("force expiry: %v", err)
	}
	if _, err := repo.Authorize(ctx, expired.Token, "", true); err == nil {
		t.Fatal("expired link authorized")
	} else if apiCode(err) != "SHARE_EXPIRED" {
		t.Fatalf("code = %s", apiCode(err))
	}
}

func TestUpdateReactivatesDisabledLink(t *testing.T) {
	repo, _, projectID := setup(t)
	ctx := context.Background()

	created, err := repo.Create(ctx, share.CreateInput{ProjectID: projectID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	disabled := false
	updated, err := repo.Update(ctx, created.ID, share.UpdateInput{Enabled: &disabled})
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if updated.Status != share.StatusDisabled {
		t.Fatalf("status = %q", updated.Status)
	}
	if _, err := repo.Authorize(ctx, created.Token, "", false); err == nil {
		t.Fatal("disabled link authorized")
	} else if apiCode(err) != "SHARE_DISABLED" {
		t.Fatalf("code = %s", apiCode(err))
	}

	enabled := true
	expires := time.Now().Add(2 * time.Hour)
	updated, err = repo.Update(ctx, created.ID, share.UpdateInput{Enabled: &enabled, ExpiresAt: &expires})
	if err != nil {
		t.Fatalf("reenable: %v", err)
	}
	if updated.Status != share.StatusActive {
		t.Fatalf("status = %q, want active", updated.Status)
	}
	if _, err := repo.Authorize(ctx, created.Token, "", false); err != nil {
		t.Fatalf("reactivated link: %v", err)
	}
}

func TestAuthorizeWithReadyVersion(t *testing.T) {
	repo, db, projectID := setup(t)
	ctx := context.Background()

	versions := version.NewRepo(db)
	v, err := versions.Create(ctx, projectID, "", "sha", "_system/manifests/prj/v")
	if err != nil {
		t.Fatalf("create version: %v", err)
	}
	if _, err := versions.MarkReady(ctx, projectID, v.ID, "sha", 1, 100); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	created, err := repo.Create(ctx, share.CreateInput{ProjectID: projectID, VersionID: v.ID})
	if err != nil {
		t.Fatalf("create with version: %v", err)
	}
	if created.VersionID != v.ID {
		t.Fatalf("version_id = %q", created.VersionID)
	}
}
