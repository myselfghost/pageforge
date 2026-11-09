// Package api exposes the control-plane HTTP API.
package api

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"

	"github.com/pageforge/pageforge/internal/alias"
	"github.com/pageforge/pageforge/internal/authpolicy"
	"github.com/pageforge/pageforge/internal/binding"
	"github.com/pageforge/pageforge/internal/deploy"
	"github.com/pageforge/pageforge/internal/httpx"
	"github.com/pageforge/pageforge/internal/objectstore"
	"github.com/pageforge/pageforge/internal/observability"
	"github.com/pageforge/pageforge/internal/project"
	"github.com/pageforge/pageforge/internal/share"
	"github.com/pageforge/pageforge/internal/version"
)

// ConnectionTester verifies alias credentials against a bucket.
type ConnectionTester func(ctx context.Context, a alias.Alias, secret, bucket string) error

// Server wires repositories and HTTP handlers.
type Server struct {
	Aliases      *alias.Repo
	Projects     *project.Repo
	Bindings     *binding.Repo
	AuthPolicies *authpolicy.Repo
	Versions     *version.Repo
	Shares       *share.Repo
	Deployments  *deploy.Service
	AdminToken   string
	AliasTester  ConnectionTester
	// Stores resolves the object store for a project (FR-20 package download).
	Stores        func(context.Context, project.Project) (objectstore.ObjectStore, error)
	PackagePrefix string
	Recorder      *observability.Recorder
	Logger        *slog.Logger
}

// Handler returns the admin API handler including bearer authentication.
func (s *Server) Handler() http.Handler {
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)

	mux.HandleFunc("GET /api/v1/aliases", s.handle(s.listAliases))
	mux.HandleFunc("POST /api/v1/aliases", s.handle(s.createAlias))
	mux.HandleFunc("GET /api/v1/aliases/{id}", s.handle(s.getAlias))
	mux.HandleFunc("PATCH /api/v1/aliases/{id}", s.handle(s.updateAlias))
	mux.HandleFunc("DELETE /api/v1/aliases/{id}", s.handle(s.deleteAlias))
	mux.HandleFunc("POST /api/v1/aliases/{id}/test", s.handle(s.testAlias))

	mux.HandleFunc("GET /api/v1/projects", s.handle(s.listProjects))
	mux.HandleFunc("POST /api/v1/projects", s.handle(s.createProject))
	mux.HandleFunc("GET /api/v1/projects/{id}", s.handle(s.getProject))
	mux.HandleFunc("PATCH /api/v1/projects/{id}", s.handle(s.updateProject))
	mux.HandleFunc("DELETE /api/v1/projects/{id}", s.handle(s.deleteProject))
	mux.HandleFunc("GET /api/v1/projects/{id}/versions", s.handle(s.listVersions))
	mux.HandleFunc("GET /api/v1/projects/{id}/versions/{vid}", s.handle(s.getVersion))
	mux.HandleFunc("DELETE /api/v1/projects/{id}/versions/{vid}", s.handle(s.deleteVersion))
	mux.HandleFunc("GET /api/v1/projects/{id}/versions/{vid}/package", s.handle(s.downloadPackage))
	mux.HandleFunc("POST /api/v1/projects/{id}/pointer", s.handle(s.setPointer))
	mux.HandleFunc("POST /api/v1/projects/{id}/versions/{vid}/protect", s.handle(s.setProtection))

	mux.HandleFunc("GET /api/v1/bindings", s.handle(s.listBindings))
	mux.HandleFunc("POST /api/v1/bindings", s.handle(s.createBinding))
	mux.HandleFunc("POST /api/v1/bindings/validate", s.handle(s.validateBinding))
	mux.HandleFunc("GET /api/v1/bindings/{id}", s.handle(s.getBinding))
	mux.HandleFunc("PATCH /api/v1/bindings/{id}", s.handle(s.updateBinding))
	mux.HandleFunc("DELETE /api/v1/bindings/{id}", s.handle(s.deleteBinding))

	mux.HandleFunc("GET /api/v1/auth-policies", s.handle(s.listAuthPolicies))
	mux.HandleFunc("POST /api/v1/auth-policies", s.handle(s.createAuthPolicy))
	mux.HandleFunc("GET /api/v1/auth-policies/{id}", s.handle(s.getAuthPolicy))
	mux.HandleFunc("PATCH /api/v1/auth-policies/{id}", s.handle(s.updateAuthPolicy))
	mux.HandleFunc("DELETE /api/v1/auth-policies/{id}", s.handle(s.deleteAuthPolicy))
	mux.HandleFunc("PUT /api/v1/auth-policies/{id}/users/{username}", s.handle(s.upsertAuthUser))
	mux.HandleFunc("DELETE /api/v1/auth-policies/{id}/users/{username}", s.handle(s.deleteAuthUser))

	mux.HandleFunc("GET /api/v1/deployments", s.handle(s.listDeployments))
	mux.HandleFunc("POST /api/v1/deployments", s.handle(s.createDeployment))
	mux.HandleFunc("PUT /api/v1/deployments/{id}/package", s.handle(s.uploadDeploymentPackage))
	mux.HandleFunc("POST /api/v1/deployments/{id}/complete", s.handle(s.completeDeployment))
	mux.HandleFunc("GET /api/v1/deployments/{id}", s.handle(s.getDeployment))
	mux.HandleFunc("POST /api/v1/deployments/{id}/cancel", s.handle(s.cancelDeployment))

	mux.HandleFunc("GET /api/v1/logs", s.handle(s.listRequestLogs))
	mux.HandleFunc("GET /api/v1/metrics", s.handle(s.getMetrics))

	mux.HandleFunc("GET /api/v1/share-links", s.handle(s.listShareLinks))
	mux.HandleFunc("POST /api/v1/share-links", s.handle(s.createShareLink))
	mux.HandleFunc("GET /api/v1/share-links/{id}", s.handle(s.getShareLink))
	mux.HandleFunc("PATCH /api/v1/share-links/{id}", s.handle(s.updateShareLink))
	mux.HandleFunc("DELETE /api/v1/share-links/{id}", s.handle(s.deleteShareLink))

	return s.withAuth(mux)
}

func (s *Server) withAuth(next http.Handler) http.Handler {
	if s.AdminToken == "" {
		s.Logger.Warn("admin_token is empty; the control-plane API is unauthenticated")
		return next
	}
	token := []byte(s.AdminToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		provided, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(provided), token) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			httpx.WriteError(w, r, &httpx.Error{
				Status: http.StatusUnauthorized, Code: "UNAUTHORIZED", Message: "invalid admin token",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handle(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			httpx.WriteError(w, r, err)
		}
	}
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
