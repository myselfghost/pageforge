package snapshot_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/pageforge/pageforge/internal/snapshot"
)

// serveEmptyDirectories answers the credential, password and share-link
// directory requests with empty payloads, so a test can focus on the snapshot
// itself. It reports whether it handled the request.
func serveEmptyDirectories(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/internal/v1/auth-policies":
		_ = json.NewEncoder(w).Encode(snapshot.AuthDirectory{Policies: map[string]snapshot.AuthPolicySecrets{}})
	case "/internal/v1/alias-credentials":
		_ = json.NewEncoder(w).Encode(snapshot.CredentialDirectory{Aliases: map[string]snapshot.AliasCredentials{}})
	case "/internal/v1/share-links":
		_ = json.NewEncoder(w).Encode(snapshot.ShareDirectory{Links: map[string]snapshot.ShareLinkSecrets{}})
	default:
		return false
	}
	return true
}

func TestPollerUsesETagAndRejectsStaleGenerations(t *testing.T) {
	generation := uint64(1)
	etag := `"1"`
	var gotIfNoneMatch string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveEmptyDirectories(w, r) {
			return
		}
		gotIfNoneMatch = r.Header.Get("If-None-Match")
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		_ = json.NewEncoder(w).Encode(snapshot.Snapshot{
			Generation: generation,
			Bindings:   []snapshot.BindingView{},
		})
	}))
	defer server.Close()

	store := snapshot.NewStore()
	poller := snapshot.NewPoller(snapshot.PollerConfig{BaseURL: server.URL, Store: store})
	if err := poller.Poll(context.Background()); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if got := store.Load(); got == nil || got.Generation != 1 {
		t.Fatalf("active snapshot = %+v, want generation 1", got)
	}

	if err := poller.Poll(context.Background()); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if gotIfNoneMatch != etag {
		t.Fatalf("If-None-Match = %q, want %q", gotIfNoneMatch, etag)
	}

	etag = `"2"`
	generation = 2
	if err := poller.Poll(context.Background()); err != nil {
		t.Fatalf("third poll: %v", err)
	}
	if got := store.Load(); got.Generation != 2 {
		t.Fatalf("generation = %d, want 2", got.Generation)
	}

	etag = `"1"`
	generation = 1
	if err := poller.Poll(context.Background()); err == nil {
		t.Fatal("stale generation must be rejected")
	}
	if got := store.Load(); got.Generation != 2 {
		t.Fatalf("active snapshot must keep generation 2, got %d", got.Generation)
	}
}

func TestPollerKeepsLastSnapshotOnFailure(t *testing.T) {
	failing := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveEmptyDirectories(w, r) {
			return
		}
		if failing {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("ETag", `"5"`)
		_ = json.NewEncoder(w).Encode(snapshot.Snapshot{
			Generation: 5,
			Bindings:   []snapshot.BindingView{},
		})
	}))
	defer server.Close()

	store := snapshot.NewStore()
	poller := snapshot.NewPoller(snapshot.PollerConfig{BaseURL: server.URL, Store: store})
	if err := poller.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	failing = true
	if err := poller.Poll(context.Background()); err == nil {
		t.Fatal("expected poll error")
	}
	if got := store.Load(); got == nil || got.Generation != 5 {
		t.Fatalf("last good snapshot lost: %+v", got)
	}
}

