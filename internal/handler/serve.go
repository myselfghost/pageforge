package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/pageforge/pageforge/internal/cache"
	"github.com/pageforge/pageforge/internal/deploy"
	"github.com/pageforge/pageforge/internal/manifest"
	"github.com/pageforge/pageforge/internal/model"
	"github.com/pageforge/pageforge/internal/objectstore"
	"github.com/pageforge/pageforge/internal/snapshot"
)

func (h *Handler) serveEntry(w http.ResponseWriter, r *http.Request, store objectstore.ObjectStore,
	project snapshot.ProjectView, versionID string, binding snapshot.BindingView,
	root *manifest.Root, entry manifest.Entry, status int) {
	ctx := r.Context()

	// Wrap the response writer so the client idle watchdog resets on every
	// write; a stalled client trips it and cancels ctx, aborting upstream work.
	w, stopClient := clientWriter(ctx, w)
	defer stopClient()

	served := entry
	if entry.Encoding == "" {
		if variant, ok := h.selectCompressed(ctx, store, project.Bucket, root, entry, r.Header.Get("Accept-Encoding")); ok {
			served = variant
		}
	}
	etagSHA := entry.SHA256
	if etagSHA == "" {
		etagSHA = served.SHA256
	}

	contentType := served.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if served.Encoding != "" {
		w.Header().Set("Content-Encoding", served.Encoding)
		w.Header().Add("Vary", "Accept-Encoding")
	}
	// Response-header rules (FR-21) take precedence over the defaults below.
	setDefault := func(name, value string) {
		if w.Header().Get(name) == "" {
			w.Header().Set(name, value)
		}
	}
	setDefault("Content-Type", contentType)
	setDefault("ETag", `"`+etagSHA+`"`)
	setDefault("Cache-Control", cacheControl(project.Cache, served.Key))
	setDefault("Accept-Ranges", "bytes")
	var modified time.Time
	if served.ModTime > 0 {
		modified = time.Unix(served.ModTime, 0).UTC()
		setDefault("Last-Modified", modified.UTC().Format(http.TimeFormat))
	}

	if notModified(r, etagSHA, modified) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	objectKey := "projects/" + binding.ProjectID + "/" + versionID + "/" + deploy.StorageKey(served.Key)
	cached := cache.Object{
		AliasID:   project.AliasID,
		Bucket:    project.Bucket,
		ProjectID: binding.ProjectID,
		VersionID: versionID,
		Key:       served.Key,
	}
	if h.cache != nil && h.cache.IsNotFound(cached) {
		w.Header().Set("X-Cache", string(cache.StatusNegative))
		writeError(w, http.StatusNotFound, "NOT_FOUND", "object is missing from storage")
		return
	}

	rangeHeader := r.Header.Get("Range")
	if status == http.StatusOK && rangeHeader != "" && !strings.Contains(rangeHeader, ",") {
		rng, ok := parseRange(rangeHeader, served.Size)
		if !ok {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", served.Size))
			writeError(w, http.StatusRequestedRangeNotSatisfiable, "RANGE_NOT_SATISFIABLE", "requested range is not satisfiable")
			return
		}
		if h.cache != nil {
			if file, size, ok := h.cache.Open(cached); ok {
				defer func() { _ = file.Close() }()
				w.Header().Set("X-Cache", string(cache.StatusHit))
				h.serveRangeFile(w, r, file, size, rng)
				return
			}
		}
		h.serveRangeUpstream(w, r, store, project.Bucket, objectKey, served.Size, rng, cached)
		return
	}

	if h.cache != nil {
		// HEAD is answered from the manifest; it neither downloads nor fills.
		if r.Method == http.MethodHead {
			if file, size, ok := h.cache.Open(cached); ok {
				_ = file.Close()
				w.Header().Set("X-Cache", string(cache.StatusHit))
				w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			} else {
				w.Header().Set("X-Cache", string(cache.StatusMiss))
				w.Header().Set("Content-Length", strconv.FormatInt(served.Size, 10))
			}
			w.WriteHeader(status)
			return
		}
		fetch := func(ctx context.Context) (io.ReadCloser, error) {
			s3ctx, cancel := h.upstreamContext(ctx)
			body, _, err := store.Get(s3ctx, project.Bucket, objectKey, objectstore.GetOptions{})
			if err != nil {
				cancel()
				return nil, err
			}
			// 对象 body 是懒读取的：用空闲看门狗包裹，有字节流动就重置；
			// 长时间无数据则触发取消并中断本次回源。
			return newIdleBody(body, h.opts.UpstreamIdleTimeout, cancel), nil
		}
		reader, size, cacheStatus, err := h.cache.Get(ctx, cached, served.Size, fetch)
		if cacheStatus == cache.StatusNegative {
			w.Header().Set("X-Cache", string(cache.StatusNegative))
			writeError(w, http.StatusNotFound, "NOT_FOUND", "object is missing from storage")
			return
		}
		if err != nil {
			h.objectError(w, err, cached)
			return
		}
		defer func() { _ = reader.Close() }()
		w.Header().Set("X-Cache", string(cacheStatus))
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(status)
		_, _ = io.Copy(w, reader)
		return
	}

	s3ctx, cancel := h.upstreamContext(ctx)
	defer cancel()
	body, _, err := store.Get(s3ctx, project.Bucket, objectKey, objectstore.GetOptions{})
	if err != nil {
		h.objectError(w, err, cached)
		return
	}
	wrapped := newIdleBody(body, h.opts.UpstreamIdleTimeout, cancel)
	defer func() { _ = wrapped.Close() }()
	w.Header().Set("Content-Length", strconv.FormatInt(served.Size, 10))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, wrapped)
	}
}

