package internalapi_test

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pageforge/pageforge/internal/alias"
	"github.com/pageforge/pageforge/internal/internalapi"
	"github.com/pageforge/pageforge/internal/snapshot"
	"github.com/pageforge/pageforge/internal/testutil"
)

func newServer(t *testing.T, token string) (*httptest.Server, string) {
	t.Helper()
	db := testutil.NewDB(t)
	aliases := alias.NewRepo(db, testutil.NewCipher(t))
	created, err := aliases.Create(context.Background(), alias.CreateInput{
		Name:            "primary",
		Endpoint:        "https://s3.example.com",
		Region:          "us-east-1",
		AccessKey:       "AKIASECRET",
		SecretKey:       "top-secret-value",
		BucketAllowlist: []string{"bucket-a"},
	})
	if err != nil {
		t.Fatalf("create alias: %v", err)
	}
	gen := snapshot.NewGenerator(db, 0, nil)
	srv := httptest.NewServer((&internalapi.Server{
		Token:     token,
		Snapshots: gen,
		Aliases:   aliases,
	}).Handler())
	t.Cleanup(srv.Close)
	return srv, created.ID
}

func get(t *testing.T, url, token string, header map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range header {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestSnapshotEndpointRequiresToken(t *testing.T) {
	srv, _ := newServer(t, "internal-token")
	resp := get(t, srv.URL+"/internal/v1/snapshot", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	resp = get(t, srv.URL+"/internal/v1/snapshot", "internal-token", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if strings.Contains(string(body), "top-secret-value") {
		t.Fatal("snapshot leaks alias secret")
	}
	var snap snapshot.Snapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snap.Generation == 0 || len(snap.Aliases) != 1 {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}

	resp = get(t, srv.URL+"/internal/v1/snapshot", "internal-token", map[string]string{"If-None-Match": etag})
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", resp.StatusCode)
	}
}

func TestSnapshotGzip(t *testing.T) {
	srv, _ := newServer(t, "")
	resp := get(t, srv.URL+"/internal/v1/snapshot", "", map[string]string{"Accept-Encoding": "gzip"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q", resp.Header.Get("Content-Encoding"))
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	body, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("read gzip: %v", err)
	}
	if !strings.Contains(string(body), `"aliases"`) {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestCredentialsEndpoint(t *testing.T) {
	srv, aliasID := newServer(t, "internal-token")

	resp := get(t, srv.URL+"/internal/v1/aliases/"+aliasID+"/credentials", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	resp = get(t, srv.URL+"/internal/v1/aliases/"+aliasID+"/credentials", "internal-token", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var creds struct {
		AccessKey     string `json:"access_key"`
		SecretKey     string `json:"secret_key"`
		CredentialVer int64  `json:"credential_version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&creds); err != nil {
		t.Fatalf("decode credentials: %v", err)
	}
	if creds.AccessKey != "AKIASECRET" || creds.SecretKey != "top-secret-value" || creds.CredentialVer == 0 {
		t.Fatalf("unexpected credentials: %+v", creds)
	}

	resp = get(t, srv.URL+"/internal/v1/aliases/missing/credentials", "internal-token", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
