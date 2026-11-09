package api

import (
	"net/http"
	"strconv"

	"github.com/pageforge/pageforge/internal/httpx"
	"github.com/pageforge/pageforge/internal/observability"
)

func (s *Server) listRequestLogs(w http.ResponseWriter, r *http.Request) error {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > 500 {
			return httpx.Invalid("INVALID_REQUEST", "limit must be between 1 and 500")
		}
		limit = n
	}
	entries := []observability.RequestEntry{}
	if s.Recorder != nil {
		entries = s.Recorder.Recent(limit)
	}
	httpx.WriteJSON(w, http.StatusOK, entries)
	return nil
}

func (s *Server) getMetrics(w http.ResponseWriter, _ *http.Request) error {
	metrics := observability.Metrics{}
	if s.Recorder != nil {
		metrics = s.Recorder.Metrics()
	}
	httpx.WriteJSON(w, http.StatusOK, metrics)
	return nil
}
