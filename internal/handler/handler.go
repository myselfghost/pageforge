// Package handler implements the data-plane request lifecycle: routing,
// manifest resolution, caching and HTTP semantics.
package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pageforge/pageforge/internal/cache"
	"github.com/pageforge/pageforge/internal/httpx"
	"github.com/pageforge/pageforge/internal/manifest"
	"github.com/pageforge/pageforge/internal/model"
	"github.com/pageforge/pageforge/internal/objectstore"
	"github.com/pageforge/pageforge/internal/router"
	"github.com/pageforge/pageforge/internal/snapshot"
)

// StoreProvider resolves the object store used by a project's alias.
type StoreProvider interface {
	Store(ctx context.Context, aliasID string, view snapshot.AliasView) (objectstore.ObjectStore, error)
}

// Options wires the handler dependencies.
type Options struct {
	Snapshots      *snapshot.Store
	Stores         StoreProvider
	Manifests      *manifest.Reader
	Cache          *cache.Cache
	ReservedPrefix string
	ControlURL     string
	InternalToken  string
	Logger         *slog.Logger

	// ClientIdleTimeout is the maximum idle time (no bytes transferred) between
	// the proxy and the client (nginx: send_timeout). A slow but steady
	// download never trips it; only a stalled peer does.
	ClientIdleTimeout time.Duration
	// ClientTotalTimeout is an optional hard ceiling on the whole client-facing
	// request (0 disables it). It exists only to bound runaway transfers.
	ClientTotalTimeout time.Duration

	// UpstreamIdleTimeout is the maximum idle time (no bytes) while reading the
	// object body from storage (nginx: proxy_read_timeout, body phase). A long
	// but flowing download keeps resetting it.
	UpstreamIdleTimeout time.Duration
	// UpstreamTotalTimeout is an optional hard ceiling on a single upstream
	// object fetch (0 disables it).
	UpstreamTotalTimeout time.Duration

	// ApplyResponseHeaders is the M4 hook for project/binding header rules.
	ApplyResponseHeaders func(w http.ResponseWriter, project snapshot.ProjectView, binding snapshot.BindingView)

	// ShareAuth authorizes share-link requests (FR-18). When nil and
	// ControlURL is set, a control-plane client is used.
	ShareAuth ShareAuthorizer
}

// Handler serves static-site requests from S3 using the active snapshot.
type Handler struct {
	opts      Options
	logger    *slog.Logger
	snapshots *snapshot.Store
	stores    StoreProvider
	manifests *manifest.Reader
	cache     *cache.Cache
	proxy     *httputil.ReverseProxy
	shareAuth ShareAuthorizer
	authCache *basicAuthCache

	routing atomic.Pointer[compiledRouter]
}

type compiledRouter struct {
	generation uint64
	table      *router.Table
}

// New builds a data-plane handler.
func New(opts Options) *Handler {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if opts.ReservedPrefix == "" {
		opts.ReservedPrefix = "/__pageforge"
	}
	opts.ReservedPrefix = strings.TrimSuffix(opts.ReservedPrefix, "/")
	if opts.Manifests == nil {
		opts.Manifests = manifest.NewReader()
	}
	h := &Handler{
		opts:      opts,
		logger:    logger,
		snapshots: opts.Snapshots,
		stores:    opts.Stores,
		manifests: opts.Manifests,
		cache:     opts.Cache,
		shareAuth: opts.ShareAuth,
		authCache: newBasicAuthCache(),
	}
	h.proxy = buildProxy(opts.ControlURL, opts.InternalToken, opts.ReservedPrefix)
	if h.shareAuth == nil && opts.ControlURL != "" {
		h.shareAuth = newControlShareAuthorizer(opts.ControlURL, opts.InternalToken)
	}
	return h
}

