package internalapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pageforge/pageforge/internal/alias"
	"github.com/pageforge/pageforge/internal/internalapi"
	"github.com/pageforge/pageforge/internal/project"
	"github.com/pageforge/pageforge/internal/share"
	"github.com/pageforge/pageforge/internal/snapshot"
	"github.com/pageforge/pageforge/internal/testutil"
)

func newShareServer(t *testing.T) (*httptest.Server, *share.Repo, string) {
	t.Helper()
	db := testutil.NewDB(t)
	ctx := context.Background()
	createdAlias, err := alias.NewRepo(db, testutil.NewCipher(t)).Create(ctx, alias.CreateInput{
		Name: "primary", Endpoint: "https://s3.example.com", AccessKey: "ak", SecretKey: "sk",
	})
	if err != nil {
		t.Fatalf("create alias: %v", err)
	}
	createdProject, err := project.NewRepo(db).Create(ctx, project.CreateInput{
		Name: "portal", AliasID: createdAlias.ID, Bucket: "bucket-a",
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	shares := share.NewRepo(db)
	srv := httptest.NewServer((&internalapi.Server{
		Token:     "internal-token",
		Snapshots: snapshot.NewGenerator(db, 0, nil),
		Aliases:   alias.NewRepo(db, testutil.NewCipher(t)),
		Shares:    shares,
	}).Handler())
	t.Cleanup(srv.Close)
	return srv, shares, createdProject.ID
}

func postShare(t *testing.T, url, token string, body any) (*http.Response, []byte) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Internal-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp, respBody
}

func TestShareAuthorizeEndpoint(t *testing.T) {
	srv, shares, projectID := newShareServer(t)
	ctx := context.Background()

	link, err := shares.Create(ctx, share.CreateInput{
		ProjectID: projectID,
		Password:  "s3cret",
		MaxViews:  2,
	})
	if err != nil {
		t.Fatalf("create share: %v", err)
	}
	endpoint := srv.URL + "/internal/v1/share-links/" + link.Token + "/authorize"

	resp, _ := postShare(t, endpoint, "", map[string]any{"password": "s3cret", "count_view": true})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing internal token status = %d", resp.StatusCode)
	}

	resp, body := postShare(t, endpoint, "internal-token", map[string]any{"count_view": true})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing password status = %d (%s)", resp.StatusCode, body)
	}

	resp, _ = postShare(t, endpoint, "internal-token", map[string]any{"password": "wrong"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d", resp.StatusCode)
	}

	resp, body = postShare(t, endpoint, "internal-token", map[string]any{"password": "s3cret", "count_view": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorize status = %d (%s)", resp.StatusCode, body)
	}
	var grant struct {
		ProjectID string `json:"project_id"`
		Views     int64  `json:"views"`
	}
	if err := json.Unmarshal(body, &grant); err != nil {
		t.Fatalf("decode grant: %v", err)
	}
	if grant.ProjectID != projectID || grant.Views != 1 {
		t.Fatalf("grant = %+v", grant)
	}

	if _, err := shares.Update(ctx, link.ID, share.UpdateInput{
		ExpiresAt: timePtr(time.Now().Add(-time.Minute)),
	}); err == nil {
		t.Fatal("past expiry accepted")
	}

	resp, body = postShare(t, srv.URL+"/internal/v1/share-links/tok_missing/authorize", "internal-token", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing token status = %d (%s)", resp.StatusCode, body)
	}
}

func timePtr(v time.Time) *time.Time { return &v }
