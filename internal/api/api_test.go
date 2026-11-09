package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pageforge/pageforge/internal/alias"
	"github.com/pageforge/pageforge/internal/api"
	"github.com/pageforge/pageforge/internal/authpolicy"
	"github.com/pageforge/pageforge/internal/binding"
	"github.com/pageforge/pageforge/internal/deploy"
	"github.com/pageforge/pageforge/internal/objectstore"
	"github.com/pageforge/pageforge/internal/project"
	"github.com/pageforge/pageforge/internal/share"
	"github.com/pageforge/pageforge/internal/store"
	"github.com/pageforge/pageforge/internal/testutil"
	"github.com/pageforge/pageforge/internal/version"
)

const testToken = "admin-secret"

type harness struct {
	server   *httptest.Server
	db       *store.Store
	objects  *testutil.MemoryStore
	versions *version.Repo
	projects *project.Repo
	aliases  *alias.Repo
	auth     *authpolicy.Repo
}

func newHarness(t *testing.T, tester api.ConnectionTester) *harness {
	t.Helper()
	db := testutil.NewDB(t)
	objects := testutil.NewMemoryStore()
	h := &harness{
		db:       db,
		objects:  objects,
		versions: version.NewRepo(db),
		projects: project.NewRepo(db),
		aliases:  alias.NewRepo(db, testutil.NewCipher(t)),
		auth:     authpolicy.NewRepo(db),
	}
	deployments := deploy.NewService(db, deploy.Options{
		Config: deploy.Config{TempDir: t.TempDir()},
		Stores: func(context.Context, project.Project) (objectstore.ObjectStore, error) {
			return objects, nil
		},
	})
	srv := &api.Server{
		Aliases:      h.aliases,
		Projects:     h.projects,
		Bindings:     binding.NewRepo(db, "/__pageforge"),
		AuthPolicies: h.auth,
		Versions:     h.versions,
		Shares:       share.NewRepo(db),
		Deployments:  deployments,
		AdminToken:   testToken,
		AliasTester:  tester,
		Stores: func(context.Context, project.Project) (objectstore.ObjectStore, error) {
			return objects, nil
		},
		PackagePrefix: "_system/packages",
	}
	h.server = httptest.NewServer(srv.Handler())
	t.Cleanup(h.server.Close)
	return h
}

func (h *harness) do(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, h.server.URL+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if path != "/healthz" {
		req.Header.Set("Authorization", "Bearer "+testToken)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, data
}

func decode[T any](t *testing.T, data []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("decode %s: %v", string(data), err)
	}
	return v
}

type errorResponse struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

func requireStatus(t *testing.T, got, want int, body []byte) {
	t.Helper()
	if got != want {
		t.Fatalf("status = %d, want %d (body: %s)", got, want, string(body))
	}
}

func requireErrorCode(t *testing.T, body []byte, code string) {
	t.Helper()
	envelope := decode[errorResponse](t, body)
	if envelope.Error.Code != code {
		t.Fatalf("error code = %q, want %q (body: %s)", envelope.Error.Code, code, string(body))
	}
}

func TestHealthAndAuth(t *testing.T) {
	h := newHarness(t, nil)

	status, _ := h.do(t, http.MethodGet, "/healthz", nil)
	requireStatus(t, status, http.StatusOK, nil)

	req, _ := http.NewRequest(http.MethodGet, h.server.URL+"/api/v1/aliases", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	requireStatus(t, resp.StatusCode, http.StatusUnauthorized, body)
	requireErrorCode(t, body, "UNAUTHORIZED")

	status, body = h.do(t, http.MethodGet, "/api/v1/aliases", nil)
	requireStatus(t, status, http.StatusOK, body)
	if strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("aliases = %s, want []", string(body))
	}
}

func TestAliasAPIHidesSecret(t *testing.T) {
	h := newHarness(t, nil)

	status, body := h.do(t, http.MethodPost, "/api/v1/aliases", map[string]any{
		"name": "minio", "endpoint": "https://minio.example.com",
		"access_key": "ak", "secret_key": "super-secret",
	})
	requireStatus(t, status, http.StatusCreated, body)
	if strings.Contains(string(body), "super-secret") || strings.Contains(string(body), "secret_key\"") {
		t.Fatalf("response leaks the secret: %s", string(body))
	}
	created := decode[alias.View](t, body)
	if !created.SecretKeySet {
		t.Fatal("secret_key_set = false")
	}
	if created.Region != "us-east-1" {
		t.Fatalf("region = %q", created.Region)
	}

	status, body = h.do(t, http.MethodPost, "/api/v1/aliases", map[string]any{
		"name": "minio", "endpoint": "https://minio.example.com",
		"access_key": "ak", "secret_key": "super-secret",
	})
	requireStatus(t, status, http.StatusConflict, body)
	requireErrorCode(t, body, "ALIAS_NAME_TAKEN")

	status, body = h.do(t, http.MethodPatch, "/api/v1/aliases/"+created.ID, map[string]any{
		"secret_key": "rotated",
	})
	requireStatus(t, status, http.StatusOK, body)
	rotated := decode[alias.View](t, body)
	if rotated.CredentialVersion != 2 {
		t.Fatalf("credential_version = %d, want 2", rotated.CredentialVersion)
	}
}

