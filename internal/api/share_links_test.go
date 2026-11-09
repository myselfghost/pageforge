package api_test

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pageforge/pageforge/internal/alias"
	"github.com/pageforge/pageforge/internal/objectstore"
	"github.com/pageforge/pageforge/internal/project"
	"github.com/pageforge/pageforge/internal/share"
)

func bootstrapProjectWithVersion(t *testing.T, h *harness, retention bool) (string, string) {
	t.Helper()
	status, body := h.do(t, http.MethodPost, "/api/v1/aliases", map[string]any{
		"name": "minio", "endpoint": "https://minio.example.com",
		"access_key": "ak", "secret_key": "sk",
	})
	requireStatus(t, status, http.StatusCreated, body)
	createdAlias := decode[alias.View](t, body)

	status, body = h.do(t, http.MethodPost, "/api/v1/projects", map[string]any{
		"name": "portal", "alias_id": createdAlias.ID, "bucket": "static",
		"package_retention": retention,
	})
	requireStatus(t, status, http.StatusCreated, body)
	createdProject := decode[project.Project](t, body)

	ctx := context.Background()
	created, err := h.versions.Create(ctx, createdProject.ID, "", "sha", "")
	if err != nil {
		t.Fatalf("create version: %v", err)
	}
	if _, err := h.versions.MarkReady(ctx, createdProject.ID, created.ID, "sha", 1, 100); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	return createdProject.ID, created.ID
}

func TestShareLinkAPI(t *testing.T) {
	h := newHarness(t, nil)
	projectID, versionID := bootstrapProjectWithVersion(t, h, false)

	status, body := h.do(t, http.MethodPost, "/api/v1/share-links", map[string]any{
		"project_id": projectID,
		"version_id": versionID,
		"password":   "s3cret",
		"max_views":  10,
	})
	requireStatus(t, status, http.StatusCreated, body)
	if strings.Contains(string(body), "s3cret") || strings.Contains(string(body), "password_hash") {
		t.Fatalf("share link response leaks the password: %s", string(body))
	}
	created := decode[share.Link](t, body)
	if !created.PasswordSet || created.Status != share.StatusActive {
		t.Fatalf("created = %+v", created)
	}
	if created.Token == "" || created.VersionID != versionID {
		t.Fatalf("created token/version = %+v", created)
	}

	status, body = h.do(t, http.MethodGet, "/api/v1/share-links?project_id="+projectID, nil)
	requireStatus(t, status, http.StatusOK, body)
	if items := decode[[]share.Link](t, body); len(items) != 1 || items[0].ID != created.ID {
		t.Fatalf("list = %s", string(body))
	}

	disabled := false
	status, body = h.do(t, http.MethodPatch, "/api/v1/share-links/"+created.ID,
		map[string]any{"enabled": disabled})
	requireStatus(t, status, http.StatusOK, body)
	if updated := decode[share.Link](t, body); updated.Status != share.StatusDisabled {
		t.Fatalf("status = %q", updated.Status)
	}

	status, body = h.do(t, http.MethodPost, "/api/v1/share-links", map[string]any{
		"project_id": projectID,
		"expires_at": time.Now().Add(-time.Minute).Format(time.RFC3339),
	})
	requireStatus(t, status, http.StatusBadRequest, body)
	requireErrorCode(t, body, "INVALID_SHARE_LINK")

	status, body = h.do(t, http.MethodDelete, "/api/v1/share-links/"+created.ID, nil)
	requireStatus(t, status, http.StatusNoContent, body)
	status, body = h.do(t, http.MethodGet, "/api/v1/share-links/"+created.ID, nil)
	requireStatus(t, status, http.StatusNotFound, body)
	requireErrorCode(t, body, "SHARE_LINK_NOT_FOUND")
}

func TestDownloadOriginalPackage(t *testing.T) {
	h := newHarness(t, nil)
	projectID, versionID := bootstrapProjectWithVersion(t, h, true)

	payload := []byte("PK\x03\x04 fake zip bytes")
	key := "_system/packages/" + projectID + "/" + versionID + ".zip"
	if err := h.objects.Put(context.Background(), "static", key,
		bytes.NewReader(payload), int64(len(payload)), objectstore.PutOptions{ContentType: "application/zip"}); err != nil {
		t.Fatalf("put package: %v", err)
	}

	status, body := h.do(t, http.MethodGet, "/api/v1/projects/"+projectID+"/versions/"+versionID+"/package", nil)
	requireStatus(t, status, http.StatusOK, body)
	if !bytes.Equal(body, payload) {
		t.Fatalf("package body = %q, want %q", body, payload)
	}

	head, _ := http.NewRequest(http.MethodHead,
		h.server.URL+"/api/v1/projects/"+projectID+"/versions/"+versionID+"/package", nil)
	head.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(head)
	if err != nil {
		t.Fatalf("head package: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/zip" {
		t.Fatalf("Content-Type = %q", got)
	}
	if disp := resp.Header.Get("Content-Disposition"); !strings.Contains(disp, "attachment") {
		t.Fatalf("Content-Disposition = %q", disp)
	}
}

func TestDownloadPackageUnavailable(t *testing.T) {
	h := newHarness(t, nil)
	projectID, versionID := bootstrapProjectWithVersion(t, h, false)

	status, body := h.do(t, http.MethodGet, "/api/v1/projects/"+projectID+"/versions/"+versionID+"/package", nil)
	requireStatus(t, status, http.StatusNotFound, body)
	requireErrorCode(t, body, "PACKAGE_NOT_AVAILABLE")

	status, body = h.do(t, http.MethodGet, "/api/v1/projects/"+projectID+"/versions/ver_missing/package", nil)
	requireStatus(t, status, http.StatusNotFound, body)
}
