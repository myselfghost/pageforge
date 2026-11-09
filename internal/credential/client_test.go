package credential_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/pageforge/pageforge/internal/credential"
	"github.com/pageforge/pageforge/internal/snapshot"
)

func TestClientCachesByCredentialVersion(t *testing.T) {
	var calls int32
	version := int64(1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.Header.Get("Authorization") != "Bearer internal-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_key":         "AKIA",
			"secret_key":         "secret",
			"credential_version": version,
		})
	}))
	defer server.Close()

	client := credential.NewClient(server.URL, "internal-token")
	view := snapshot.AliasView{Endpoint: "https://s3.example.com", Region: "us-east-1", CredentialVer: 1}
	ctx := context.Background()

	first, err := client.Store(ctx, "ali_1", view)
	if err != nil {
		t.Fatalf("first store: %v", err)
	}
	second, err := client.Store(ctx, "ali_1", view)
	if err != nil {
		t.Fatalf("second store: %v", err)
	}
	if first != second {
		t.Fatal("store should be reused for the same credential version")
	}
	if calls != 1 {
		t.Fatalf("credential fetches = %d, want 1", calls)
	}

	version = 2
	view.CredentialVer = 2
	if _, err := client.Store(ctx, "ali_1", view); err != nil {
		t.Fatalf("third store: %v", err)
	}
	if calls != 2 {
		t.Fatalf("credential fetches = %d, want 2 after version bump", calls)
	}
}

func TestClientFailsWhenControlPlaneRejects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	client := credential.NewClient(server.URL, "bad-token")
	if _, err := client.Store(context.Background(), "ali_1", snapshot.AliasView{
		Endpoint: "https://s3.example.com", CredentialVer: 1,
	}); err == nil {
		t.Fatal("expected credential fetch error")
	}
}