func buildProxy(controlURL, token, reservedPrefix string) *httputil.ReverseProxy {
	if controlURL == "" {
		return nil
	}
	target, err := url.Parse(controlURL)
	if err != nil || target.Host == "" {
		return nil
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	inner := proxy.Director
	proxy.Director = func(r *http.Request) {
		inner(r)
		r.URL.Path = strings.TrimPrefix(r.URL.Path, reservedPrefix)
		if r.URL.Path == "" {
			r.URL.Path = "/"
		}
		r.Host = target.Host
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("X-Internal-Token", token)
		}
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Default().Error("internal api proxy failed",
			slog.String("path", r.URL.Path), slog.String("error", err.Error()))
		writeError(w, http.StatusBadGateway, "UPSTREAM_ERROR", "control plane is unavailable")
	}
	return proxy
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !(r.Method == http.MethodPost && h.isSharePath(r.URL.Path)) {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "only GET and HEAD are supported")
		return
	}

	ctx := r.Context()
	var cancel context.CancelFunc
	// Client-facing timeout: an idle watchdog on the response writer, with an
	// optional hard ceiling. Both are expressed through the request context so
	// the upstream body copy is aborted when they fire.
	ctx, cancel = h.clientTimeout(ctx)
	defer cancel()
	r = r.WithContext(ctx)

	if h.isReserved(r.URL.Path) {
		h.serveReserved(w, r)
		return
	}

	host, err := httpx.NormalizeHost(r.Host)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_HOST", "invalid host header")
		return
	}
	// 必须传入转义后的路径：net/http 已把 %2F/%5C 解码进 URL.Path，只有
	// EscapedPath() 仍保留原始百分号编码，NormalizeKey 才能拦截编码斜杠穿越。
	key, err := httpx.NormalizeKey(r.URL.EscapedPath())
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_PATH", "invalid request path")
		return
	}

	snap := h.snapshots.Load()
	if snap == nil {
		writeError(w, http.StatusServiceUnavailable, "SNAPSHOT_UNAVAILABLE", "routing table is not loaded")
		return
	}

	match, ok := h.table(snap).Match(host, key)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no route matches this host and path")
		return
	}
	if h.isReserved(match.Remainder) {
		writeError(w, http.StatusNotFound, "RESERVED_PATH", "path crosses the reserved prefix")
		return
	}
	if !h.authorizeBinding(w, r, snap, match.Binding) {
		return
	}

	h.serveProject(w, r, snap, serveParams{
		projectID: match.Binding.ProjectID,
		versionID: match.Binding.PinnedVersionID,
		binding:   match.Binding,
		remainder: match.Remainder,
	})
}

// serveParams describes one project render, either through a binding or through
// a version direct link / share link.
type serveParams struct {
	projectID string
	versionID string
	binding   snapshot.BindingView
	remainder string
	// cache overrides the project cache policy when set (share links carry
	// their own policy, FR-18).
	cache *model.CachePolicy
}

