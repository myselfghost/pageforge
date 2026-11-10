package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/pageforge/pageforge/internal/httpx"
	"github.com/pageforge/pageforge/internal/model"
	"github.com/pageforge/pageforge/internal/snapshot"
)

const (
	versionDirectPath   = "/v/"
	sharePath           = "/s/"
	sharePasswordCookie = "pf_share_pw"
)

// ShareGrant is the control-plane decision for a share-link request.
type ShareGrant struct {
	ProjectID string            `json:"project_id"`
	VersionID string            `json:"version_id"`
	Cache     model.CachePolicy `json:"cache"`
}

// ShareAuthError carries the control-plane status and stable error code so the
// data plane can reproduce it.
type ShareAuthError struct {
	Status  int
	Code    string
	Message string
}

func (e *ShareAuthError) Error() string {
	return fmt.Sprintf("share authorization failed: %s", e.Code)
}

// ShareAuthorizer validates share tokens, passwords, expiry and view budgets.
type ShareAuthorizer interface {
	Authorize(ctx context.Context, token, password string, countView bool) (ShareGrant, error)
}

// shareDirectorySource is the share-link directory the poller keeps in memory.
type shareDirectorySource interface {
	Shares() *snapshot.ShareDirectory
}

// localShareAuthorizer validates share links from the mirrored directory, so an
// offline data plane keeps serving. View counting is deliberately conservative:
// a locally counted view is only released once an updated directory shows the
// control plane has consumed it, so an ambiguous write-back can never grant
// extra views.
type localShareAuthorizer struct {
	dir     shareDirectorySource
	client  *http.Client
	baseURL string
	token   string

	mu sync.Mutex
	// pending holds locally counted views not yet reflected in the directory.
	pending map[string]int64
	// sent is the part of pending already dispatched to the control plane, so a
	// retry never sends the same view twice.
	sent map[string]int64
	// seen is the last directory total observed per token, used to detect when
	// the control plane has absorbed the dispatched count.
	seen map[string]int64
}

func newLocalShareAuthorizer(dir shareDirectorySource, controlURL, token string) *localShareAuthorizer {
	return &localShareAuthorizer{
		dir:     dir,
		client:  &http.Client{Timeout: 5 * time.Second},
		baseURL: strings.TrimSuffix(controlURL, "/"),
		token:   token,
		pending: map[string]int64{},
		sent:    map[string]int64{},
		seen:    map[string]int64{},
	}
}

// Authorize validates the token against the mirrored directory. When the
// directory is missing, the control plane is tried so an unrefreshed data plane
// still works rather than failing every share link.
func (a *localShareAuthorizer) Authorize(ctx context.Context, token, password string, countView bool) (ShareGrant, error) {
	dir := a.dir.Shares()
	if dir == nil {
		if a.baseURL == "" {
			return ShareGrant{}, &ShareAuthError{Status: http.StatusServiceUnavailable, Code: "SHARE_LINKS_UNAVAILABLE", Message: "share links are not loaded"}
		}
		return controlAuthorize(ctx, a.client, a.baseURL, a.token, token, password, countView)
	}
	link, ok := dir.Links[token]
	if !ok {
		return ShareGrant{}, &ShareAuthError{Status: http.StatusNotFound, Code: "SHARE_LINK_NOT_FOUND", Message: "share link not found"}
	}
	now := time.Now().UTC()
	if !link.Enabled {
		return ShareGrant{}, &ShareAuthError{Status: http.StatusForbidden, Code: "SHARE_DISABLED", Message: "share link is disabled"}
	}
	if link.ExpiresAt != nil && !now.Before(*link.ExpiresAt) {
		return ShareGrant{}, &ShareAuthError{Status: http.StatusGone, Code: "SHARE_EXPIRED", Message: "share link has expired"}
	}
	if link.PasswordHash != "" {
		if password == "" {
			return ShareGrant{}, &ShareAuthError{Status: http.StatusUnauthorized, Code: "SHARE_PASSWORD_REQUIRED", Message: "share link requires a password"}
		}
		if bcrypt.CompareHashAndPassword([]byte(link.PasswordHash), []byte(password)) != nil {
			return ShareGrant{}, &ShareAuthError{Status: http.StatusUnauthorized, Code: "SHARE_PASSWORD_INVALID", Message: "share link password is invalid"}
		}
	}

	a.mu.Lock()
	// The directory total growing means the control plane consumed an earlier
	// write-back; release the matching pending count so it is not counted twice.
	if prev, ok := a.seen[token]; ok && link.Views > prev {
		delta := link.Views - prev
		if delta >= a.pending[token] {
			delete(a.pending, token)
			delete(a.sent, token)
		} else {
			a.pending[token] -= delta
			if a.sent[token] > a.pending[token] {
				a.sent[token] = a.pending[token]
			}
		}
	}
	a.seen[token] = link.Views
	pending := a.pending[token]
	if countView {
		a.pending[token] = pending + 1
	}
	a.mu.Unlock()

	if link.MaxViews > 0 && link.Views+pending >= link.MaxViews {
		return ShareGrant{}, &ShareAuthError{Status: http.StatusForbidden, Code: "SHARE_VIEWS_EXCEEDED", Message: "share link view limit reached"}
	}
	if countView {
		go a.flush(token)
	}
	return ShareGrant{ProjectID: link.ProjectID, VersionID: link.VersionID, Cache: link.Cache}, nil
}

