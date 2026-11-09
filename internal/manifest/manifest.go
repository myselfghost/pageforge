// Package manifest builds and stores the immutable, sharded manifest that the
// data plane uses to resolve object keys.
package manifest

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/pageforge/pageforge/internal/objectstore"
)

// Manifest schemas.
const (
	SchemaRoot  = "pageforge.manifest.v1"
	SchemaShard = "pageforge.shard.v1"

	rootShardPrefix = "_root"
)

// Entry describes one published object.
type Entry struct {
	Key         string `json:"key"`                // project/version relative, no leading slash
	Size        int64  `json:"size"`               // object size in bytes
	SHA256      string `json:"sha256"`             // hex digest of the object body
	ContentType string `json:"ctype"`              // resolved content type
	ModTime     int64  `json:"mtime"`              // unix seconds
	Encoding    string `json:"encoding,omitempty"` // "" | "br" | "gz"
}

// Root is the always-resident manifest object.
type Root struct {
	Schema       string            `json:"schema"`
	ProjectID    string            `json:"project_id"`
	VersionID    string            `json:"version_id"`
	FileCount    int64             `json:"file_count"`
	TotalBytes   int64             `json:"total_bytes"`
	IndexDoc     string            `json:"index_doc"`
	ErrorDoc     string            `json:"error_doc"`
	DefaultShard string            `json:"default_shard"`
	Shards       map[string]string `json:"shards"`
}

// Shard is one lazily loaded subset of the manifest.
type Shard struct {
	Schema string           `json:"schema"`
	Prefix string           `json:"prefix"`
	Files  map[string]Entry `json:"files"`
	Dirs   []string         `json:"dirs"`
}

// Limits controls when a prefix is split into child shards.
type Limits struct {
	ShardFiles int
	ShardBytes int
}

// DefaultLimits returns the documented shard thresholds.
func DefaultLimits() Limits {
	return Limits{ShardFiles: 20000, ShardBytes: 2 << 20}
}

func (l Limits) withDefaults() Limits {
	def := DefaultLimits()
	if l.ShardFiles <= 0 {
		l.ShardFiles = def.ShardFiles
	}
	if l.ShardBytes <= 0 {
		l.ShardBytes = def.ShardBytes
	}
	return l
}

// RootOptions carries the version identity written into root.json.
type RootOptions struct {
	ProjectID string
	VersionID string
	IndexDoc  string
	ErrorDoc  string
}

// Result reports what Build uploaded.
type Result struct {
	FileCount  int64
	TotalBytes int64
	RootSHA256 string
	RootKey    string
	RootSize   int64
	Shards     map[string]string
}

// Builder spools manifest entries to disk while objects upload, then emits
// shards and the root document. The spool keeps the unpack phase independent of
// the number of in-memory entries.
type Builder struct {
	prefix    string
	limits    Limits
	spool     *os.File
	spoolPath string
	count     int64
	total     int64
	closed    bool
}

// NewBuilder creates a builder writing the manifest under objectPrefix.
func NewBuilder(objectPrefix string, limits Limits, tempDir string) (*Builder, error) {
	spool, err := os.CreateTemp(tempDir, "manifest-spool-*.jsonl")
	if err != nil {
		return nil, fmt.Errorf("create manifest spool: %w", err)
	}
	return &Builder{
		prefix:    strings.Trim(objectPrefix, "/"),
		limits:    limits.withDefaults(),
		spool:     spool,
		spoolPath: spool.Name(),
	}, nil
}

// Add appends one entry to the spool.
func (b *Builder) Add(entry Entry) error {
	if b.closed {
		return fmt.Errorf("manifest builder is closed")
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode manifest entry %q: %w", entry.Key, err)
	}
	if _, err := b.spool.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write manifest spool: %w", err)
	}
	b.count++
	b.total += entry.Size
	return nil
}

// FileCount returns the number of spooled entries.
func (b *Builder) FileCount() int64 {
	return b.count
}

// TotalBytes returns the sum of the spooled object sizes.
func (b *Builder) TotalBytes() int64 {
	return b.total
}