func (h *Handler) serveProject(w http.ResponseWriter, r *http.Request, snap *snapshot.Snapshot, params serveParams) {
	ctx := r.Context()
	project, ok := snap.Projects[params.projectID]
	if !ok {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "project is not available")
		return
	}
	if project.Status == model.ProjectDisabled {
		writeError(w, http.StatusServiceUnavailable, "PROJECT_DISABLED", "project is disabled")
		return
	}

	versionID := params.versionID
	if versionID == "" {
		versionID = project.CurrentVersionID
	}
	if versionID == "" {
		writeError(w, http.StatusServiceUnavailable, "VERSION_NOT_AVAILABLE", "project has no published version")
		return
	}
	if params.cache != nil {
		project.Cache = params.cache.WithDefaults()
	}

	h.applyResponseHeaders(w, r, project, params.binding, versionID)

	// 绑定挂载根不带尾斜杠时先补 301：浏览器以当前 URL 作为相对路径基准，
	// 缺少尾斜杠会把 ./assets/app.css 解析到上一级（如 /assets/app.css）。
	// 该规范化与 try_files 开关无关，保证挂在子路径下的站点相对引用可用。
	if params.binding.ID != "" && params.remainder == "" && !strings.HasSuffix(r.URL.Path, "/") {
		location := r.URL.EscapedPath() + "/"
		if r.URL.RawQuery != "" {
			location += "?" + r.URL.RawQuery
		}
		w.Header().Set("Location", location)
		w.WriteHeader(http.StatusMovedPermanently)
		return
	}

	aliasView, ok := snap.Aliases[project.AliasID]
	if !ok {
		writeError(w, http.StatusBadGateway, "ALIAS_UNAVAILABLE", "project alias is not available")
		return
	}
	store, err := h.stores.Store(ctx, project.AliasID, aliasView)
	if err != nil {
		h.logger.Error("resolve object store",
			slog.String("alias_id", project.AliasID), slog.String("error", err.Error()))
		writeError(w, http.StatusBadGateway, "UPSTREAM_ERROR", "object storage is unavailable")
		return
	}

	root, err := h.manifests.Root(ctx, store, project.Bucket, params.projectID, versionID)
	if err != nil {
		if errors.Is(err, objectstore.ErrNotFound) {
			writeError(w, http.StatusNotFound, "VERSION_NOT_AVAILABLE", "version manifest is missing")
			return
		}
		h.logger.Error("load manifest root",
			slog.String("project_id", params.projectID),
			slog.String("version_id", versionID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusBadGateway, "UPSTREAM_ERROR", "failed to load manifest")
		return
	}

	// project 显式配置 > 发布包配置（固化在 manifest root）> 系统默认。
	// 索引文档的系统默认值即 index.html，显式配置等于默认值时仍让位给发布包。
	indexDoc := effectiveIndexDoc(project.IndexDoc, root.IndexDoc)
	errorDoc := effectiveErrorDoc(project.ErrorDoc, root.ErrorDoc)

	res, err := h.resolve(ctx, r, store, project, root, indexDoc, params.remainder)
	if err != nil {
		h.upstreamError(w, err)
		return
	}
	if res.redirect != "" {
		location := res.redirect
		if r.URL.RawQuery != "" {
			location += "?" + r.URL.RawQuery
		}
		w.Header().Set("Location", location)
		w.WriteHeader(http.StatusMovedPermanently)
		return
	}
	if !res.found {
		if errorDoc != "" {
			if entry, ok, err := h.manifests.Lookup(ctx, store, project.Bucket, root, errorDoc); err != nil {
				h.upstreamError(w, err)
				return
			} else if ok {
				h.serveEntry(w, r, store, project, versionID, params.binding, root, entry, http.StatusNotFound)
				return
			}
		}
		writeError(w, http.StatusNotFound, "NOT_FOUND", "file not found")
		return
	}

	h.serveEntry(w, r, store, project, versionID, params.binding, root, res.entry, http.StatusOK)
}

func (h *Handler) table(snap *snapshot.Snapshot) *router.Table {
	if compiled := h.routing.Load(); compiled != nil && compiled.generation == snap.Generation {
		return compiled.table
	}
	table := router.Build(snap)
	h.routing.Store(&compiledRouter{generation: snap.Generation, table: table})
	return table
}

func (h *Handler) upstreamError(w http.ResponseWriter, err error) {
	h.logger.Error("object storage request failed", slog.String("error", err.Error()))
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(w, http.StatusGatewayTimeout, "UPSTREAM_TIMEOUT", "object storage timed out")
		return
	}
	writeError(w, http.StatusBadGateway, "UPSTREAM_ERROR", "object storage failed")
}

func effectiveIndexDoc(projectDoc, rootDoc string) string {
	if projectDoc == "" || projectDoc == "index.html" {
		if rootDoc != "" {
			return rootDoc
		}
	}
	if projectDoc == "" {
		return "index.html"
	}
	return projectDoc
}

// effectiveErrorDoc 以 project 的实时配置为准，project 未配置时才回退到发布包里的取值。
func effectiveErrorDoc(projectDoc, rootDoc string) string {
	if projectDoc != "" {
		return projectDoc
	}
	return rootDoc
}
