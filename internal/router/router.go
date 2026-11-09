// Package router matches requests to bindings by host and longest path prefix.
package router

import (
	"sort"
	"strings"

	"github.com/pageforge/pageforge/internal/snapshot"
)

// Table is an immutable routing table built from one snapshot.
type Table struct {
	exact         map[string]*hostNode
	wildcards     []wildcardRoute
	wildcardIndex map[string]int // suffix -> index in wildcards (dedup during Build only)
	fallback      *hostNode
}

type wildcardRoute struct {
	suffix string // includes the leading dot, e.g. ".example.com"
	node   *hostNode
}

type hostNode struct {
	root *pathNode
}

type pathNode struct {
	children map[string]*pathNode
	binding  *snapshot.BindingView
}

// Match is one successful route lookup.
type Match struct {
	Binding   snapshot.BindingView
	Remainder string // project-relative, no leading slash, trailing slash preserved
}

// Build compiles a routing table. Bindings with enabled=false are ignored.
// Wildcard suffixes are sorted once at the end by descending length, so that
// longer (more specific) wildcards are checked first by hostNode().
func Build(snap *snapshot.Snapshot) *Table {
	t := &Table{
		exact:         map[string]*hostNode{},
		wildcardIndex: map[string]int{},
	}
	if snap == nil {
		return t
	}
	for i := range snap.Bindings {
		b := snap.Bindings[i]
		if !b.Enabled {
			continue
		}
		node := t.host(b.Host)
		node.root.insert(b.PathPrefix, &b)
	}
	// 单次排序：把 host() 里每次 append 都 sort 的 O(N² log N) 降到 O(N log N)
	sort.SliceStable(t.wildcards, func(i, j int) bool {
		return len(t.wildcards[i].suffix) > len(t.wildcards[j].suffix)
	})
	return t
}

func (t *Table) host(host string) *hostNode {
	if host == "*" {
		if t.fallback == nil {
			t.fallback = newHostNode()
		}
		return t.fallback
	}
	if strings.HasPrefix(host, "*.") {
		suffix := host[1:]
		if idx, ok := t.wildcardIndex[suffix]; ok {
			return t.wildcards[idx].node
		}
		node := newHostNode()
		t.wildcardIndex[suffix] = len(t.wildcards)
		t.wildcards = append(t.wildcards, wildcardRoute{suffix: suffix, node: node})
		return node
	}
	node, ok := t.exact[host]
	if !ok {
		node = newHostNode()
		t.exact[host] = node
	}
	return node
}

func newHostNode() *hostNode {
	return &hostNode{root: &pathNode{children: map[string]*pathNode{}}}
}

func (n *pathNode) insert(prefix string, binding *snapshot.BindingView) {
	node := n
	for _, segment := range segments(strings.TrimPrefix(prefix, "/")) {
		child, ok := node.children[segment]
		if !ok {
			child = &pathNode{children: map[string]*pathNode{}}
			node.children[segment] = child
		}
		node = child
	}
	node.binding = binding
}

// Match resolves host and a normalized path key (no leading slash, trailing
// slash preserved) to the longest matching binding.
func (t *Table) Match(host, pathKey string) (Match, bool) {
	node := t.hostNode(host)
	if node == nil {
		return Match{}, false
	}

	trailing := strings.HasSuffix(pathKey, "/")
	segs := segments(strings.TrimSuffix(pathKey, "/"))

	current := node.root
	best := current.binding
	depth := 0
	for i, segment := range segs {
		child, ok := current.children[segment]
		if !ok {
			break
		}
		current = child
		depth = i + 1
		if current.binding != nil {
			best = current.binding
		}
	}
	if best == nil {
		return Match{}, false
	}

	remainder := strings.Join(segs[depth:], "/")
	if trailing && remainder != "" {
		remainder += "/"
	}
	return Match{Binding: *best, Remainder: remainder}, true
}

func (t *Table) hostNode(host string) *hostNode {
	if node, ok := t.exact[host]; ok {
		return node
	}
	for _, route := range t.wildcards {
		if len(host) > len(route.suffix) && strings.HasSuffix(host, route.suffix) {
			return route.node
		}
	}
	return t.fallback
}

func segments(path string) []string {
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}
