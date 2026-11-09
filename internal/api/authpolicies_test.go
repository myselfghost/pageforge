package api_test

import (
	"net/http"
	"strings"
	"testing"
)

func TestAuthPolicyAPI(t *testing.T) {
	h := newHarness(t, nil)

	status, body := h.do(t, http.MethodPost, "/api/v1/auth-policies", map[string]any{
		"name": "internal-docs",
	})
	requireStatus(t, status, http.StatusCreated, body)
	if strings.Contains(string(body), "$2") || strings.Contains(string(body), "password") {
		t.Fatalf("create response leaked password material: %s", body)
	}
	created := decode[struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Mode string `json:"mode"`
	}](t, body)
	if created.Name != "internal-docs" || created.Mode != "basic" {
		t.Fatalf("created = %+v", created)
	}

	status, body = h.do(t, http.MethodPut,
		"/api/v1/auth-policies/"+created.ID+"/users/alice", map[string]any{"password": "s3cret!"})
	requireStatus(t, status, http.StatusOK, body)
	if strings.Contains(string(body), "$2") || strings.Contains(string(body), "s3cret") {
		t.Fatalf("user response leaked password material: %s", body)
	}

	status, body = h.do(t, http.MethodGet, "/api/v1/auth-policies/"+created.ID, nil)
	requireStatus(t, status, http.StatusOK, body)
	detail := decode[struct {
		Users []struct {
			Username string `json:"username"`
			HashSet  bool   `json:"password_hash_set"`
		} `json:"users"`
		SecretVersion int64 `json:"secret_version"`
	}](t, body)
	if len(detail.Users) != 1 || detail.Users[0].Username != "alice" || !detail.Users[0].HashSet {
		t.Fatalf("users = %+v", detail.Users)
	}
	if detail.SecretVersion != 2 {
		t.Fatalf("secret_version = %d", detail.SecretVersion)
	}
	if strings.Contains(string(body), "$2") || strings.Contains(string(body), "s3cret") {
		t.Fatalf("detail response leaked password material: %s", body)
	}

	status, body = h.do(t, http.MethodPost, "/api/v1/auth-policies", map[string]any{
		"name": "bad", "mode": "none",
	})
	requireStatus(t, status, http.StatusBadRequest, body)
	requireErrorCode(t, body, "INVALID_AUTH_POLICY")

	status, body = h.do(t, http.MethodPatch, "/api/v1/auth-policies/"+created.ID, map[string]any{
		"name": "renamed",
	})
	requireStatus(t, status, http.StatusOK, body)

	status, body = h.do(t, http.MethodDelete,
		"/api/v1/auth-policies/"+created.ID+"/users/alice", nil)
	requireStatus(t, status, http.StatusNoContent, body)
	status, body = h.do(t, http.MethodDelete, "/api/v1/auth-policies/"+created.ID, nil)
	requireStatus(t, status, http.StatusNoContent, body)
	status, body = h.do(t, http.MethodGet, "/api/v1/auth-policies/"+created.ID, nil)
	requireStatus(t, status, http.StatusNotFound, body)
	requireErrorCode(t, body, "AUTH_POLICY_NOT_FOUND")
}
