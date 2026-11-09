// Benchmark 20k bindings routing performance.
// 用法：go test ./internal/router -bench=. -benchmem -count=3 -run=^$
package router

import (
	"testing"

	"github.com/pageforge/pageforge/internal/snapshot"
)

func newBind(id, host, prefix string) snapshot.BindingView {
	return snapshot.BindingView{
		ID: id, Host: host, PathPrefix: prefix,
		ProjectID: "prj", Enabled: true,
	}
}

func itoaBytes(i int) []byte {
	if i == 0 {
		return []byte{'0'}
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return buf[pos:]
}

// N 个 binding 挂在同一个 host 上，路径形如 /b0 /b1 ... /bN
func singleHostN(n int) []snapshot.BindingView {
	b := make([]snapshot.BindingView, n)
	for i := 0; i < n; i++ {
		b[i] = newBind("b"+string(itoaBytes(i)), "example.com", "/b"+string(itoaBytes(i)))
	}
	return b
}

// N 个 binding，N 个不同的 exact host
func manyHostsN(n int) []snapshot.BindingView {
	b := make([]snapshot.BindingView, n)
	for i := 0; i < n; i++ {
		b[i] = newBind("b"+string(itoaBytes(i)), "h"+string(itoaBytes(i))+".example.com", "/")
	}
	return b
}

// N 个 binding，N 个不同的 wildcard host
func wildcardsN(n int) []snapshot.BindingView {
	b := make([]snapshot.BindingView, n)
	for i := 0; i < n; i++ {
		b[i] = newBind("b"+string(itoaBytes(i)), "*."+string(itoaBytes(i))+".example.com", "/")
	}
	return b
}

// 真实项目常见形态：少量 host + 大量 path
func realistN(n, hosts int) []snapshot.BindingView {
	b := make([]snapshot.BindingView, n)
	for i := 0; i < n; i++ {
		h := "h" + string(itoaBytes(i%hosts)) + ".example.com"
		b[i] = newBind("b"+string(itoaBytes(i)), h, "/p"+string(itoaBytes(i)))
	}
	return b
}

// ---------------- Build benchmarks ----------------

func BenchmarkBuild20kSingleHost(b *testing.B) {
	snap := &snapshot.Snapshot{Bindings: singleHostN(20000)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Build(snap)
	}
}

func BenchmarkBuild20kManyHosts(b *testing.B) {
	snap := &snapshot.Snapshot{Bindings: manyHostsN(20000)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Build(snap)
	}
}

func BenchmarkBuild20kWildcards(b *testing.B) {
	snap := &snapshot.Snapshot{Bindings: wildcardsN(20000)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Build(snap)
	}
}

func BenchmarkBuild10kRealist(b *testing.B) {
	snap := &snapshot.Snapshot{Bindings: realistN(10000, 100)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Build(snap)
	}
}

// ---------------- Match benchmarks ----------------

func BenchmarkMatch20kSingleHostHot(b *testing.B) {
	table := Build(&snapshot.Snapshot{Bindings: singleHostN(20000)})
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 12345
		for pb.Next() {
			_, _ = table.Match("example.com", "b"+string(itoaBytes(i%20000))+"/index.html")
			i++
		}
	})
}

func BenchmarkMatch20kManyHosts(b *testing.B) {
	table := Build(&snapshot.Snapshot{Bindings: manyHostsN(20000)})
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			h := "h" + string(itoaBytes(i%20000)) + ".example.com"
			_, _ = table.Match(h, "index.html")
			i++
		}
	})
}

func BenchmarkMatch20kWildcards(b *testing.B) {
	table := Build(&snapshot.Snapshot{Bindings: wildcardsN(20000)})
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			h := "a." + string(itoaBytes(i%20000)) + ".example.com"
			_, _ = table.Match(h, "index.html")
			i++
		}
	})
}

func BenchmarkMatch20kWildcardsMiss(b *testing.B) {
	table := Build(&snapshot.Snapshot{Bindings: wildcardsN(20000)})
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = table.Match("totally-unknown.example.org", "index.html")
		}
	})
}

func BenchmarkMatch10kRealist(b *testing.B) {
	table := Build(&snapshot.Snapshot{Bindings: realistN(10000, 100)})
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			h := "h" + string(itoaBytes(i%100)) + ".example.com"
			_, _ = table.Match(h, "p"+string(itoaBytes(i%10000))+"/index.html")
			i++
		}
	})
}
