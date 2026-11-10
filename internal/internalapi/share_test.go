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

// 数据面不再逐请求回源，改为整目录拉取；这里验证目录内容与鉴权边界。
func TestCredentialDirectoryEndpoint(t *testing.T) {
	srv, _, _ := newShareServer(t)

	resp, body := getInternal(t, srv.URL+"/internal/v1/alias-credentials", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing internal token status = %d", resp.StatusCode)
	}
	resp, body = getInternal(t, srv.URL+"/internal/v1/alias-credentials", "internal-token")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (%s)", resp.StatusCode, body)
	}
	var dir snapshot.CredentialDirectory
	if err := json.Unmarshal(body, &dir); err != nil {
		t.Fatalf("decode directory: %v", err)
	}
	if len(dir.Aliases) != 1 {
		t.Fatalf("expected one alias, got %+v", dir.Aliases)
	}
	for _, creds := range dir.Aliases {
		if creds.AccessKey != "ak" || len(creds.SecretCipher) == 0 {
			t.Fatalf("credentials = %+v", creds)
		}
		secret, err := testutil.NewCipher(t).DecryptString(creds.SecretCipher)
		if err != nil {
			t.Fatalf("decrypt directory secret: %v", err)
		}
		if secret != "sk" {
			t.Fatalf("decrypted secret = %q", secret)
		}
	}
}

func TestShareDirectoryEndpointAndViewWriteBack(t *testing.T) {
	srv, shares, projectID := newShareServer(t)
	ctx := context.Background()
	link, err := shares.Create(ctx, share.CreateInput{ProjectID: projectID, Password: "s3cret", MaxViews: 5})
	if err != nil {
		t.Fatalf("create share: %v", err)
	}

	resp, body := getInternal(t, srv.URL+"/internal/v1/share-links", "internal-token")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (%s)", resp.StatusCode, body)
	}
	var dir snapshot.ShareDirectory
	if err := json.Unmarshal(body, &dir); err != nil {
		t.Fatalf("decode directory: %v", err)
	}
	secrets, ok := dir.Links[link.Token]
	if !ok || secrets.ProjectID != projectID || secrets.PasswordHash == "" || !secrets.Enabled || secrets.MaxViews != 5 {
		t.Fatalf("share directory = %+v", dir.Links)
	}

	// 离线计数回写：不做 token 校验，直接累加。
	resp, body = postShare(t, srv.URL+"/internal/v1/share-links/"+link.Token+"/views", "internal-token",
		map[string]any{"count": 3})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("views status = %d (%s)", resp.StatusCode, body)
	}
	got, err := shares.Get(ctx, link.ID)
	if err != nil {
		t.Fatalf("reload share: %v", err)
	}
	if got.Views != 3 {
		t.Fatalf("views = %d, want 3", got.Views)
	}
}

func getInternal(t *testing.T, url, token string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("X-Internal-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}
