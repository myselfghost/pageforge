package pageconfig_test

import (
	"strings"
	"testing"

	"github.com/pageforge/pageforge/internal/pageconfig"
	"github.com/pageforge/pageforge/internal/testutil"
)

func TestParseAcceptsDocumentedFields(t *testing.T) {
	cfg, warnings, err := pageconfig.Parse([]byte(`{
		"index_doc": "home.html",
		"error_doc": "errors/404.html",
		"content_types": {"js": "text/x-custom", "woff3": "font/woff3"}
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	if cfg.IndexDoc == nil || *cfg.IndexDoc != "home.html" {
		t.Fatalf("index_doc = %v", cfg.IndexDoc)
	}
	if cfg.ErrorDoc == nil || *cfg.ErrorDoc != "errors/404.html" {
		t.Fatalf("error_doc = %v", cfg.ErrorDoc)
	}
	if cfg.ContentTypes["js"] != "text/x-custom" || cfg.ContentTypes["woff3"] != "font/woff3" {
		t.Fatalf("content_types = %v", cfg.ContentTypes)
	}
}

func TestParseIgnoresPolicyFieldsWithWarnings(t *testing.T) {
	cfg, warnings, err := pageconfig.Parse([]byte(`{
		"spa": {"enabled": true},
		"cache": {"html": "no-store"},
		"response_headers": [{"name": "x"}, {"name": "y"}],
		"unknown": 1
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.IndexDoc != nil || cfg.ErrorDoc != nil {
		t.Fatalf("policy fields leaked into config: %+v", cfg)
	}
	if len(warnings) != 4 {
		t.Fatalf("warnings = %v, want 4 entries", warnings)
	}
	if !contains(warnings, "spa") || !contains(warnings, "cache") || !contains(warnings, "unknown") {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestParseRejectsInvalidConfig(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "not json", body: `{`},
		{name: "wrong type", body: `{"index_doc": 42}`},
		{name: "empty index doc", body: `{"index_doc": "  "}`},
		{name: "traversal doc", body: `{"error_doc": "../404.html"}`},
		{name: "content types not object", body: `{"content_types": []}`},
		{name: "extension with dot", body: `{"content_types": {".js": "text/javascript"}}`},
		{name: "invalid mime", body: `{"content_types": {"js": "not a mime"}}`},
		{name: "empty mime", body: `{"content_types": {"js": ""}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := pageconfig.Parse([]byte(tc.body))
			if err == nil {
				t.Fatal("invalid config was accepted")
			}
			testutil.RequireCode(t, err, "INVALID_PAGE_CONFIG")
		})
	}
}

func TestParseRejectsOversizedConfig(t *testing.T) {
	body := `{"index_doc":"` + strings.Repeat("a", pageconfig.MaxBytes) + `.html"}`
	_, _, err := pageconfig.Parse([]byte(body))
	if err == nil {
		t.Fatal("oversized config was accepted")
	}
	testutil.RequireCode(t, err, "INVALID_PAGE_CONFIG")
}

func TestIsConfigName(t *testing.T) {
	for _, name := range []string{"pageforge.json", "PageForge.JSON", "PAGEFORGE.json"} {
		if !pageconfig.IsConfigName(name) {
			t.Fatalf("IsConfigName(%q) = false", name)
		}
	}
	for _, name := range []string{"pageforge.json5", "assets/pageforge.json", "pageforge"} {
		if pageconfig.IsConfigName(name) {
			t.Fatalf("IsConfigName(%q) = true", name)
		}
	}
}

func contains(values []string, needle string) bool {
	for _, value := range values {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
