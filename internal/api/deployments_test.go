package api_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/pageforge/pageforge/internal/alias"
	"github.com/pageforge/pageforge/internal/project"
)

func TestDeploymentLifecycle(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	a, err := h.aliases.Create(ctx, alias.CreateInput{
		Name:            "test",
		Endpoint:        "http://127.0.0.1:9000",
		AccessKey:       "key",
		SecretKey:       "secret",
		BucketAllowlist: []string{"static"},
	})
	if err != nil {
		t.Fatalf("create alias: %v", err)
	}
	p, err := h.projects.Create(ctx, project.CreateInput{
		Name: "site", AliasID: a.ID, Bucket: "static",
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	status, body := h.do(t, http.MethodPost, "/api/v1/deployments", map[string]any{
		"project_id":      p.ID,
		"idempotency_key": "api-key",
	})
	if status != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", status, body)
	}
	var created struct {
		DeploymentID string `json:"deployment_id"`
		VersionID    string `json:"version_id"`
		Status       string `json:"status"`
		UploadURL    string `json:"upload_url"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.DeploymentID == "" || created.VersionID == "" || created.UploadURL == "" {
		t.Fatalf("create body = %s", body)
	}

	status, body = h.do(t, http.MethodPost, "/api/v1/deployments", map[string]any{
		"project_id":      p.ID,
		"idempotency_key": "api-key",
	})
	if status != http.StatusOK {
		t.Fatalf("idempotent create status = %d body=%s", status, body)
	}

	req, err := http.NewRequest(http.MethodPut, h.server.URL+created.UploadURL, bytes.NewReader(zipBytes(t)))
	if err != nil {
		t.Fatalf("new upload request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/zip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload package: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload status = %d", resp.StatusCode)
	}

	status, body = h.do(t, http.MethodPost, "/api/v1/deployments/"+created.DeploymentID+"/complete", nil)
	if status != http.StatusOK {
		t.Fatalf("complete status = %d body=%s", status, body)
	}
	var done struct {
		Status     string `json:"status"`
		FileCount  int64  `json:"file_count"`
		TotalBytes int64  `json:"total_bytes"`
	}
	if err := json.Unmarshal(body, &done); err != nil {
		t.Fatalf("decode complete: %v", err)
	}
	if done.Status != "ready" || done.FileCount != 1 || done.TotalBytes == 0 {
		t.Fatalf("complete body = %s", body)
	}

	status, body = h.do(t, http.MethodGet, "/api/v1/deployments/"+created.DeploymentID, nil)
	if status != http.StatusOK {
		t.Fatalf("get status = %d body=%s", status, body)
	}
	if _, err := h.projects.Get(ctx, p.ID); err != nil {
		t.Fatalf("get project: %v", err)
	}
}

func zipBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	part, err := writer.Create("index.html")
	if err != nil {
		t.Fatalf("create zip entry: %v", err)
	}
	if _, err := part.Write([]byte("hello")); err != nil {
		t.Fatalf("write zip entry: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}
