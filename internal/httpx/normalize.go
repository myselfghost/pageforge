// Package httpx normalizes request paths and hosts and writes JSON responses
// shared by the control-plane API.
package httpx

import (
	"net/url"
	"strings"
	"unicode/utf8"
)

const (
	// MaxKeyLen bounds a request path before it becomes an object key.
	MaxKeyLen = 2048
	// MaxHostLen bounds a normalized host or wildcard host.
	MaxHostLen = 253
	// MaxPathPrefixLen bounds a binding path prefix.
	MaxPathPrefixLen = 256
)

func invalidPath(msg string) error {
	return &Error{Status: 400, Code: "INVALID_PATH", Message: msg}
}

func invalidHost(msg string) error {
	return &Error{Status: 400, Code: "INVALID_HOST", Message: msg}
}

func invalidPathPrefix(msg string) error {
	return &Error{Status: 400, Code: "INVALID_PATH_PREFIX", Message: msg}
}

// NormalizeKey validates a URL path and returns the object key relative to a
// project root (no leading slash). A trailing slash is preserved so callers can
// still tell directories from files.
func NormalizeKey(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if len(raw) > MaxKeyLen {
		return "", invalidPath("path exceeds maximum length")
	}
	if strings.ContainsAny(raw, "\\\x00") {
		return "", invalidPath("path contains a control or backslash character")
	}
	if !strings.HasPrefix(raw, "/") {
		return "", invalidPath("path must start with /")
	}
	lower := strings.ToLower(raw)
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") {
		return "", invalidPath("encoded slash or backslash is not allowed")
	}

	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return "", invalidPath("path contains invalid percent-encoding")
	}
	if decoded != raw {
		// Re-encoding a decoded path must not smuggle a traversal or separator.
		lowerDecoded := strings.ToLower(decoded)
		for _, marker := range []string{"%2f", "%5c", "%2e", "%00"} {
			if strings.Contains(lowerDecoded, marker) {
				return "", invalidPath("double-encoded path is not allowed")
			}
		}
	}
	if strings.ContainsAny(decoded, "\\\x00") {
		return "", invalidPath("path contains a control or backslash character")
	}
	if !utf8.ValidString(decoded) {
		return "", invalidPath("path is not valid UTF-8")
	}

	key := strings.TrimPrefix(decoded, "/")
	if key == "" {
		return "", nil
	}
	trailing := strings.HasSuffix(key, "/")
	if trailing {
		key = strings.TrimSuffix(key, "/")
	}
	if key == "" {
		return "", nil
	}
	for _, seg := range strings.Split(key, "/") {
		switch seg {
		case "":
			return "", invalidPath("empty path segment is not allowed")
		case ".", "..":
			return "", invalidPath("relative path segment is not allowed")
		}
		for i := 0; i < len(seg); i++ {
			if seg[i] < 0x20 || seg[i] == 0x7f {
				return "", invalidPath("path contains a control character")
			}
		}
	}
	if trailing {
		key += "/"
	}
	return key, nil
}

// ValidateRelKey validates a project-relative key such as index_doc or
// error_doc. It must not start with a slash and must be a plain file key.
func ValidateRelKey(value string) error {
	if value == "" {
		return invalidPath("key must not be empty")
	}
	if strings.HasPrefix(value, "/") {
		return invalidPath("key must not start with /")
	}
	key, err := NormalizeKey("/" + value)
	if err != nil {
		return err
	}
	if strings.HasSuffix(key, "/") {
		return invalidPath("key must not end with /")
	}
	return nil
}

// NormalizeHost lowercases a host, strips the port and a trailing dot, and
// normalizes the default host to "*". Wildcards are only allowed as a leftmost
// "*.example.com" label.
func NormalizeHost(raw string) (string, error) {
	host := strings.TrimSpace(raw)
	if host == "" || host == "*" {
		return "*", nil
	}
	host = strings.TrimSuffix(host, ".")
	if len(host) > MaxHostLen+6 {
		return "", invalidHost("host exceeds maximum length")
	}

	if strings.HasPrefix(host, "[") {
		end := strings.Index(host, "]")
		if end < 0 {
			return "", invalidHost("invalid IPv6 literal")
		}
		return "", invalidHost("IPv6 literals are not supported")
	} else if i := strings.LastIndex(host, ":"); i >= 0 {
		port := host[i+1:]
		if port == "" || !isDigits(port) {
			return "", invalidHost("invalid port")
		}
		host = host[:i]
		if host == "" {
			return "", invalidHost("host must not be empty")
		}
	}

	host = strings.ToLower(host)
	if host == "" {
		return "", invalidHost("host must not be empty")
	}
	if host == "*" {
		return "", invalidHost("wildcard must not carry a trailing dot or port")
	}
	if len(host) > MaxHostLen {
		return "", invalidHost("host exceeds maximum length")
	}

	rest := host
	if strings.HasPrefix(host, "*.") {
		rest = host[2:]
		if rest == "" || strings.Contains(rest, "*") {
			return "", invalidHost("wildcard is only allowed as the leftmost label")
		}
	} else if strings.Contains(host, "*") {
		return "", invalidHost("wildcard is only allowed as the leftmost label")
	}

	for _, label := range strings.Split(rest, ".") {
		if label == "" {
			return "", invalidHost("host contains an empty label")
		}
		if len(label) > 63 {
			return "", invalidHost("host label exceeds 63 characters")
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return "", invalidHost("host label must not start or end with -")
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' {
				continue
			}
			return "", invalidHost("host contains invalid characters")
		}
	}
	return host, nil
}

// NormalizePathPrefix validates a binding path prefix. The root prefix is "/";
// other prefixes are returned without a trailing slash.
func NormalizePathPrefix(raw string) (string, error) {
	prefix := strings.TrimSpace(raw)
	if prefix == "" {
		return "", invalidPathPrefix("path prefix must not be empty")
	}
	if len(prefix) > MaxPathPrefixLen {
		return "", invalidPathPrefix("path prefix exceeds maximum length")
	}
	if !strings.HasPrefix(prefix, "/") {
		return "", invalidPathPrefix("path prefix must start with /")
	}
	if strings.ContainsAny(prefix, "\\\x00%?# ") {
		return "", invalidPathPrefix("path prefix contains an invalid character")
	}
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" {
		return "/", nil
	}
	for _, seg := range strings.Split(strings.TrimPrefix(prefix, "/"), "/") {
		switch seg {
		case "":
			return "", invalidPathPrefix("path prefix contains an empty segment")
		case ".", "..":
			return "", invalidPathPrefix("relative path segment is not allowed")
		}
	}
	return prefix, nil
}

// ConflictsReserved reports whether prefix equals reserved or is nested below it.
func ConflictsReserved(prefix, reserved string) bool {
	if reserved == "" {
		return false
	}
	return prefix == reserved || strings.HasPrefix(prefix, reserved+"/")
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
