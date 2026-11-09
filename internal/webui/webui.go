// Package webui serves the embedded PageForge management console.
//
// The console runs in the same process as the control plane and talks to the
// management API on the same origin, so no CORS configuration is required.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed assets/*
var embedded embed.FS

// Handler serves the console and falls back to index.html for unknown paths so
// the single-page app can own its own routing.
func Handler() http.Handler {
	sub, err := fs.Sub(embedded, "assets")
	if err != nil {
		return http.NotFoundHandler()
	}
	files := http.FileServer(http.FS(sub))
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		return http.NotFoundHandler()
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name != "" && name != "." && exists(sub, name) {
			if strings.HasSuffix(name, ".css") || strings.HasSuffix(name, ".js") {
				// 每次回源校验，避免 5 分钟强缓存导致前端改动不生效
				w.Header().Set("Cache-Control", "no-cache")
			}
			files.ServeHTTP(w, r)
			return
		}
		serveIndex(w, r, index)
	})
}

func exists(fsys fs.FS, name string) bool {
	info, err := fs.Stat(fsys, name)
	return err == nil && !info.IsDir()
}

func serveIndex(w http.ResponseWriter, r *http.Request, index []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "index.html", time.Time{}, strings.NewReader(string(index)))
}
