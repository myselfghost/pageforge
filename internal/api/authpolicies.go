package api

import (
	"net/http"

	"github.com/pageforge/pageforge/internal/authpolicy"
	"github.com/pageforge/pageforge/internal/httpx"
)

func (s *Server) listAuthPolicies(w http.ResponseWriter, r *http.Request) error {
	items, err := s.AuthPolicies.List(r.Context())
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, items)
	return nil
}

func (s *Server) createAuthPolicy(w http.ResponseWriter, r *http.Request) error {
	var in authpolicy.CreateInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	created, err := s.AuthPolicies.Create(r.Context(), in)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusCreated, created)
	return nil
}

func (s *Server) getAuthPolicy(w http.ResponseWriter, r *http.Request) error {
	found, err := s.AuthPolicies.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, found)
	return nil
}

func (s *Server) updateAuthPolicy(w http.ResponseWriter, r *http.Request) error {
	var in authpolicy.UpdateInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	updated, err := s.AuthPolicies.Update(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, updated)
	return nil
}

func (s *Server) deleteAuthPolicy(w http.ResponseWriter, r *http.Request) error {
	if err := s.AuthPolicies.Delete(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) upsertAuthUser(w http.ResponseWriter, r *http.Request) error {
	var in authpolicy.UserInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	user, err := s.AuthPolicies.UpsertUser(r.Context(), r.PathValue("id"), r.PathValue("username"), in)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, user)
	return nil
}

func (s *Server) deleteAuthUser(w http.ResponseWriter, r *http.Request) error {
	if err := s.AuthPolicies.DeleteUser(r.Context(), r.PathValue("id"), r.PathValue("username")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
