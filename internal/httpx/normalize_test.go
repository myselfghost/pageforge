package httpx

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeKey(t *testing.T) {
	valid := map[string]string{
		"/":                  "",
		"":                   "",
		"/index.html":        "index.html",
		"/docs/":             "docs/",
		"/assets/app.abc.js": "assets/app.abc.js",
		"/a%20b.txt":         "a b.txt",
		"/中文/首页.html":        "中文/首页.html",
	}
	for in, want := range valid {
		got, err := NormalizeKey(in)
		if err != nil {
			t.Fatalf("NormalizeKey(%q) error: %v", in, err)
		}
		if got != want {
			t.Fatalf("NormalizeKey(%q) = %q, want %q", in, got, want)
		}
	}

	invalid := []string{
		"index.html",
		"/../etc/passwd",
		"/a/../b",
		"/a/./b",
		"//a",
		"/a//b",
		"/a%2Fb",
		"/a%2fb",
		"/a%5Cb",
		"/a%252Fb",
		"/a%252e%252e/b",
		"/a\\b",
		"/a\x00b",
		"/" + strings.Repeat("a", MaxKeyLen),
		"/a/%2e%2e/b",
	}
	for _, in := range invalid {
		if got, err := NormalizeKey(in); err == nil {
			t.Fatalf("NormalizeKey(%q) = %q, want error", in, got)
		} else {
			var apiErr *Error
			if !errors.As(err, &apiErr) || apiErr.Code != "INVALID_PATH" {
				t.Fatalf("NormalizeKey(%q) error = %v, want INVALID_PATH", in, err)
			}
		}
	}
}

func TestValidateRelKey(t *testing.T) {
	for _, ok := range []string{"index.html", "404.html", "docs/index.html"} {
		if err := ValidateRelKey(ok); err != nil {
			t.Fatalf("ValidateRelKey(%q) error: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "/index.html", "../index.html", "a//b", "docs/"} {
		if err := ValidateRelKey(bad); err == nil {
			t.Fatalf("ValidateRelKey(%q) = nil, want error", bad)
		}
	}
}

func TestNormalizeHost(t *testing.T) {
	valid := map[string]string{
		"":                 "*",
		"*":                "*",
		"Example.COM":      "example.com",
		"example.com:8080": "example.com",
		"example.com.":     "example.com",
		"*.Example.com":    "*.example.com",
		"localhost:80":     "localhost",
	}
	for in, want := range valid {
		got, err := NormalizeHost(in)
		if err != nil {
			t.Fatalf("NormalizeHost(%q) error: %v", in, err)
		}
		if got != want {
			t.Fatalf("NormalizeHost(%q) = %q, want %q", in, got, want)
		}
	}

	invalid := []string{
		"*.",
		"a*.example.com",
		"*.*.example.com",
		"exa mple.com",
		"example.com:abc",
		"-bad.com",
		"bad-.com",
		"a..b",
		"a_b.example.com",
		"[::1]:8080",
		":8080",
		strings.Repeat("a", 64) + ".com",
	}
	for _, in := range invalid {
		if got, err := NormalizeHost(in); err == nil {
			t.Fatalf("NormalizeHost(%q) = %q, want error", in, got)
		} else {
			var apiErr *Error
			if !errors.As(err, &apiErr) || apiErr.Code != "INVALID_HOST" {
				t.Fatalf("NormalizeHost(%q) error = %v, want INVALID_HOST", in, err)
			}
		}
	}
}

func TestNormalizePathPrefix(t *testing.T) {
	valid := map[string]string{
		"/":             "/",
		"/url1":         "/url1",
		"/url1/":        "/url1",
		"/a/b/":         "/a/b",
		"/__pageforge":  "/__pageforge",
		"/__pageforge2": "/__pageforge2",
	}
	for in, want := range valid {
		got, err := NormalizePathPrefix(in)
		if err != nil {
			t.Fatalf("NormalizePathPrefix(%q) error: %v", in, err)
		}
		if got != want {
			t.Fatalf("NormalizePathPrefix(%q) = %q, want %q", in, got, want)
		}
	}

	invalid := []string{"", "url1", "//a", "/a//b", "/a/../b", "/a b", "/a%2Fb", "/a\\b", "/a?b"}
	for _, in := range invalid {
		if got, err := NormalizePathPrefix(in); err == nil {
			t.Fatalf("NormalizePathPrefix(%q) = %q, want error", in, got)
		} else {
			var apiErr *Error
			if !errors.As(err, &apiErr) || apiErr.Code != "INVALID_PATH_PREFIX" {
				t.Fatalf("NormalizePathPrefix(%q) error = %v, want INVALID_PATH_PREFIX", in, err)
			}
		}
	}
}

func TestConflictsReserved(t *testing.T) {
	reserved := "/__pageforge"
	cases := map[string]bool{
		"/__pageforge":   true,
		"/__pageforge/x": true,
		"/__pageforge2":  false,
		"/":              false,
		"/url1":          false,
	}
	for prefix, want := range cases {
		if got := ConflictsReserved(prefix, reserved); got != want {
			t.Fatalf("ConflictsReserved(%q) = %v, want %v", prefix, got, want)
		}
	}
}

func FuzzNormalizeKey(f *testing.F) {
	for _, seed := range []string{"/", "/index.html", "/docs/", "/a%20b", "/../x", "/a%2Fb", "//a"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		key, err := NormalizeKey(raw)
		if err != nil {
			return
		}
		if strings.HasPrefix(key, "/") {
			t.Fatalf("key %q has a leading slash", key)
		}
		for _, seg := range strings.Split(key, "/") {
			if seg == "." || seg == ".." {
				t.Fatalf("key %q contains a relative segment", key)
			}
		}
	})
}

func FuzzNormalizeHost(f *testing.F) {
	for _, seed := range []string{"", "*", "Example.com:80", "*.example.com"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		host, err := NormalizeHost(raw)
		if err != nil {
			return
		}
		if host != "*" && host != strings.ToLower(host) {
			t.Fatalf("host %q is not lowercase", host)
		}
		if strings.HasPrefix(host, "*") && host != "*" && !strings.HasPrefix(host, "*.") {
			t.Fatalf("host %q has an invalid wildcard", host)
		}
	})
}
