package router_test

import (
	"testing"

	"github.com/pageforge/pageforge/internal/router"
	"github.com/pageforge/pageforge/internal/snapshot"
)

func binding(id, host, prefix string, enabled bool) snapshot.BindingView {
	return snapshot.BindingView{ID: id, Host: host, PathPrefix: prefix, ProjectID: "prj", Enabled: enabled}
}

func TestMatchLongestPathPrefix(t *testing.T) {
	table := router.Build(&snapshot.Snapshot{Bindings: []snapshot.BindingView{
		binding("root", "example.com", "/", true),
		binding("docs", "example.com", "/docs", true),
		binding("api-docs", "example.com", "/docs/api", true),
		binding("disabled", "example.com", "/disabled", false),
	}})

	match, ok := table.Match("example.com", "docs/api/index.html")
	if !ok || match.Binding.ID != "api-docs" || match.Remainder != "index.html" {
		t.Fatalf("match = %+v ok=%v", match, ok)
	}
	match, ok = table.Match("example.com", "docs/guide.html")
	if !ok || match.Binding.ID != "docs" || match.Remainder != "guide.html" {
		t.Fatalf("match = %+v ok=%v", match, ok)
	}
	match, ok = table.Match("example.com", "other/file.txt")
	if !ok || match.Binding.ID != "root" || match.Remainder != "other/file.txt" {
		t.Fatalf("match = %+v ok=%v", match, ok)
	}
	match, ok = table.Match("example.com", "")
	if !ok || match.Binding.ID != "root" || match.Remainder != "" {
		t.Fatalf("root match = %+v ok=%v", match, ok)
	}
	if match, ok := table.Match("example.com", "disabled/file"); ok && match.Binding.ID == "disabled" {
		t.Fatal("disabled binding must not be routable")
	}
}

func TestMatchSegmentsNotSubstrings(t *testing.T) {
	table := router.Build(&snapshot.Snapshot{Bindings: []snapshot.BindingView{
		binding("app", "example.com", "/app", true),
	}})
	if _, ok := table.Match("example.com", "application/index.html"); ok {
		t.Fatal("/app must not match /application")
	}
	match, ok := table.Match("example.com", "app/")
	if !ok || match.Remainder != "" {
		t.Fatalf("match = %+v ok=%v", match, ok)
	}
	match, ok = table.Match("example.com", "app/docs/")
	if !ok || match.Remainder != "docs/" {
		t.Fatalf("trailing slash remainder = %q", match.Remainder)
	}
}

func TestMatchRemainderTrailingSlash(t *testing.T) {
	table := router.Build(&snapshot.Snapshot{Bindings: []snapshot.BindingView{
		binding("root", "example.com", "/", true),
		binding("docs", "example.com", "/docs", true),
		binding("api", "example.com", "/docs/api", true),
	}})

	cases := []struct {
		pathKey   string
		bindingID string
		remainder string
	}{
		// 路径根：命中托底 binding，remainder 为空。
		{"", "root", ""},
		// 命中绑定前缀本身，且请求带尾斜杠：remainder 为空（目录语义）。
		{"docs/", "docs", ""},
		// 命中绑定前缀本身，无尾斜杠：remainder 为空。
		{"docs", "docs", ""},
		// 更深前缀命中，保留尾部斜杠供 try_files 识别目录。
		{"docs/api/", "api", ""},
		{"docs/api/index.html", "api", "index.html"},
		// 非绑定前缀的尾斜杠必须保留。
		{"docs/guide/", "docs", "guide/"},
		{"docs/guide.html", "docs", "guide.html"},
	}
	for _, tc := range cases {
		match, ok := table.Match("example.com", tc.pathKey)
		if !ok {
			t.Fatalf("Match(%q) not found", tc.pathKey)
		}
		if match.Binding.ID != tc.bindingID || match.Remainder != tc.remainder {
			t.Fatalf("Match(%q) = binding %q remainder %q, want %q / %q",
				tc.pathKey, match.Binding.ID, match.Remainder, tc.bindingID, tc.remainder)
		}
	}
}

func TestHostWildcardAndFallback(t *testing.T) {
	table := router.Build(&snapshot.Snapshot{Bindings: []snapshot.BindingView{
		binding("exact", "www.example.com", "/", true),
		binding("wild", "*.example.com", "/", true),
		binding("deep", "*.eu.example.com", "/", true),
		binding("default", "*", "/", true),
	}})

	if match, ok := table.Match("www.example.com", ""); !ok || match.Binding.ID != "exact" {
		t.Fatalf("exact match = %+v", match)
	}
	if match, ok := table.Match("a.example.com", ""); !ok || match.Binding.ID != "wild" {
		t.Fatalf("wildcard match = %+v", match)
	}
	if match, ok := table.Match("a.eu.example.com", ""); !ok || match.Binding.ID != "deep" {
		t.Fatalf("longest wildcard = %+v", match)
	}
	if match, ok := table.Match("unknown.test", ""); !ok || match.Binding.ID != "default" {
		t.Fatalf("default host = %+v", match)
	}
	if match, ok := table.Match("example.com", ""); ok && match.Binding.ID == "wild" {
		t.Fatal("bare domain must not match *.example.com")
	}
}
