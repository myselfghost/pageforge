package manifest_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"

	"github.com/pageforge/pageforge/internal/manifest"
	"github.com/pageforge/pageforge/internal/testutil"
)

const testBucket = "static"

func addEntries(t *testing.T, builder *manifest.Builder, keys []string) {
	t.Helper()
	for _, key := range keys {
		entry := manifest.Entry{
			Key:         key,
			Size:        int64(len(key)),
			SHA256:      "00",
			ContentType: "text/plain",
			ModTime:     1762300000,
		}
		if err := builder.Add(entry); err != nil {
			t.Fatalf("add %q: %v", key, err)
		}
	}
}

func readGzipJSON(t *testing.T, data []byte, target any) {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer reader.Close()
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read gzip: %v", err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("decode json: %v", err)
	}
}

func TestBuildSplitsShardsBySegment(t *testing.T) {
	store := testutil.NewMemoryStore()
	builder, err := manifest.NewBuilder("_system/manifests/prj/ver",
		manifest.Limits{ShardFiles: 20, ShardBytes: 1 << 20}, t.TempDir())
	if err != nil {
		t.Fatalf("new builder: %v", err)
	}
	defer builder.Close()

	keys := []string{"index.html", "robots.txt", "assets/readme.txt"}
	for i := 0; i < 12; i++ {
		keys = append(keys, fmt.Sprintf("assets/x/%02d.js", i))
		keys = append(keys, fmt.Sprintf("assets/y/%02d.js", i))
	}
	addEntries(t, builder, keys)

	result, err := builder.Build(context.Background(), store, testBucket, manifest.RootOptions{
		ProjectID: "prj_1",
		VersionID: "ver_1",
		IndexDoc:  "index.html",
		ErrorDoc:  "404.html",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if result.FileCount != int64(len(keys)) {
		t.Fatalf("file count = %d, want %d", result.FileCount, len(keys))
	}

	raw, ok := store.Read(testBucket, result.RootKey)
	if !ok {
		t.Fatalf("root object %q missing", result.RootKey)
	}
	if store.Encoding(testBucket, result.RootKey) != "gzip" {
		t.Fatal("root object is not gzip encoded")
	}
	var root manifest.Root
	readGzipJSON(t, raw, &root)

	if root.Schema != manifest.SchemaRoot || root.IndexDoc != "index.html" || root.ErrorDoc != "404.html" {
		t.Fatalf("root = %+v", root)
	}
	if root.DefaultShard != "shards/_root.json" {
		t.Fatalf("default shard = %q", root.DefaultShard)
	}
	for _, prefix := range []string{"_root", "assets", "assets/x", "assets/y"} {
		if _, ok := root.Shards[prefix]; !ok {
			t.Fatalf("missing shard %q in %v", prefix, root.Shards)
		}
	}
	if len(root.Shards) != 4 {
		t.Fatalf("shards = %v, want 4", root.Shards)
	}

	var rootShard manifest.Shard
	readGzipJSON(t, mustRead(t, store, result.RootKey[:len(result.RootKey)-len("root.json")]+root.Shards["_root"]), &rootShard)
	if len(rootShard.Files) != 2 {
		t.Fatalf("root shard files = %v", rootShard.Files)
	}
	if len(rootShard.Dirs) != 1 || rootShard.Dirs[0] != "assets" {
		t.Fatalf("root dirs = %v, want [assets]", rootShard.Dirs)
	}

	var nested manifest.Shard
	readGzipJSON(t, mustRead(t, store, "_system/manifests/prj/ver/"+root.Shards["assets/x"]), &nested)
	if len(nested.Files) != 12 {
		t.Fatalf("nested shard files = %d, want 12", len(nested.Files))
	}
	if len(nested.Dirs) != 2 || nested.Dirs[0] != "assets" || nested.Dirs[1] != "assets/x" {
		t.Fatalf("nested dirs = %v", nested.Dirs)
	}

	order := store.PutOrder()
	if len(order) == 0 || order[len(order)-1] != result.RootKey {
		t.Fatalf("root was not uploaded last: %v", order)
	}
}

func TestBuildHundredThousandFiles(t *testing.T) {
	store := testutil.NewMemoryStore()
	builder, err := manifest.NewBuilder("manifests/prj/ver", manifest.DefaultLimits(), t.TempDir())
	if err != nil {
		t.Fatalf("new builder: %v", err)
	}
	defer builder.Close()

	const files = 100000
	for i := 0; i < files; i++ {
		key := fmt.Sprintf("big/d%d/f%d.js", i%10, i)
		if err := builder.Add(manifest.Entry{Key: key, Size: 1, SHA256: "00", ContentType: "text/javascript"}); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	result, err := builder.Build(context.Background(), store, testBucket, manifest.RootOptions{
		ProjectID: "prj_1", VersionID: "ver_1", IndexDoc: "index.html",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if result.FileCount != files {
		t.Fatalf("file count = %d", result.FileCount)
	}

	var root manifest.Root
	readGzipJSON(t, mustRead(t, store, result.RootKey), &root)
	if root.FileCount != files {
		t.Fatalf("root file count = %d", root.FileCount)
	}
	// _root plus ten "big/dN" shards: 100k files exceed one shard's 20k limit.
	if len(root.Shards) != 11 {
		t.Fatalf("shard count = %d, want 11 (%v)", len(root.Shards), keysOf(root.Shards))
	}
	var total int
	for prefix, key := range root.Shards {
		var shard manifest.Shard
		readGzipJSON(t, mustRead(t, store, "manifests/prj/ver/"+key), &shard)
		if shard.Prefix != prefix {
			t.Fatalf("shard %q has prefix %q", key, shard.Prefix)
		}
		total += len(shard.Files)
	}
	if total != files {
		t.Fatalf("shard files total = %d", total)
	}
}

func mustRead(t *testing.T, store *testutil.MemoryStore, key string) []byte {
	t.Helper()
	data, ok := store.Read(testBucket, key)
	if !ok {
		t.Fatalf("object %q missing", key)
	}
	return data
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	return out
}