func (h *Handler) selectCompressed(ctx context.Context, store objectstore.ObjectStore, bucket string,
	root *manifest.Root, entry manifest.Entry, acceptEncoding string) (manifest.Entry, bool) {
	if !compressible(entry.ContentType) {
		return manifest.Entry{}, false
	}
	candidates := []struct {
		suffix   string
		encoding string
	}{
		{".br", "br"},
		{".gz", "gz"},
	}
	for _, candidate := range candidates {
		if !encodingAccepted(acceptEncoding, candidate.encoding) {
			continue
		}
		variant, ok, err := h.manifests.Lookup(ctx, store, bucket, root, entry.Key+candidate.suffix)
		if err != nil || !ok {
			continue
		}
		if variant.Encoding == candidate.encoding {
			return variant, true
		}
	}
	return manifest.Entry{}, false
}

func (h *Handler) serveRangeFile(w http.ResponseWriter, r *http.Request, file *os.File, size int64, rng byteRange) {
	length := rng.end - rng.start + 1
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rng.start, rng.end, size))
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(http.StatusPartialContent)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.CopyN(w, io.NewSectionReader(file, rng.start, length), length)
}

func (h *Handler) serveRangeUpstream(w http.ResponseWriter, r *http.Request, store objectstore.ObjectStore,
	bucket, key string, size int64, rng byteRange, negativeKey cache.Object) {
	ctx, cancel := h.upstreamContext(r.Context())
	defer cancel()
	body, _, err := store.Get(ctx, bucket, key, objectstore.GetOptions{
		Range: fmt.Sprintf("bytes=%d-%d", rng.start, rng.end),
	})
	if err != nil {
		h.objectError(w, err, negativeKey)
		return
	}
	wrapped := newIdleBody(body, h.opts.UpstreamIdleTimeout, cancel)
	defer func() { _ = wrapped.Close() }()
	length := rng.end - rng.start + 1
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rng.start, rng.end, size))
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(http.StatusPartialContent)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, wrapped)
}

func (h *Handler) objectError(w http.ResponseWriter, err error, key cache.Object) {
	if errors.Is(err, objectstore.ErrNotFound) {
		if h.cache != nil {
			h.cache.MarkNotFound(key)
		}
		writeError(w, http.StatusNotFound, "NOT_FOUND", "object is missing from storage")
		return
	}
	h.upstreamError(w, err)
}

