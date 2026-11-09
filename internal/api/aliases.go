package api

import (
	"net/http"

	"github.com/pageforge/pageforge/internal/alias"
	"github.com/pageforge/pageforge/internal/httpx"
)

func (s *Server) listAliases(w http.ResponseWriter, r *http.Request) error {
	items, err := s.Aliases.List(r.Context())
	if err != nil {
		return err
	}
	views := make([]alias.View, 0, len(items))
	for _, item := range items {
		views = append(views, item.View())
	}
	httpx.WriteJSON(w, http.StatusOK, views)
	return nil
}

func (s *Server) createAlias(w http.ResponseWriter, r *http.Request) error {
	var in alias.CreateInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	created, err := s.Aliases.Create(r.Context(), in)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusCreated, created.View())
	return nil
}

func (s *Server) getAlias(w http.ResponseWriter, r *http.Request) error {
	found, err := s.Aliases.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, found.View())
	return nil
}

func (s *Server) updateAlias(w http.ResponseWriter, r *http.Request) error {
	var in alias.UpdateInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	updated, err := s.Aliases.Update(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, updated.View())
	return nil
}

func (s *Server) deleteAlias(w http.ResponseWriter, r *http.Request) error {
	if err := s.Aliases.Delete(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

type testAliasRequest struct {
	Bucket string `json:"bucket"`
}

func (s *Server) testAlias(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	found, err := s.Aliases.Get(r.Context(), id)
	if err != nil {
		return err
	}
	var req testAliasRequest
	if r.ContentLength != 0 {
		if err := httpx.DecodeJSON(r, &req); err != nil {
			return err
		}
	}
	bucket := req.Bucket
	if bucket == "" && len(found.BucketAllowlist) > 0 {
		bucket = found.BucketAllowlist[0]
	}
	if bucket == "" {
		return httpx.Invalid("BUCKET_REQUIRED", "bucket is required for a connection test")
	}
	if s.AliasTester == nil {
		return httpx.Errorf(http.StatusNotImplemented, "ALIAS_TEST_UNAVAILABLE",
			"connection testing is not configured")
	}
	_, secret, err := s.Aliases.Secret(r.Context(), id)
	if err != nil {
		return err
	}
	if err := s.AliasTester(r.Context(), found, secret, bucket); err != nil {
		return httpx.Errorf(http.StatusBadGateway, "ALIAS_TEST_FAILED",
			"connection test failed: %v", err)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "bucket": bucket})
	return nil
}