// Build uploads all shards and then root.json. Root is written last so a
// partially uploaded manifest is never discoverable.
func (b *Builder) Build(ctx context.Context, store objectstore.ObjectStore, bucket string, opts RootOptions) (Result, error) {
	if b.closed {
		return Result{}, fmt.Errorf("manifest builder is closed")
	}
	entries, err := b.load()
	if err != nil {
		return Result{}, err
	}
	groups := partition(entries, b.limits)

	result := Result{
		FileCount:  b.count,
		TotalBytes: b.total,
		Shards:     make(map[string]string, len(groups)),
	}
	root := Root{
		Schema:     SchemaRoot,
		ProjectID:  opts.ProjectID,
		VersionID:  opts.VersionID,
		FileCount:  b.count,
		TotalBytes: b.total,
		IndexDoc:   opts.IndexDoc,
		ErrorDoc:   opts.ErrorDoc,
		Shards:     map[string]string{},
	}

	names := make(map[string]int, len(groups))
	for _, group := range groups {
		name := shardName(group.prefix, names)
		key := b.prefix + "/shards/" + name
		dirs := dirsFor(group.files)
		if group.prefix == rootShardPrefix {
			dirs = firstLevelDirs(entries)
		}
		shard := Shard{
			Schema: SchemaShard,
			Prefix: group.prefix,
			Files:  make(map[string]Entry, len(group.files)),
			Dirs:   dirs,
		}
		for _, entry := range group.files {
			shard.Files[entry.Key] = entry
		}
		if err := b.putGzipJSON(ctx, store, bucket, key, shard); err != nil {
			return Result{}, err
		}
		root.Shards[group.prefix] = "shards/" + name
		result.Shards[group.prefix] = "shards/" + name
	}

	root.DefaultShard = root.Shards[rootShardPrefix]
	raw, err := json.Marshal(root)
	if err != nil {
		return Result{}, fmt.Errorf("encode manifest root: %w", err)
	}
	sum := sha256.Sum256(raw)
	rootKey := b.prefix + "/root.json"
	size, err := b.putGzip(ctx, store, bucket, rootKey, raw)
	if err != nil {
		return Result{}, err
	}
	result.RootSHA256 = hex.EncodeToString(sum[:])
	result.RootKey = rootKey
	result.RootSize = size
	return result, nil
}

// Close removes the spool file; it is safe to call more than once.
func (b *Builder) Close() error {
	if b.closed {
		return nil
	}
	b.closed = true
	var err error
	if b.spool != nil {
		err = b.spool.Close()
		b.spool = nil
	}
	if b.spoolPath != "" {
		if removeErr := os.Remove(b.spoolPath); removeErr != nil && err == nil {
			err = removeErr
		}
		b.spoolPath = ""
	}
	return err
}

func (b *Builder) load() ([]Entry, error) {
	if err := b.spool.Sync(); err != nil {
		return nil, fmt.Errorf("flush manifest spool: %w", err)
	}
	if _, err := b.spool.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind manifest spool: %w", err)
	}
	entries := make([]Entry, 0, b.count)
	scanner := bufio.NewScanner(b.spool)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var entry Entry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return nil, fmt.Errorf("decode manifest spool: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read manifest spool: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	return entries, nil
}

func (b *Builder) putGzipJSON(ctx context.Context, store objectstore.ObjectStore, bucket, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode manifest %s: %w", key, err)
	}
	if _, err := b.putGzip(ctx, store, bucket, key, raw); err != nil {
		return err
	}
	return nil
}

func (b *Builder) putGzip(ctx context.Context, store objectstore.ObjectStore, bucket, key string, raw []byte) (int64, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		return 0, fmt.Errorf("compress manifest %s: %w", key, err)
	}
	if err := gz.Close(); err != nil {
		return 0, fmt.Errorf("compress manifest %s: %w", key, err)
	}
	body := buf.Bytes()
	opt := objectstore.PutOptions{ContentType: "application/json", Encoding: "gzip"}
	if err := store.Put(ctx, bucket, key, bytes.NewReader(body), int64(len(body)), opt); err != nil {
		return 0, fmt.Errorf("upload manifest %s: %w", key, err)
	}
	return int64(len(body)), nil
}

