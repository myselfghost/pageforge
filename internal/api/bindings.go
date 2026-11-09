package api

import (
	"net/http"

	"github.com/pageforge/pageforge/internal/binding"
	"github.com/pageforge/pageforge/internal/httpx"
)

func (s *Server) listBindings(w http.ResponseWriter, r *http.Request) error {
	items, err := s.Bindings.List(r.Context(), r.URL.Query().Get("project_id"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, items)
	return nil
}

func (s *Server) createBinding(w http.ResponseWriter, r *http.Request) error {
	var in binding.CreateInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	created, err := s.Bindings.Create(r.Context(), in)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusCreated, created)
	return nil
}

func (s *Server) validateBinding(w http.ResponseWriter, r *http.Request) error {
	var in binding.CreateInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	checked, err := s.Bindings.Validate(r.Context(), in)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "binding": checked})
	return nil
}

func (s *Server) getBinding(w http.ResponseWriter, r *http.Request) error {
	found, err := s.Bindings.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, found)
	return nil
}

func (s *Server) updateBinding(w http.ResponseWriter, r *http.Request) error {
	var in binding.UpdateInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	updated, err := s.Bindings.Update(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, updated)
	return nil
}

func (s *Server) deleteBinding(w http.ResponseWriter, r *http.Request) error {
	if err := s.Bindings.Delete(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
