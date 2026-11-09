package handler

import (
	"net/http"
	"strings"
)

func (h *Handler) isReserved(value string) bool {
	prefix := h.opts.ReservedPrefix
	if prefix == "" {
		return false
	}
	candidate := value
	if !strings.HasPrefix(candidate, "/") {
		candidate = "/" + candidate
	}
	return candidate == prefix || strings.HasPrefix(candidate, prefix+"/")
}

// isSharePath 判断路径是否属于分享链接入口；该入口允许 POST，用于提交密码表单。
func (h *Handler) isSharePath(value string) bool {
	prefix := h.opts.ReservedPrefix
	return prefix != "" && strings.HasPrefix(value, prefix+sharePath)
}

// serveReserved handles health probes, version direct links (FR-19), share
// links (FR-18) and forwards the internal API paths to the control plane.
func (h *Handler) serveReserved(w http.ResponseWriter, r *http.Request) {
	prefix := h.opts.ReservedPrefix
	switch r.URL.Path {
	case prefix + "/healthz", prefix + "/readyz":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte("ok\n"))
		}
		return
	}
	// 保留前缀的派发用解码后的 URL.Path；但进入版本直达 / 分享入口的余下路径
	// 必须回退到 EscapedPath，才能让 NormalizeKey 拦下 %2F/%5C 编码穿越。
	escaped := r.URL.EscapedPath()
	if rest, ok := strings.CutPrefix(escaped, prefix+versionDirectPath); ok {
		h.serveVersionDirect(w, r, rest)
		return
	}
	if rest, ok := strings.CutPrefix(escaped, prefix+sharePath); ok {
		h.serveShare(w, r, rest)
		return
	}
	if h.proxy != nil {
		h.proxy.ServeHTTP(w, r)
		return
	}
	writeError(w, http.StatusNotFound, "RESERVED_PATH", "unknown reserved path")
}