// flush dispatches only the not-yet-sent part of the pending count off the
// request path. The count stays pending until a later directory refresh shows
// the control plane absorbed it, so a write-back that fails or is ambiguous is
// retried. A duplicate send can only over-count, which is the safe direction for
// a view budget.
func (a *localShareAuthorizer) flush(token string) {
	if a.baseURL == "" {
		return
	}
	a.mu.Lock()
	count := a.pending[token] - a.sent[token]
	if count <= 0 {
		a.mu.Unlock()
		return
	}
	a.sent[token] += count
	a.mu.Unlock()

	payload, err := json.Marshal(shareCountBody{Count: count})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	endpoint := a.baseURL + "/internal/v1/share-links/" + url.PathEscape(token) + "/views"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if a.token != "" {
		req.Header.Set("X-Internal-Token", a.token)
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		a.rollback(token, count)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		a.rollback(token, count)
	}
}

// rollback re-arms a failed dispatch for the next view.
func (a *localShareAuthorizer) rollback(token string, count int64) {
	a.mu.Lock()
	a.sent[token] -= count
	if a.sent[token] < 0 {
		delete(a.sent, token)
	}
	a.mu.Unlock()
}

// shareCountBody is the POST /internal/v1/share-links/{token}/views payload.
type shareCountBody struct {
	Count int64 `json:"count"`
}

type shareAuthorizeBody struct {
	Password  string `json:"password"`
	CountView bool   `json:"count_view"`
}

// controlAuthorize asks the control plane to validate a share request. It backs
// the fallback used while the mirrored share directory has not been loaded yet.
func controlAuthorize(ctx context.Context, client *http.Client, baseURL, token, shareToken, password string, countView bool) (ShareGrant, error) {
	payload, err := json.Marshal(shareAuthorizeBody{Password: password, CountView: countView})
	if err != nil {
		return ShareGrant{}, err
	}
	endpoint := baseURL + "/internal/v1/share-links/" + url.PathEscape(shareToken) + "/authorize"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return ShareGrant{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Internal-Token", token)
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return ShareGrant{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ShareGrant{}, err
	}
	if resp.StatusCode != http.StatusOK {
		var envelope struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &envelope)
		code := envelope.Error.Code
		if code == "" {
			code = "SHARE_AUTH_FAILED"
		}
		message := envelope.Error.Message
		if message == "" {
			message = "share link authorization failed"
		}
		return ShareGrant{}, &ShareAuthError{Status: resp.StatusCode, Code: code, Message: message}
	}
	var grant ShareGrant
	if err := json.Unmarshal(body, &grant); err != nil {
		return ShareGrant{}, err
	}
	return grant, nil
}