type shardGroup struct {
	prefix string
	files  []Entry
}

// partition implements 9.3: root files plus first-level groups; a group that
// exceeds the thresholds is split by its next path segment, recursively.
func partition(entries []Entry, limits Limits) []shardGroup {
	rootFiles := make([]Entry, 0, len(entries))
	rest := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		if isRootFile(entry.Key) {
			rootFiles = append(rootFiles, entry)
		} else {
			rest = append(rest, entry)
		}
	}

	groups := []shardGroup{{prefix: rootShardPrefix, files: rootFiles}}
	var walk func(prefix string, files []Entry)
	walk = func(prefix string, files []Entry) {
		direct := make([]Entry, 0, len(files))
		children := make(map[string][]Entry)
		for _, entry := range files {
			remainder := entry.Key
			if prefix != "" {
				remainder = strings.TrimPrefix(entry.Key, prefix+"/")
			}
			if i := strings.IndexByte(remainder, '/'); i >= 0 {
				children[remainder[:i]] = append(children[remainder[:i]], entry)
			} else {
				direct = append(direct, entry)
			}
		}
		if len(direct) > 0 {
			groups = append(groups, shardGroup{prefix: prefix, files: direct})
		}
		segments := make([]string, 0, len(children))
		for segment := range children {
			segments = append(segments, segment)
		}
		sort.Strings(segments)
		for _, segment := range segments {
			childPrefix := segment
			if prefix != "" {
				childPrefix = prefix + "/" + segment
			}
			group := children[segment]
			if fits(group, limits) {
				groups = append(groups, shardGroup{prefix: childPrefix, files: group})
			} else {
				walk(childPrefix, group)
			}
		}
	}
	if len(rest) > 0 {
		walk("", rest)
	}

	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].prefix == rootShardPrefix {
			return true
		}
		if groups[j].prefix == rootShardPrefix {
			return false
		}
		return groups[i].prefix < groups[j].prefix
	})
	return groups
}

func fits(files []Entry, limits Limits) bool {
	if limits.ShardFiles > 0 && len(files) > limits.ShardFiles {
		return false
	}
	estimate := 0
	for _, file := range files {
		estimate += estimateEntryBytes(file)
		if estimate > limits.ShardBytes {
			return false
		}
	}
	return true
}

// estimateEntryBytes approximates one entry's encoded JSON size.
func estimateEntryBytes(entry Entry) int {
	return 180 + 2*len(entry.Key)
}

func isRootFile(key string) bool {
	return !strings.Contains(key, "/")
}

// shardName maps a prefix to a stable object name. Different prefixes can
// collide after "/" is replaced; collisions get a numeric suffix.
func shardName(prefix string, used map[string]int) string {
	base := prefix
	if prefix != rootShardPrefix {
		base = strings.ReplaceAll(prefix, "/", "_")
	}
	used[base]++
	if used[base] == 1 {
		return base + ".json"
	}
	return fmt.Sprintf("%s-%d.json", base, used[base])
}

// dirsFor returns all ancestor directories of files.
func dirsFor(files []Entry) []string {
	set := make(map[string]struct{})
	for _, file := range files {
		segments := strings.Split(file.Key, "/")
		for i := 1; i < len(segments); i++ {
			set[strings.Join(segments[:i], "/")] = struct{}{}
		}
	}
	return sortedKeys(set)
}

// firstLevelDirs lists every top-level directory so the root shard can answer
// directory probes without loading child shards.
func firstLevelDirs(files []Entry) []string {
	set := make(map[string]struct{})
	for _, file := range files {
		if i := strings.IndexByte(file.Key, '/'); i > 0 {
			set[file.Key[:i]] = struct{}{}
		}
	}
	return sortedKeys(set)
}

func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
