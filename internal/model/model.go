// Package model holds configuration types shared by control-plane entities.
package model

import (
	"strings"

	"github.com/pageforge/pageforge/internal/httpx"
)

// Project status values.
const (
	ProjectActive   = "active"
	ProjectDisabled = "disabled"
)

// SPAConfig controls SPA fallback behaviour for a project.
type SPAConfig struct {
	Enabled           bool     `json:"enabled"`
	ExcludePrefixes   []string `json:"exclude_prefixes,omitempty"`
	ExcludeExtensions []string `json:"exclude_extensions,omitempty"`
}

// CachePolicy holds project-level Cache-Control values.
type CachePolicy struct {
	HTML                string `json:"html,omitempty"`
	Assets              string `json:"assets,omitempty"`
	AssetsImmutableGlob string `json:"assets_immutable_glob,omitempty"`
	ImmutableMaxAge     int    `json:"immutable_max_age,omitempty"`
	NotFoundTTLSeconds  int    `json:"not_found_ttl_seconds,omitempty"`
}

// WithDefaults fills the documented defaults for empty fields.
func (c CachePolicy) WithDefaults() CachePolicy {
	if c.HTML == "" {
		c.HTML = "no-cache"
	}
	if c.Assets == "" {
		c.Assets = "public, max-age=300"
	}
	if c.ImmutableMaxAge == 0 {
		c.ImmutableMaxAge = 31536000
	}
	return c
}

// HeaderRule is a response-header rule (FR-21). Replace defaults to true;
// set it to false to append the value instead of overwriting.
type HeaderRule struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Replace *bool  `json:"replace,omitempty"`
}

// Replaces reports whether the rule overwrites an existing header value.
func (r HeaderRule) Replaces() bool {
	return r.Replace == nil || *r.Replace
}

var forbiddenHeaders = map[string]struct{}{
	"content-length":    {},
	"connection":        {},
	"transfer-encoding": {},
	"trailer":           {},
	"host":              {},
}

// IsForbiddenHeader reports whether the header may not be overridden by a
// response-header rule.
func IsForbiddenHeader(name string) bool {
	_, forbidden := forbiddenHeaders[strings.ToLower(strings.TrimSpace(name))]
	return forbidden
}

// ValidateHeaderRules rejects hop-by-hop and length-controlled headers.
func ValidateHeaderRules(rules []HeaderRule) error {
	for _, rule := range rules {
		name := strings.TrimSpace(rule.Name)
		if name == "" {
			return httpx.Invalid("INVALID_RESPONSE_HEADER", "response header name must not be empty")
		}
		if _, forbidden := forbiddenHeaders[strings.ToLower(name)]; forbidden {
			return httpx.Invalid("INVALID_RESPONSE_HEADER", "response header "+name+" is not allowed")
		}
		if strings.ContainsAny(rule.Value, "\r\n\x00") {
			return httpx.Invalid("INVALID_RESPONSE_HEADER", "response header value contains a control character")
		}
		if !validHeaderName(name) {
			return httpx.Invalid("INVALID_RESPONSE_HEADER", "invalid response header name "+name)
		}
	}
	return nil
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	if !strings.ContainsRune("!#$%&'*+-.^_`|~0123456789", rune(name[0])) && !isAlpha(name[0]) {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if isAlpha(c) || c >= '0' && c <= '9' {
			continue
		}
		if !strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			return false
		}
	}
	return true
}

func isAlpha(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
