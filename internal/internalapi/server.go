// Package internalapi serves the control-plane endpoints consumed by data-plane
// nodes. It never exposes user-facing administration.
package internalapi

import (
	"bytes"
	"compress/gzip"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/pageforge/pageforge/internal/alias"
	"github.com/pageforge/pageforge/internal/authpolicy"
	"github.com/pageforge/pageforge/internal/httpx"
	"github.com/pageforge/pageforge/internal/share"
	"github.com/pageforge/pageforge/internal/snapshot"
)

// Server wires the internal endpoints.
type Server struct {
	Token        string
	Snapshots    *snapshot.Generator
	Aliases      *alias.Repo
	Shares       *share.Repo
	AuthPolicies *authpolicy.Repo
	Logger       *slog.Logger
}

// Handler returns the internal mux guarded by the internal token.
func (s *Server) Handler() http.Handler {
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	if s.Token == "" {
		logger.Warn("internal_token is empty; the internal API is unauthenticated")
	}
	mux.Handle("GET /internal/v1/snapshot", s.authorize(http.HandlerFunc(s.getSnapshot)))
	mux.Handle("POST /internal/v1/reload", s.authorize(http.HandlerFunc(s.reload)))
	mux.Handle("GET /internal/v1/aliases/{id}/credentials", s.authorize(http.HandlerFunc(s.credentials)))
	mux.Handle("GET /internal/v1/auth-policies", s.authorize(http.HandlerFunc(s.authPolicies)))
	mux.Handle("POST /internal/v1/share-links/{token}/authorize", s.authorize(http.HandlerFunc(s.authorizeShare)))
	return mux
}

// shareAuthorizeRequest carries the optional password and whether this request
// counts as a share view (page requests only, not assets).
type shareAuthorizeRequest struct {
	Password  string `json:"password"`
	CountView bool   `json:"count_view"`
}

func (s *Server) authorizeShare(w http.ResponseWriter, r *http.Request) {
	if s.Shares == nil {
		writeError(w, http.StatusNotImplemented, "SHARE_LINKS_UNAVAILABLE", "share links are not configured")
		return
	}
	var req shareAuthorizeRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", "invalid JSON body")
		return
	}
	link, err := s.Shares.Authorize(r.Context(), r.PathValue("token"), req.Password, req.CountView)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, link)
}

// writeServiceError maps domain errors onto the shared error envelope.
func writeServiceError(w http.ResponseWriter, err error) {
	var apiErr *httpx.Error
	if errors.As(err, &apiErr) {
		writeError(w, apiErr.Status, apiErr.Code, apiErr.Message)
		return
	}
	writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
}

func (s *Server) authorize(next http.Handler) http.Handler {
	if s.Token == "" {
		return next
	}
	token := []byte(s.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := r.Header.Get("X-Internal-Token")
		if provided == "" {
			provided, _ = strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if subtle.ConstantTimeCompare([]byte(provided), token) != 1 {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid internal token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// LoggerOr returns the configured logger.
func (s *Server) LoggerOr() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func (s *Server) getSnapshot(w http.ResponseWriter, r *http.Request) {
	snap, body, err := s.Snapshots.Current(r.Context())
	if err != nil {
		s.LoggerOr().Error("generate snapshot", slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "SNAPSHOT_FAILED", "failed to generate snapshot")
		return
	}
	etag := quoteGeneration(snap.Generation)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Add("Vary", "Accept-Encoding")
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	if acceptsGzip(r.Header.Get("Accept-Encoding")) {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		if _, err := gz.Write(body); err != nil {
			s.LoggerOr().Error("compress snapshot", slog.String("error", err.Error()))
			writeError(w, http.StatusInternalServerError, "SNAPSHOT_FAILED", "failed to encode snapshot")
			return
		}
		if err := gz.Close(); err != nil {
			s.LoggerOr().Error("compress snapshot", slog.String("error", err.Error()))
			writeError(w, http.StatusInternalServerError, "SNAPSHOT_FAILED", "failed to encode snapshot")
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(buf.Bytes())
		return
	}

	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (s *Server) reload(w http.ResponseWriter, r *http.Request) {
	snap, _, err := s.Snapshots.Reload(r.Context())
	if err != nil {
		s.LoggerOr().Error("reload snapshot", slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "SNAPSHOT_FAILED", "failed to regenerate snapshot")
		return
	}
	writeJSON(w, http.StatusOK, map[string]uint64{"generation": snap.Generation})
}

// credentialsResponse must never be logged: it carries plaintext secrets.
type credentialsResponse struct {
	AccessKey     string `json:"access_key"`
	SecretKey     string `json:"secret_key"`
	CredentialVer int64  `json:"credential_version"`
}

func (s *Server) authPolicies(w http.ResponseWriter, r *http.Request) {
	if s.AuthPolicies == nil {
		writeError(w, http.StatusServiceUnavailable, "AUTH_UNAVAILABLE", "auth policies unavailable")
		return
	}
	dir, err := s.AuthPolicies.Directory(r.Context())
	if err != nil {
		s.LoggerOr().Error("auth directory failed", slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "auth directory failed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(dir)
}

func (s *Server) credentials(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, err := s.Aliases.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "ALIAS_NOT_FOUND", "alias not found")
		return
	}
	_, secret, err := s.Aliases.Secret(r.Context(), id)
	if err != nil {
		s.LoggerOr().Error("decrypt alias credentials", slog.String("alias_id", id), slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "CREDENTIALS_FAILED", "failed to load credentials")
		return
	}
	writeJSON(w, http.StatusOK, credentialsResponse{
		AccessKey:     a.AccessKey,
		SecretKey:     secret,
		CredentialVer: a.CredentialVer,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}

func quoteGeneration(generation uint64) string {
	return `"` + strconv.FormatUint(generation, 10) + `"`
}

func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag {
			return true
		}
	}
	return false
}

func acceptsGzip(header string) bool {
	for _, token := range strings.Split(header, ",") {
		name := strings.TrimSpace(strings.SplitN(token, ";", 2)[0])
		if strings.EqualFold(name, "gzip") {
			return true
		}
	}
	return false
}
