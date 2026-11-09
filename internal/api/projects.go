package api

import (
	"net/http"

	"github.com/pageforge/pageforge/internal/httpx"
	"github.com/pageforge/pageforge/internal/project"
)

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) error {
	items, err := s.Projects.List(r.Context())
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, items)
	return nil
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) error {
	var in project.CreateInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	created, err := s.Projects.Create(r.Context(), in)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusCreated, created)
	return nil
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) error {
	found, err := s.Projects.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, found)
	return nil
}

func (s *Server) updateProject(w http.ResponseWriter, r *http.Request) error {
	var in project.UpdateInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	updated, err := s.Projects.Update(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, updated)
	return nil
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) error {
	if err := s.Projects.Delete(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) listVersions(w http.ResponseWriter, r *http.Request) error {
	projectID := r.PathValue("id")
	if _, err := s.Projects.Get(r.Context(), projectID); err != nil {
		return err
	}
	items, err := s.Versions.List(r.Context(), projectID)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, items)
	return nil
}

func (s *Server) getVersion(w http.ResponseWriter, r *http.Request) error {
	found, err := s.Versions.Get(r.Context(), r.PathValue("id"), r.PathValue("vid"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, found)
	return nil
}

func (s *Server) deleteVersion(w http.ResponseWriter, r *http.Request) error {
	if err := s.Versions.Delete(r.Context(), r.PathValue("id"), r.PathValue("vid")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

type pointerRequest struct {
	VersionID              string `json:"version_id"`
	ExpectedCurrentVersion string `json:"expected_current_version_id"`
}

func (s *Server) setPointer(w http.ResponseWriter, r *http.Request) error {
	var req pointerRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	if req.VersionID == "" {
		return httpx.Invalid("INVALID_REQUEST", "version_id is required")
	}
	updated, err := s.Versions.SetPointer(r.Context(), r.PathValue("id"), req.VersionID, req.ExpectedCurrentVersion)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, updated)
	return nil
}

type protectRequest struct {
	Protected *bool `json:"protected"`
}

func (s *Server) setProtection(w http.ResponseWriter, r *http.Request) error {
	var req protectRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	if req.Protected == nil {
		return httpx.Invalid("INVALID_REQUEST", "protected is required")
	}
	updated, err := s.Versions.SetProtected(r.Context(), r.PathValue("id"), r.PathValue("vid"), *req.Protected)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, updated)
	return nil
}
