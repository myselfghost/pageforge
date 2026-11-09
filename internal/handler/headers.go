package handler

import (
	"net/http"
	"net/textproto"
	"strings"

	"github.com/pageforge/pageforge/internal/model"
	"github.com/pageforge/pageforge/internal/observability"
	"github.com/pageforge/pageforge/internal/snapshot"
)

// applyResponseHeaders merges project defaults with binding rules and writes
// them before any status line is sent. Binding rules win on conflicts without
// discarding project rules, matching FR-21.
func (h *Handler) applyResponseHeaders(w http.ResponseWriter, r *http.Request,
	project snapshot.ProjectView, binding snapshot.BindingView, versionID string) {
	if h.opts.ApplyResponseHeaders != nil {
		h.opts.ApplyResponseHeaders(w, project, binding)
		return
	}

	values := make(map[string]string)
	for _, rule := range project.ResponseHeaders {
		writeHeaderRule(w.Header(), values, rule, r, binding.ProjectID, versionID, binding.ID)
	}
	for _, rule := range binding.ResponseHeaders {
		writeHeaderRule(w.Header(), values, rule, r, binding.ProjectID, versionID, binding.ID)
	}
}

func writeHeaderRule(dst http.Header, seen map[string]string, rule model.HeaderRule,
	r *http.Request, projectID, versionID, bindingID string) {
	name := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(rule.Name))
	if name == "" || model.IsForbiddenHeader(name) {
		return
	}
	value := expandHeaderValue(rule.Value, r, projectID, versionID, bindingID)

	// replace=false appends to the value produced so far, otherwise the rule
	// replaces every previously applied value for this header.
	if current, ok := seen[name]; ok && !rule.Replaces() {
		value = current + ", " + value
	}
	seen[name] = value
	dst.Set(name, value)
}

func expandHeaderValue(value string, r *http.Request, projectID, versionID, bindingID string) string {
	requestID := observability.RequestIDFromContext(r.Context())
	replacer := strings.NewReplacer(
		"$project_id", projectID,
		"$version_id", versionID,
		"$binding_id", bindingID,
		"$request_id", requestID,
	)
	return replacer.Replace(value)
}
