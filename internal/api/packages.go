package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/pageforge/pageforge/internal/httpx"
	"github.com/pageforge/pageforge/internal/objectstore"
	"github.com/pageforge/pageforge/internal/version"
)

// downloadPackage streams the original archive kept for a ready version
// (FR-20). It returns PACKAGE_NOT_AVAILABLE when retention is disabled, the
// version is not ready, or the object has been garbage-collected. The storage
// key suffix is probed for each supported format (.zip, .tar, .tar.gz).
func (s *Server) downloadPackage(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	projectID := r.PathValue("id")
	versionID := r.PathValue("vid")

	proj, err := s.Projects.Get(ctx, projectID)
	if err != nil {
		return err
	}
	found, err := s.Versions.Get(ctx, projectID, versionID)
	if err != nil {
		return err
	}
	if found.Status != version.StatusReady || !proj.PackageRetention {
		return httpx.NotFound("PACKAGE_NOT_AVAILABLE", "original package is not available")
	}
	if s.Stores == nil {
		return &httpx.Error{Status: http.StatusServiceUnavailable, Code: "STORAGE_UNAVAILABLE", Message: "object store is not configured"}
	}
	store, err := s.Stores(ctx, proj)
	if err != nil {
		return &httpx.Error{Status: http.StatusBadGateway, Code: "ALIAS_UNAVAILABLE", Message: "cannot resolve project object store"}
	}

	prefix := s.PackagePrefix
	if prefix == "" {
		prefix = "_system/packages"
	}
	// Try each supported format suffix to find the stored package.
	baseKey := fmt.Sprintf("%s/%s/%s", prefix, projectID, versionID)
	// Suffixes ordered by likelihood: zip is the most common, then tgz, then tar.
	suffixes := []struct {
		suffix      string
		contentType string
	}{
		{".zip", "application/zip"},
		{".tar.gz", "application/gzip"},
		{".tar", "application/x-tar"},
	}

	var foundKey string
	var foundContentType string
	var foundSize int64
	for _, sfx := range suffixes {
		key := baseKey + sfx.suffix
		info, err := store.Stat(ctx, proj.Bucket, key)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return &httpx.Error{Status: http.StatusBadGateway, Code: "STORAGE_ERROR", Message: "cannot stat package"}
		}
		foundKey = key
		foundContentType = sfx.contentType
		foundSize = info.Size
		break
	}
	if foundKey == "" {
		return httpx.NotFound("PACKAGE_NOT_AVAILABLE", "original package is not available")
	}

	body, _, err := store.Get(ctx, proj.Bucket, foundKey, objectstore.GetOptions{})
	if err != nil {
		if isNotFound(err) {
			return httpx.NotFound("PACKAGE_NOT_AVAILABLE", "original package is not available")
		}
		return &httpx.Error{Status: http.StatusBadGateway, Code: "STORAGE_ERROR", Message: "cannot read package"}
	}
	defer body.Close()

	// Derive a friendly filename from the actual key suffix.
	filename := fmt.Sprintf("%s-%s%s", projectID, versionID, foundKey[len(baseKey):])

	w.Header().Set("Content-Type", foundContentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	if foundSize > 0 {
		w.Header().Set("Content-Length", fmt.Sprint(foundSize))
	}
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return nil
	}
	_, _ = io.Copy(w, body)
	return nil
}

func isNotFound(err error) bool {
	return errors.Is(err, objectstore.ErrNotFound)
}
