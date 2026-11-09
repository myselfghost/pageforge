package api

import (
	"net/http"
	"strconv"

	"github.com/pageforge/pageforge/internal/deploy"
	"github.com/pageforge/pageforge/internal/httpx"
)

func (s *Server) listDeployments(w http.ResponseWriter, r *http.Request) error {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 500 {
			return httpx.Invalid("INVALID_REQUEST", "limit must be between 1 and 500")
		}
		limit = n
	}
	items, err := s.Deployments.List(r.Context(), r.URL.Query().Get("project_id"), limit)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, items)
	return nil
}

func (s *Server) createDeployment(w http.ResponseWriter, r *http.Request) error {
	var in deploy.CreateInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	view, reused, err := s.Deployments.Create(r.Context(), in)
	if err != nil {
		return err
	}
	status := http.StatusCreated
	if reused {
		status = http.StatusOK
	}
	httpx.WriteJSON(w, status, view)
	return nil
}

// uploadDeploymentPackage streams the zip body; it is not JSON, so it bypasses
// the shared decoder.
func (s *Server) uploadDeploymentPackage(w http.ResponseWriter, r *http.Request) error {
	view, err := s.Deployments.PutPackage(r.Context(), r.PathValue("id"), r.Body, r.ContentLength)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, view)
	return nil
}

func (s *Server) completeDeployment(w http.ResponseWriter, r *http.Request) error {
	view, err := s.Deployments.Complete(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, view)
	return nil
}

func (s *Server) getDeployment(w http.ResponseWriter, r *http.Request) error {
	view, err := s.Deployments.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, view)
	return nil
}

func (s *Server) cancelDeployment(w http.ResponseWriter, r *http.Request) error {
	view, err := s.Deployments.Cancel(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, view)
	return nil
}