func TestAliasTestEndpoint(t *testing.T) {
	var lastBucket string
	h := newHarness(t, func(_ context.Context, a alias.Alias, secret, bucket string) error {
		if secret != "sk" {
			return errors.New("unexpected secret")
		}
		lastBucket = bucket
		return nil
	})

	status, body := h.do(t, http.MethodPost, "/api/v1/aliases", map[string]any{
		"name": "minio", "endpoint": "https://minio.example.com",
		"access_key": "ak", "secret_key": "sk", "bucket_allowlist": []string{"static"},
	})
	requireStatus(t, status, http.StatusCreated, body)
	created := decode[alias.View](t, body)

	status, body = h.do(t, http.MethodPost, "/api/v1/aliases/"+created.ID+"/test", nil)
	requireStatus(t, status, http.StatusOK, body)
	if lastBucket != "static" {
		t.Fatalf("tested bucket = %q, want static", lastBucket)
	}
}

func TestProjectBindingVersionFlow(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	status, body := h.do(t, http.MethodPost, "/api/v1/aliases", map[string]any{
		"name": "minio", "endpoint": "https://minio.example.com",
		"access_key": "ak", "secret_key": "sk",
	})
	requireStatus(t, status, http.StatusCreated, body)
	createdAlias := decode[alias.View](t, body)

	status, body = h.do(t, http.MethodPost, "/api/v1/projects", map[string]any{
		"name": "portal", "alias_id": createdAlias.ID, "bucket": "static",
	})
	requireStatus(t, status, http.StatusCreated, body)
	createdProject := decode[project.Project](t, body)

	status, body = h.do(t, http.MethodPost, "/api/v1/projects", map[string]any{
		"name": "other", "alias_id": "als_missing", "bucket": "static",
	})
	requireStatus(t, status, http.StatusNotFound, body)
	requireErrorCode(t, body, "ALIAS_NOT_FOUND")

	status, body = h.do(t, http.MethodPost, "/api/v1/bindings", map[string]any{
		"host": "Example.com:8080", "path_prefix": "/url1/", "project_id": createdProject.ID,
	})
	requireStatus(t, status, http.StatusCreated, body)
	createdBinding := decode[binding.Binding](t, body)
	if createdBinding.Host != "example.com" || createdBinding.PathPrefix != "/url1" {
		t.Fatalf("binding = %+v", createdBinding)
	}

	status, body = h.do(t, http.MethodPost, "/api/v1/bindings", map[string]any{
		"host": "example.com", "path_prefix": "/url1", "project_id": createdProject.ID,
	})
	requireStatus(t, status, http.StatusConflict, body)
	requireErrorCode(t, body, "BINDING_CONFLICT")

	status, body = h.do(t, http.MethodPost, "/api/v1/bindings/validate", map[string]any{
		"path_prefix": "/__pageforge", "project_id": createdProject.ID,
	})
	requireStatus(t, status, http.StatusConflict, body)
	requireErrorCode(t, body, "RESERVED_PREFIX_CONFLICT")

	uploading, err := h.versions.Create(ctx, createdProject.ID, "", "", "")
	if err != nil {
		t.Fatalf("create version: %v", err)
	}
	status, body = h.do(t, http.MethodPost, "/api/v1/projects/"+createdProject.ID+"/pointer",
		map[string]any{"version_id": uploading.ID})
	requireStatus(t, status, http.StatusBadRequest, body)
	requireErrorCode(t, body, "VERSION_NOT_AVAILABLE")

	if _, err := h.versions.MarkReady(ctx, createdProject.ID, uploading.ID, "sha", 2, 200); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	status, body = h.do(t, http.MethodPost, "/api/v1/projects/"+createdProject.ID+"/pointer",
		map[string]any{"version_id": uploading.ID})
	requireStatus(t, status, http.StatusOK, body)

	status, body = h.do(t, http.MethodGet, "/api/v1/projects/"+createdProject.ID, nil)
	requireStatus(t, status, http.StatusOK, body)
	if got := decode[project.Project](t, body); got.CurrentVersionID != uploading.ID {
		t.Fatalf("current_version_id = %q", got.CurrentVersionID)
	}

	status, body = h.do(t, http.MethodPost,
		"/api/v1/projects/"+createdProject.ID+"/versions/"+uploading.ID+"/protect",
		map[string]any{"protected": true})
	requireStatus(t, status, http.StatusOK, body)
	if got := decode[version.Version](t, body); !got.Protected {
		t.Fatal("version was not protected")
	}

	status, body = h.do(t, http.MethodDelete, "/api/v1/projects/"+createdProject.ID, nil)
	requireStatus(t, status, http.StatusNoContent, body)

	status, body = h.do(t, http.MethodGet, "/api/v1/bindings?project_id="+createdProject.ID, nil)
	requireStatus(t, status, http.StatusOK, body)
	items := decode[[]binding.Binding](t, body)
	if len(items) != 0 {
		t.Fatalf("bindings after project delete = %d, want 0", len(items))
	}
}

