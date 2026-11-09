package handler

import (
	"encoding/json"
	"net/http"
)

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Del("Content-Encoding")
	w.Header().Del("Content-Length")
	w.Header().Del("ETag")
	w.Header().Del("Last-Modified")
	w.Header().Del("Accept-Ranges")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}
