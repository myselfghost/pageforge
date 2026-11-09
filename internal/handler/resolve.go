package handler

import (
	"context"
	"net/http"
	"path"
	"strings"

	"github.com/pageforge/pageforge/internal/manifest"
	"github.com/pageforge/pageforge/internal/model"
	"github.com/pageforge/pageforge/internal/objectstore"
	"github.com/pageforge/pageforge/internal/snapshot"
)

type resolution struct {
	entry    manifest.Entry
	found    bool
	redirect string
}

func (h *Handler) resolve(ctx context.Context, r *http.Request, store objectstore.ObjectStore,
	project snapshot.ProjectView, root *manifest.Root, indexDoc, rel string) (resolution, error) {
	if project.TryFilesEnabled {
		return h.tryFiles(ctx, r, store, project, root, indexDoc, rel)
	}
	return h.exactLookup(ctx, store, project, root, indexDoc, rel)
}

// exactLookup implements 11.1: only a bare project root falls back to index_doc.
func (h *Handler) exactLookup(ctx context.Context, store objectstore.ObjectStore,
	project snapshot.ProjectView, root *manifest.Root, indexDoc, rel string) (resolution, error) {
	if rel == "" {
		entry, ok, err := h.manifests.Lookup(ctx, store, project.Bucket, root, indexDoc)
		if err != nil {
			return resolution{}, err
		}
		return resolution{entry: entry, found: ok}, nil
	}
	entry, ok, err := h.manifests.Lookup(ctx, store, project.Bucket, root, rel)
	if err != nil {
		return resolution{}, err
	}
	return resolution{entry: entry, found: ok}, nil
}

// tryFiles implements 11.2.
func (h *Handler) tryFiles(ctx context.Context, r *http.Request, store objectstore.ObjectStore,
	project snapshot.ProjectView, root *manifest.Root, indexDoc, rel string) (resolution, error) {
	if rel == "" || strings.HasSuffix(rel, "/") {
		entry, ok, err := h.manifests.Lookup(ctx, store, project.Bucket, root, rel+indexDoc)
		if err != nil {
			return resolution{}, err
		}
		return resolution{entry: entry, found: ok}, nil
	}

	if entry, ok, err := h.manifests.Lookup(ctx, store, project.Bucket, root, rel); err != nil {
		return resolution{}, err
	} else if ok {
		return resolution{entry: entry, found: true}, nil
	}

	hasDir, err := h.manifests.HasDir(ctx, store, project.Bucket, root, rel)
	if err != nil {
		return resolution{}, err
	}
	if hasDir {
		if !strings.HasSuffix(r.URL.Path, "/") {
			// Use the escaped path so non-ASCII (e.g. Chinese) directory names
			// produce a standards-compliant percent-encoded Location header.
			return resolution{redirect: r.URL.EscapedPath() + "/"}, nil
		}
		entry, ok, err := h.manifests.Lookup(ctx, store, project.Bucket, root, rel+"/"+indexDoc)
		if err != nil {
			return resolution{}, err
		}
		if ok {
			return resolution{entry: entry, found: true}, nil
		}
	}

	if project.SPA.Enabled && isNavigationRequest(r, rel, project.SPA) {
		entry, ok, err := h.manifests.Lookup(ctx, store, project.Bucket, root, indexDoc)
		if err != nil {
			return resolution{}, err
		}
		if ok {
			return resolution{entry: entry, found: true}, nil
		}
	}
	return resolution{}, nil
}

var defaultSPAExtensions = []string{
	"js", "mjs", "cjs", "css", "map", "json", "xml", "txt", "png", "jpg", "jpeg", "gif",
	"webp", "avif", "svg", "ico", "bmp", "woff", "woff2", "ttf", "otf", "eot", "mp4",
	"webm", "mov", "mp3", "wav", "ogg", "pdf", "zip", "gz", "br", "tar", "wasm",
}

var defaultSPAPrefixes = []string{"/api", "/assets", "/static", "/media", "/_next", "/_nuxt"}

// isNavigationRequest implements 11.2 SPA detection.
func isNavigationRequest(r *http.Request, rel string, cfg model.SPAConfig) bool {
	if !cfg.Enabled {
		return false
	}
	accept := r.Header.Get("Accept")
	mode := r.Header.Get("Sec-Fetch-Mode")
	if !strings.Contains(strings.ToLower(accept), "text/html") && mode != "navigate" {
		return false
	}

	extensions := cfg.ExcludeExtensions
	if len(extensions) == 0 {
		extensions = defaultSPAExtensions
	}
	if matchExtension(rel, extensions) {
		return false
	}

	prefixes := cfg.ExcludePrefixes
	if len(prefixes) == 0 {
		prefixes = defaultSPAPrefixes
	}
	return !matchesPrefix(rel, prefixes)
}

func matchesPrefix(rel string, prefixes []string) bool {
	for _, raw := range prefixes {
		prefix := strings.Trim(raw, "/")
		if prefix == "" {
			continue
		}
		if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
			return true
		}
	}
	return false
}

func matchExtension(name string, extensions []string) bool {
	ext := strings.TrimPrefix(strings.ToLower(path.Ext(name)), ".")
	if ext == "" {
		return false
	}
	for _, candidate := range extensions {
		if ext == strings.TrimPrefix(strings.ToLower(candidate), ".") {
			return true
		}
	}
	return false
}