// upstreamContext derives the context for a single upstream object fetch.
// The optional total timeout is applied here; the idle watchdog is applied on
// the returned body via newIdleBody.
func (h *Handler) upstreamContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if h.opts.UpstreamTotalTimeout > 0 {
		return context.WithTimeout(ctx, h.opts.UpstreamTotalTimeout)
	}
	return context.WithCancel(ctx)
}

func notModified(r *http.Request, etag string, modified time.Time) bool {
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		for _, candidate := range strings.Split(inm, ",") {
			candidate = strings.TrimSpace(candidate)
			if candidate == "*" || candidate == `"`+etag+`"` {
				return true
			}
		}
		return false
	}
	if ims := r.Header.Get("If-Modified-Since"); ims != "" && !modified.IsZero() {
		if when, err := http.ParseTime(ims); err == nil && !modified.After(when) {
			return true
		}
	}
	return false
}

func cacheControl(policy model.CachePolicy, key string) string {
	html := policy.HTML
	if html == "" {
		html = "no-cache"
	}
	assets := policy.Assets
	if assets == "" {
		assets = "public, max-age=300"
	}
	lower := strings.ToLower(key)
	if strings.HasSuffix(lower, ".html") || strings.HasSuffix(lower, ".htm") {
		return html
	}
	if policy.AssetsImmutableGlob != "" && matchGlob(policy.AssetsImmutableGlob, key) {
		maxAge := policy.ImmutableMaxAge
		if maxAge <= 0 {
			maxAge = 31536000
		}
		return fmt.Sprintf("public, max-age=%d, immutable", maxAge)
	}
	return assets
}

func matchGlob(glob, key string) bool {
	glob = strings.TrimPrefix(glob, "/")
	key = strings.TrimPrefix(key, "/")
	if strings.HasSuffix(glob, "/**") {
		base := strings.TrimSuffix(glob, "/**")
		return key == base || strings.HasPrefix(key, base+"/")
	}
	ok, err := path.Match(glob, key)
	return err == nil && ok
}

func compressible(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	switch {
	case strings.HasPrefix(ct, "text/"):
		return true
	case ct == "application/javascript", ct == "application/json", ct == "application/xml",
		ct == "image/svg+xml", ct == "application/wasm":
		return true
	case strings.HasSuffix(ct, "+json"), strings.HasSuffix(ct, "+xml"):
		return true
	}
	return false
}

func encodingAccepted(header, encoding string) bool {
	for _, part := range strings.Split(header, ",") {
		token := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if strings.EqualFold(token, encoding) || token == "*" {
			return true
		}
	}
	return false
}

type byteRange struct {
	start int64
	end   int64
}

func parseRange(header string, size int64) (byteRange, bool) {
	spec := strings.TrimSpace(strings.TrimPrefix(header, "bytes="))
	if spec == "" || spec == header || strings.Contains(spec, ",") || size <= 0 {
		return byteRange{}, false
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return byteRange{}, false
	}
	startPart := strings.TrimSpace(spec[:dash])
	endPart := strings.TrimSpace(spec[dash+1:])

	if startPart == "" {
		n, err := strconv.ParseInt(endPart, 10, 64)
		if err != nil || n <= 0 {
			return byteRange{}, false
		}
		if n > size {
			n = size
		}
		return byteRange{start: size - n, end: size - 1}, true
	}

	start, err := strconv.ParseInt(startPart, 10, 64)
	if err != nil || start < 0 || start >= size {
		return byteRange{}, false
	}
	end := size - 1
	if endPart != "" {
		end, err = strconv.ParseInt(endPart, 10, 64)
		if err != nil || end < start {
			return byteRange{}, false
		}
		if end >= size {
			end = size - 1
		}
	}
	return byteRange{start: start, end: end}, true
}
