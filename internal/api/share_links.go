package api

import (
	"net/http"

	"github.com/pageforge/pageforge/internal/httpx"
	"github.com/pageforge/pageforge/internal/share"
)

func (s *Server) listShareLinks(w http.ResponseWriter, r *http.Request) error {
	items, err := s.Shares.List(r.Context(), r.URL.Query().Get("project_id"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, items)
	return nil
}

func (s *Server) createShareLink(w http.ResponseWriter, r *http.Request) error {
	var in share.CreateInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	created, err := s.Shares.Create(r.Context(), in)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusCreated, created)
	return nil
}

func (s *Server) getShareLink(w http.ResponseWriter, r *http.Request) error {
	found, err := s.Shares.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, found)
	return nil
}

func (s *Server) updateShareLink(w http.ResponseWriter, r *http.Request) error {
	var in share.UpdateInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	updated, err := s.Shares.Update(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, updated)
	return nil
}

func (s *Server) deleteShareLink(w http.ResponseWriter, r *http.Request) error {
	if err := s.Shares.Delete(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
