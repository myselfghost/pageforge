// Package router matches requests to bindings by host and longest path prefix.
package router

import (
	"sort"
	"strings"

	"github.com/pageforge/pageforge/internal/snapshot"
)

// Table is an immutable routing table built from one snapshot.
type Table struct {
	snap *snapshot.Snapshot
	// ranked 按路径前缀长度降序保存启用绑定的下标。停用绑定不进入该列表，因此
	// 最长前缀未命中或被停用时，会继续尝试更短的前缀（如 /ai 或 /）。
	ranked []int
}

// Match is one successful route lookup.
type Match struct {
	Binding   snapshot.BindingView
	Remainder string // project-relative, no leading slash, trailing slash preserved
}

// Build compiles a routing table. Bindings with enabled=false never match: the
// lookup skips them and continues with the next shorter enabled prefix.
func Build(snap *snapshot.Snapshot) *Table {
	t := &Table{snap: snap}
	if snap == nil {
		return t
	}
	for i := range snap.Bindings {
		if snap.Bindings[i].Enabled {
			t.ranked = append(t.ranked, i)
		}
	}
	prefixLen := func(i int) int { return len(snap.Bindings[i].PathPrefix) }
	sort.SliceStable(t.ranked, func(a, b int) bool {
		ia, ib := t.ranked[a], t.ranked[b]
		if prefixLen(ia) != prefixLen(ib) {
			return prefixLen(ia) > prefixLen(ib)
		}
		// 同长度前缀时优先更具体的 host（精确 > 通配 > "*"），通配中后缀更长者优先。
		sa, sb := hostSpecificity(snap.Bindings[ia].Host), hostSpecificity(snap.Bindings[ib].Host)
		if sa != sb {
			return sa > sb
		}
		return len(snap.Bindings[ia].Host) > len(snap.Bindings[ib].Host)
	})
	return t
}

// hostSpecificity ranks host patterns so exact hosts win over wildcards.
func hostSpecificity(host string) int {
	switch {
	case host == "*":
		return 0
	case strings.HasPrefix(host, "*."):
		return 1
	default:
		return 2
	}
}

// Match resolves host and a normalized path key (no leading slash, trailing
// slash preserved) to the longest matching enabled binding. Disabled bindings
// are absent from the table, so their prefixes fall through to the next shorter
// enabled prefix instead of returning "not found".
func (t *Table) Match(host, pathKey string) (Match, bool) {
	trailing := strings.HasSuffix(pathKey, "/")
	for _, idx := range t.ranked {
		candidate := t.snap.Bindings[idx]
		if !hostMatches(candidate.Host, host) || !pathHasPrefix(pathKey, candidate.PathPrefix) {
			continue
		}
		remainder := strings.TrimPrefix(strings.TrimSuffix(pathKey, "/"), strings.Trim(candidate.PathPrefix, "/"))
		remainder = strings.TrimPrefix(remainder, "/")
		if trailing && remainder != "" && !strings.HasSuffix(remainder, "/") {
			remainder += "/"
		}
		return Match{Binding: candidate, Remainder: remainder}, true
	}
	return Match{}, false
}

// hostMatches reports whether a binding host pattern matches the request host.
func hostMatches(pattern, host string) bool {
	switch {
	case pattern == "*":
		return true
	case strings.HasPrefix(pattern, "*."):
		suffix := pattern[1:]
		return len(host) > len(suffix) && strings.HasSuffix(host, suffix)
	default:
		return pattern == host
	}
}

// pathHasPrefix reports whether pathKey sits at or under prefix, comparing
// whole segments so that /ai does not match /ai-feng.
func pathHasPrefix(pathKey, prefix string) bool {
	p := strings.Trim(prefix, "/")
	if p == "" {
		return true
	}
	key := strings.TrimSuffix(pathKey, "/")
	return key == p || strings.HasPrefix(key, p+"/")
}