func TestProjectRetentionPolicyCRUD(t *testing.T) {
	h := newHarness(t, nil)

	status, body := h.do(t, http.MethodPost, "/api/v1/aliases", map[string]any{
		"name": "minio", "endpoint": "https://minio.example.com",
		"access_key": "ak", "secret_key": "sk",
	})
	requireStatus(t, status, http.StatusCreated, body)
	createdAlias := decode[alias.View](t, body)

	// 创建时不提供 keep_*，应继承全局（nil）。
	status, body = h.do(t, http.MethodPost, "/api/v1/projects", map[string]any{
		"name": "inherit", "alias_id": createdAlias.ID, "bucket": "static",
	})
	requireStatus(t, status, http.StatusCreated, body)
	inherit := decode[project.Project](t, body)
	if inherit.KeepVersions != nil || inherit.KeepDays != nil {
		t.Fatalf("omitted keep_* should be nil, got %+v", inherit)
	}

	// 创建时设置 keep_*。
	status, body = h.do(t, http.MethodPost, "/api/v1/projects", map[string]any{
		"name": "custom", "alias_id": createdAlias.ID, "bucket": "static",
		"keep_versions": 5, "keep_days": 7,
	})
	requireStatus(t, status, http.StatusCreated, body)
	custom := decode[project.Project](t, body)
	if custom.KeepVersions == nil || *custom.KeepVersions != 5 ||
		custom.KeepDays == nil || *custom.KeepDays != 7 {
		t.Fatalf("created keep_* = %+v", custom)
	}

	// PATCH 覆盖为其它值。
	status, body = h.do(t, http.MethodPatch, "/api/v1/projects/"+custom.ID, map[string]any{
		"keep_versions": 3,
	})
	requireStatus(t, status, http.StatusOK, body)
	updated := decode[project.Project](t, body)
	if updated.KeepVersions == nil || *updated.KeepVersions != 3 ||
		updated.KeepDays == nil || *updated.KeepDays != 7 {
		t.Fatalf("patched keep_* = %+v", updated)
	}

	// PATCH 显式 null 清除，回退继承全局。
	status, body = h.do(t, http.MethodPatch, "/api/v1/projects/"+custom.ID, map[string]any{
		"keep_versions": nil,
	})
	requireStatus(t, status, http.StatusOK, body)
	cleared := decode[project.Project](t, body)
	if cleared.KeepVersions != nil {
		t.Fatalf("null keep_versions should clear to nil, got %+v", cleared.KeepVersions)
	}
	if cleared.KeepDays == nil || *cleared.KeepDays != 7 {
		t.Fatalf("keep_days should be unchanged, got %+v", cleared.KeepDays)
	}

	// PATCH 负数返回校验错误。
	status, body = h.do(t, http.MethodPatch, "/api/v1/projects/"+custom.ID, map[string]any{
		"keep_days": -1,
	})
	requireStatus(t, status, http.StatusBadRequest, body)
	requireErrorCode(t, body, "INVALID_PROJECT")
}

func TestInvalidJSON(t *testing.T) {
	h := newHarness(t, nil)
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/api/v1/aliases",
		strings.NewReader(`{"name": "x", "unknown": 1}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	requireStatus(t, resp.StatusCode, http.StatusBadRequest, body)
	requireErrorCode(t, body, "INVALID_JSON")
}
