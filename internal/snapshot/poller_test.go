package snapshot_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pageforge/pageforge/internal/snapshot"
)

func TestPollerUsesETagAndRejectsStaleGenerations(t *testing.T) {
	generation := uint64(1)
	etag := `"1"`
	var gotIfNoneMatch string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