// serveVersionDirect serves /__pageforge/v/{project}/{version}/{path}.
func (h *Handler) serveVersionDirect(w http.ResponseWriter, r *http.Request, raw string) {
	projectID, versionID, rest, ok := splitDirect(raw)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "version direct link requires project and version ids")
		return
	}
	remainder, err := normalizeRemainder(rest)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_PATH", "invalid request path")
		return
	}
	// Version direct links are anonymous by default and independent from
	// binding auth policies (FR-19).
	snap := h.snapshots.Load()
	if snap == nil {
		writeError(w, http.StatusServiceUnavailable, "SNAPSHOT_UNAVAILABLE", "routing table is not loaded")
		return
	}
	binding := snapshot.BindingView{ProjectID: projectID}
	h.serveProject(w, r, snap, serveParams{
		projectID: projectID,
		versionID: versionID,
		binding:   binding,
		remainder: remainder,
	})
}

// serveShare serves /__pageforge/s/{token}/{path} after asking the control plane
// to authorize the token, password and view budget.
func (h *Handler) serveShare(w http.ResponseWriter, r *http.Request, raw string) {
	token, rest, ok := splitToken(raw)
	if !ok {
		writeError(w, http.StatusNotFound, "SHARE_LINK_NOT_FOUND", "share link not found")
		return
	}
	remainder, err := normalizeRemainder(rest)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_PATH", "invalid request path")
		return
	}
	if h.shareAuth == nil {
		writeError(w, http.StatusNotImplemented, "SHARE_LINKS_UNAVAILABLE", "share links are not configured")
		return
	}
	password, fromCookie := sharePassword(r)
	// 密码表单的 POST 只做校验，重定向后的 GET 才计一次访问。
	countView := countsAsView(remainder) && r.Method != http.MethodPost
	grant, err := h.shareAuth.Authorize(r.Context(), token, password, countView)
	if err != nil {
		var authErr *ShareAuthError
		if errors.As(err, &authErr) {
			if authErr.Code == "SHARE_PASSWORD_REQUIRED" || authErr.Code == "SHARE_PASSWORD_INVALID" {
				if wantsHTML(r) {
					h.renderSharePassword(w, r, authErr.Code == "SHARE_PASSWORD_INVALID")
					return
				}
			}
			if authErr.Code == "SHARE_PASSWORD_REQUIRED" {
				w.Header().Set("WWW-Authenticate", `Basic realm="PageForge Share"`)
			}
			writeError(w, authErr.Status, authErr.Code, authErr.Message)
			return
		}
		h.logger.Error("authorize share link",
			slog.String("token", token), slog.String("error", err.Error()))
		writeError(w, http.StatusBadGateway, "UPSTREAM_ERROR", "share authorization failed")
		return
	}

	// 表单/头部输入的密码验证通过后写入会话 Cookie，后续页面与静态资源复用。
	if password != "" && !fromCookie {
		http.SetCookie(w, &http.Cookie{
			Name:     sharePasswordCookie,
			Value:    base64.RawURLEncoding.EncodeToString([]byte(password)),
			Path:     "/__pageforge" + sharePath + token + "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   r.TLS != nil,
		})
		if r.Method == http.MethodPost {
			http.Redirect(w, r, r.URL.Path, http.StatusSeeOther)
			return
		}
	}

	snap := h.snapshots.Load()
	if snap == nil {
		writeError(w, http.StatusServiceUnavailable, "SNAPSHOT_UNAVAILABLE", "routing table is not loaded")
		return
	}
	if _, ok := snap.Projects[grant.ProjectID]; !ok {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "project is not available")
		return
	}
	cache := grant.Cache
	binding := snapshot.BindingView{ProjectID: grant.ProjectID}
	h.serveProject(w, r, snap, serveParams{
		projectID: grant.ProjectID,
		versionID: grant.VersionID,
		binding:   binding,
		remainder: remainder,
		cache:     &cache,
	})
}