// snapshotServer serves the snapshot and the three directory endpoints used by
// a successful poll.
func snapshotServer(t *testing.T, generation uint64, failures *atomic.Int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failures != nil && failures.Add(-1) >= 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/internal/v1/snapshot":
			w.Header().Set("ETag", `"`+strconv.FormatUint(generation, 10)+`"`)
			_ = json.NewEncoder(w).Encode(snapshot.Snapshot{
				Generation: generation,
				Bindings: []snapshot.BindingView{
					{ID: "bnd_1", Host: "example.com", PathPrefix: "/", Enabled: true},
				},
				AuthPolicies: map[string]snapshot.AuthPolicyView{
					"pol_1": {ID: "pol_1", Realm: "r", Mode: "basic", SecretVersion: 2},
				},
			})
		case "/internal/v1/auth-policies":
			_ = json.NewEncoder(w).Encode(snapshot.AuthDirectory{
				Policies: map[string]snapshot.AuthPolicySecrets{
					"pol_1": {Realm: "r", Mode: "basic", SecretVersion: 2, Users: map[string]string{"u": "hash"}},
				},
			})
		case "/internal/v1/alias-credentials":
			_ = json.NewEncoder(w).Encode(snapshot.CredentialDirectory{
				Aliases: map[string]snapshot.AliasCredentials{
					"ali_1": {AccessKey: "AKIA", SecretCipher: []byte("sealed"), CredentialVer: 1},
				},
			})
		case "/internal/v1/share-links":
			_ = json.NewEncoder(w).Encode(snapshot.ShareDirectory{
				Links: map[string]snapshot.ShareLinkSecrets{
					"token_1": {ID: "shr_1", ProjectID: "prj_1", Enabled: true, MaxViews: 10},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestPollerWritesSnapshotToDiskAndReloadsIt(t *testing.T) {
	dir := t.TempDir()
	server := snapshotServer(t, 7, nil)
	poller := snapshot.NewPoller(snapshot.PollerConfig{BaseURL: server.URL, CacheDir: dir})
	if err := poller.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	for _, name := range []string{"snapshot.json", "auth-policies.json", "aliases.json", "shares.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s not written: %v", name, err)
		}
	}

	// A restart with an unreachable control plane must come up from disk.
	down := snapshot.NewPoller(snapshot.PollerConfig{
		BaseURL:  "http://127.0.0.1:1",
		CacheDir: dir,
		Store:    snapshot.NewStore(),
	})
	if fromDisk, err := down.Bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap from cache: %v", err)
	} else if !fromDisk {
		t.Fatal("bootstrap should report it fell back to disk")
	}
	restored := down.Store.Load()
	if restored == nil || restored.Generation != 7 || len(restored.Bindings) != 1 {
		t.Fatalf("restored snapshot = %+v, want generation 7 with one binding", restored)
	}
	auth := down.Store.Auth()
	if auth == nil || auth.Policies["pol_1"].Users["u"] != "hash" {
		t.Fatalf("password directory not restored: %+v", auth)
	}
	creds := down.Store.Credentials()
	if creds == nil || string(creds.Aliases["ali_1"].SecretCipher) != "sealed" {
		t.Fatalf("credential directory not restored: %+v", creds)
	}
	shares := down.Store.Shares()
	if shares == nil || shares.Links["token_1"].ProjectID != "prj_1" {
		t.Fatalf("share directory not restored: %+v", shares)
	}
}

func TestPollerBootstrapRetriesBeforeFailing(t *testing.T) {
	var failures atomic.Int32
	failures.Store(2)
	server := snapshotServer(t, 3, &failures)
	store := snapshot.NewStore()
	poller := snapshot.NewPoller(snapshot.PollerConfig{BaseURL: server.URL, Store: store})
	if fromDisk, err := poller.Bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap should succeed on the third attempt: %v", err)
	} else if fromDisk {
		t.Fatal("bootstrap from a live control plane must not report a disk fallback")
	}
	if got := store.Load(); got == nil || got.Generation != 3 {
		t.Fatalf("active snapshot = %+v, want generation 3", got)
	}
}

// 304 只表示路由没变；凭证目录与分享链接目录没有 generation，必须继续刷新。
func TestPollerRefreshesDirectoriesOnNotModified(t *testing.T) {
	secret := "secret-v1"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/v1/snapshot":
			w.Header().Set("ETag", `"4"`)
			if r.Header.Get("If-None-Match") == `"4"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			_ = json.NewEncoder(w).Encode(snapshot.Snapshot{
				Generation: 4,
				Bindings:   []snapshot.BindingView{},
			})
		case "/internal/v1/auth-policies":
			_ = json.NewEncoder(w).Encode(snapshot.AuthDirectory{Policies: map[string]snapshot.AuthPolicySecrets{}})
		case "/internal/v1/share-links":
			_ = json.NewEncoder(w).Encode(snapshot.ShareDirectory{Links: map[string]snapshot.ShareLinkSecrets{}})
		case "/internal/v1/alias-credentials":
			_ = json.NewEncoder(w).Encode(snapshot.CredentialDirectory{
				Aliases: map[string]snapshot.AliasCredentials{
					"ali_1": {AccessKey: "AKIA", SecretCipher: []byte(secret), CredentialVer: 1},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	store := snapshot.NewStore()
	poller := snapshot.NewPoller(snapshot.PollerConfig{BaseURL: server.URL, Store: store})
	if err := poller.Poll(context.Background()); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if got := store.Credentials(); got == nil || string(got.Aliases["ali_1"].SecretCipher) != "secret-v1" {
		t.Fatalf("credentials after first poll = %+v", got)
	}

	// The control plane rotates the key without advancing the snapshot.
	secret = "secret-v2"
	if err := poller.Poll(context.Background()); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if got := store.Credentials(); got == nil || string(got.Aliases["ali_1"].SecretCipher) != "secret-v2" {
		t.Fatalf("304 must still refresh credentials, got %+v", got)
	}
}

func TestPollerFailsWithoutControlOrCache(t *testing.T) {
	poller := snapshot.NewPoller(snapshot.PollerConfig{BaseURL: "http://127.0.0.1:1", CacheDir: t.TempDir()})
	if _, err := poller.Bootstrap(context.Background()); err == nil {
		t.Fatal("bootstrap must fail when the control plane and cache are both unavailable")
	}
	if poller.Store.Load() != nil {
		t.Fatal("no snapshot must be activated")
	}
}

func TestPollerRejectsCorruptDiskCache(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "snapshot.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed corrupt cache: %v", err)
	}
	poller := snapshot.NewPoller(snapshot.PollerConfig{BaseURL: "http://127.0.0.1:1", CacheDir: dir})
	if _, err := poller.Bootstrap(context.Background()); err == nil {
		t.Fatal("corrupt cache must not be loaded")
	}
	if poller.Store.Load() != nil {
		t.Fatal("corrupt cache must not activate a snapshot")
	}
}
