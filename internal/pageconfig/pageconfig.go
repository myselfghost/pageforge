// Package pageconfig parses the optional pageforge.json shipped at the root of
// a publish package. Only document names and content-type overrides are allowed;
// project-level policy stays under the control of the management API.
package pageconfig

import (
	"encoding/json"
	"fmt"
	"mime"
	"strings"

	"github.com/pageforge/pageforge/internal/httpx"
)

// FileName is the configuration file name at the package root.
const FileName = "pageforge.json"

// MaxBytes bounds the configuration file size.
const MaxBytes = 64 << 10

// policyFields are accepted but ignored: they belong to the project API.
var policyFields = map[string]struct{}{
	"spa":              {},
	"cache":            {},
	"auth":             {},
	"auth_policy":      {},
	"response_headers": {},
}

// Config is the subset of pageforge.json honoured by the publisher.
type Config struct {
	IndexDoc     *string
	ErrorDoc     *string
	ContentTypes map[string]string
}

func invalid(format string, args ...any) error {
	return httpx.Invalid("INVALID_PAGE_CONFIG", fmt.Sprintf(format, args...))
}

// IsConfigName reports whether name is the package configuration file. The match
// is case-insensitive so a differently cased file cannot be published as content.
func IsConfigName(name string) bool {
	return strings.EqualFold(name, FileName)
}

// Parse decodes and validates pageforge.json. Warnings describe ignored fields.
func Parse(data []byte) (Config, []string, error) {
	if len(data) > MaxBytes {
		return Config{}, nil, invalid("%s exceeds %d bytes", FileName, MaxBytes)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Config{}, nil, invalid("%s is not valid JSON: %v", FileName, err)
	}

	cfg := Config{ContentTypes: map[string]string{}}
	var warnings []string
	for name, value := range raw {
		switch name {
		case "index_doc":
			doc, err := parseDoc(name, value)
			if err != nil {
				return Config{}, nil, err
			}
			cfg.IndexDoc = &doc
		case "error_doc":
			doc, err := parseDoc(name, value)
			if err != nil {
				return Config{}, nil, err
			}
			cfg.ErrorDoc = &doc
		case "content_types":
			overrides, err := parseContentTypes(value)
			if err != nil {
				return Config{}, nil, err
			}
			cfg.ContentTypes = overrides
		default:
			if _, policy := policyFields[name]; policy {
				warnings = append(warnings,
					fmt.Sprintf("%s: field %q is managed by the project API and was ignored", FileName, name))
				continue
			}
			warnings = append(warnings,
				fmt.Sprintf("%s: unknown field %q was ignored", FileName, name))
		}
	}
	return cfg, warnings, nil
}

func parseDoc(name string, value json.RawMessage) (string, error) {
	var doc string
	if err := json.Unmarshal(value, &doc); err != nil {
		return "", invalid("%s: %s must be a string", FileName, name)
	}
	doc = strings.TrimSpace(doc)
	if doc == "" {
		return "", invalid("%s: %s must not be empty", FileName, name)
	}
	if err := httpx.ValidateRelKey(doc); err != nil {
		return "", invalid("%s: %s is invalid: %v", FileName, name, err)
	}
	return doc, nil
}

func parseContentTypes(value json.RawMessage) (map[string]string, error) {
	var raw map[string]string
	if err := json.Unmarshal(value, &raw); err != nil {
		return nil, invalid("%s: content_types must be an object of strings", FileName)
	}
	out := make(map[string]string, len(raw))
	for ext, ctype := range raw {
		ext = strings.ToLower(strings.TrimSpace(ext))
		if ext == "" || strings.ContainsAny(ext, "./\\ \t\r\n") {
			return nil, invalid("%s: content_types key %q must be an extension without a dot", FileName, ext)
		}
		for i := 0; i < len(ext); i++ {
			c := ext[i]
			if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '-' {
				continue
			}
			return nil, invalid("%s: content_types key %q contains an invalid character", FileName, ext)
		}
		mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(ctype))
		if err != nil || mediaType == "" {
			return nil, invalid("%s: content_types[%q] is not a valid MIME type", FileName, ext)
		}
		out[ext] = mediaType
	}
	return out, nil
}