// wantsHTML 判断是否应返回 HTML 密码表单（浏览器导航），而不是 JSON。
func wantsHTML(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		return false
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// renderSharePassword 返回只包含密码输入框的页面，避免浏览器弹出带用户名的 Basic 认证框。
func (h *Handler) renderSharePassword(w http.ResponseWriter, r *http.Request, invalid bool) {
	message := "该分享链接需要访问密码"
	if invalid {
		message = "密码不正确，请重试"
	}
	action := html.EscapeString(r.URL.Path)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnauthorized)
	fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>访问密码 · PageForge</title>
<style>
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;background:#0f172a;color:#e2e8f0;font-family:system-ui,sans-serif}
.card{width:min(92vw,360px);background:#1e293b;padding:2rem;border-radius:12px;box-shadow:0 10px 30px rgba(0,0,0,.4)}
h1{margin:0 0 .5rem;font-size:1.25rem}
.msg{margin:0 0 1.25rem;color:#94a3b8;font-size:.9rem}
input{width:100%%;box-sizing:border-box;padding:.7rem .8rem;border-radius:8px;border:1px solid #334155;background:#0f172a;color:#e2e8f0;font-size:1rem}
button{margin-top:1rem;width:100%%;padding:.7rem;border:0;border-radius:8px;background:#2563eb;color:#fff;font-size:1rem;cursor:pointer}
.tip{margin:1rem 0 0;color:#64748b;font-size:.8rem;text-align:center}
</style></head>
<body><form class="card" method="post" action="%s">
<h1>需要验证</h1>
<p class="msg">%s</p>
<input name="password" type="password" placeholder="访问密码" autocomplete="off" autofocus required>
<button type="submit">进入</button>
<p class="tip">仅需输入密码，无需用户名。</p>
</form></body></html>`, action, message)
}

func sharePassword(r *http.Request) (value string, fromCookie bool) {
	if value := r.Header.Get("X-Share-Password"); value != "" {
		return value, false
	}
	if cookie, err := r.Cookie(sharePasswordCookie); err == nil && cookie.Value != "" {
		if raw, err := base64.RawURLEncoding.DecodeString(cookie.Value); err == nil {
			return string(raw), true
		}
	}
	if value := r.FormValue("password"); value != "" {
		return value, false
	}
	if _, password, ok := r.BasicAuth(); ok {
		return password, false
	}
	return "", false
}

// countsAsView limits max_views consumption to page requests rather than every
// asset fetch.
func countsAsView(remainder string) bool {
	if remainder == "" {
		return true
	}
	lower := strings.ToLower(strings.TrimSuffix(remainder, "/"))
	return strings.HasSuffix(lower, ".html") || strings.HasSuffix(lower, ".htm")
}

// splitDirect splits "v/{project}/{version}/{rest}".
func splitDirect(raw string) (string, string, string, bool) {
	parts := strings.SplitN(raw, "/", 3)
	if len(parts) < 2 || !validSegment(parts[0]) || !validSegment(parts[1]) {
		return "", "", "", false
	}
	rest := ""
	if len(parts) == 3 {
		rest = parts[2]
	}
	return parts[0], parts[1], rest, true
}

// splitToken splits "s/{token}/{rest}".
func splitToken(raw string) (string, string, bool) {
	parts := strings.SplitN(raw, "/", 2)
	if len(parts) == 0 || !validSegment(parts[0]) {
		return "", "", false
	}
	rest := ""
	if len(parts) == 2 {
		rest = parts[1]
	}
	return parts[0], rest, true
}

func normalizeRemainder(rest string) (string, error) {
	if rest == "" {
		return "", nil
	}
	return httpx.NormalizeKey("/" + rest)
}

func validSegment(segment string) bool {
	if segment == "" || segment == "." || segment == ".." {
		return false
	}
	if len(segment) > 128 {
		return false
	}
	for i := 0; i < len(segment); i++ {
		c := segment[i]
		if c < 0x20 || c == 0x7f || c == '/' || c == '\\' || c == '%' {
			return false
		}
	}
	return true
}
