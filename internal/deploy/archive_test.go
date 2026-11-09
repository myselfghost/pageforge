package deploy_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pageforge/pageforge/internal/deploy"
	"github.com/pageforge/pageforge/internal/testutil"
)

type zipEntry struct {
	name string
	body string
	mode os.FileMode
}

func writeZip(t *testing.T, entries []zipEntry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "package.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	writer := zip.NewWriter(file)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		if entry.mode != 0 {
			header.SetMode(entry.mode)
		}
		part, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatalf("create zip entry %q: %v", entry.name, err)
		}
		if _, err := part.Write([]byte(entry.body)); err != nil {
			t.Fatalf("write zip entry %q: %v", entry.name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close zip file: %v", err)
	}
	return path
}

func testLimits() deploy.Limits {
	return deploy.Limits{MaxFiles: 100, MaxFileBytes: 1 << 20, MaxTotalBytes: 10 << 20}
}

func TestOpenZipReadsEntriesAndConfig(t *testing.T) {
	path := writeZip(t, []zipEntry{
		{name: "index.html", body: "index"},
		{name: "dir/", body: ""},
		{name: "assets/app.js", body: "console.log(1)"},
		{name: "pageforge.json", body: `{"index_doc":"home.html"}`},
	})
	archive, err := deploy.OpenZip(path, testLimits())
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer archive.Close()

	if got := len(archive.Entries()); got != 2 {
		t.Fatalf("entries = %d, want 2", got)
	}
	if got := archive.FileCount(); got != 2 {
		t.Fatalf("file count = %d, want 2", got)
	}
	if got := archive.TotalBytes(); got != int64(len("index")+len("console.log(1)")) {
		t.Fatalf("total bytes = %d", got)
	}
	config, ok := archive.Config()
	if !ok {
		t.Fatal("pageforge.json was not detected")
	}
	if config.Key != "pageforge.json" {
		t.Fatalf("config key = %q", config.Key)
	}
	for _, entry := range archive.Entries() {
		if entry.Key == "pageforge.json" {
			t.Fatal("configuration file must not be part of the deployable entries")
		}
	}
}

func TestOpenZipRejectsMaliciousPackages(t *testing.T) {
	cases := []struct {
		name    string
		entries []zipEntry
	}{
		{name: "parent traversal", entries: []zipEntry{{name: "../evil.txt", body: "x"}}},
		{name: "nested traversal", entries: []zipEntry{{name: "a/../../b.txt", body: "x"}}},
		{name: "absolute path", entries: []zipEntry{{name: "/etc/passwd", body: "x"}}},
		{name: "backslash", entries: []zipEntry{{name: `a\b.txt`, body: "x"}}},
		{name: "NUL byte", entries: []zipEntry{{name: "a\x00b.txt", body: "x"}}},
		{name: "empty segment", entries: []zipEntry{{name: "a//b.txt", body: "x"}}},
		{name: "reserved system prefix", entries: []zipEntry{{name: "_system/manifests/root.json", body: "x"}}},
		{name: "reserved system prefix case", entries: []zipEntry{{name: "_System/root.json", body: "x"}}},
		{name: "case collision", entries: []zipEntry{
			{name: "Assets/A.js", body: "a"}, {name: "assets/a.js", body: "b"},
		}},
		{name: "duplicate entry", entries: []zipEntry{
			{name: "a.txt", body: "a"}, {name: "a.txt", body: "b"},
		}},
		{name: "symlink", entries: []zipEntry{{name: "link.txt", body: "/etc/passwd", mode: os.ModeSymlink | 0o777}}},
		{name: "config in subdirectory", entries: []zipEntry{{name: "conf/pageforge.json", body: "{}"}}},
		{name: "colon in path segment", entries: []zipEntry{
			{name: "~demos/:id/index.html", body: "x"}, {name: "index.html", body: "i"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeZip(t, tc.entries)
			archive, err := deploy.OpenArchive(path, testLimits(), "auto")
			if err == nil {
				_ = archive.Close()
				t.Fatal("malicious package was accepted")
			}
			testutil.RequireCode(t, err, "INVALID_PACKAGE")
		})
	}
}

func TestOpenZipDecodesGBKNames(t *testing.T) {
	// GBK-encoded bytes that are not valid UTF-8 (common in Windows-created
	// zips with Chinese filenames). The names must be decoded to UTF-8 so the
	// manifest keys can be addressed by URL requests later.
	path := writeZip(t, []zipEntry{
		{name: "\xb7\xa2\xb2\xbc/ai\xb5\xbc\xc8\xeb.html", body: "content"},
		{name: "\xcf\xea\xc7\xe9.html", body: "a"},
		{name: "\xd0\xc2\xd4\xf6.html", body: "b"},
		{name: "index.html", body: "index"},
	})
	archive, err := deploy.OpenZip(path, testLimits())
	if err != nil {
		t.Fatalf("open archive with GBK entry names: %v", err)
	}
	defer archive.Close()

	want := []string{"发布/ai导入.html", "详情.html", "新增.html", "index.html"}
	entries := archive.Entries()
	if len(entries) != len(want) {
		t.Fatalf("entries = %d, want %d", len(entries), len(want))
	}
	for i, entry := range entries {
		if entry.Key != want[i] {
			t.Errorf("entries[%d].Key = %q, want %q", i, entry.Key, want[i])
		}
	}
}

// TestOpenZipRejectsUndecodableName ensures names that are neither UTF-8 nor
// GB18030 are rejected instead of being stored as unreachable manifest keys.
func TestOpenZipRejectsUndecodableName(t *testing.T) {
	path := writeZip(t, []zipEntry{{name: "\xff\xff.html", body: "a"}})
	archive, err := deploy.OpenZip(path, testLimits())
	if archive != nil {
		_ = archive.Close()
	}
	if err == nil {
		t.Fatal("undecodable entry name was accepted")
	}
	testutil.RequireCode(t, err, "INVALID_PACKAGE")
}

func TestValidateArchiveKey(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		valid bool
	}{
		{name: "simple", key: "index.html", valid: true},
		{name: "nested", key: "assets/app.js", valid: true},
		{name: "non-utf8 bytes", key: "\xb7\xa2\xb2\xbc/ai.html", valid: true},
		{name: "empty", key: "", valid: false},
		{name: "leading slash", key: "/index.html", valid: false},
		{name: "trailing slash", key: "index.html/", valid: false},
		{name: "backslash", key: `a\b.txt`, valid: false},
		{name: "NUL byte", key: "a\x00b.txt", valid: false},
		{name: "parent segment", key: "../evil.txt", valid: false},
		{name: "empty segment", key: "a//b.txt", valid: false},
		{name: "control char", key: "a\x01b.txt", valid: false},
		{name: "DEL char", key: "a\x7fb.txt", valid: false},
		{name: "colon segment", key: "~demos/:id/index.html", valid: false},
		{name: "question mark", key: "assets/a?b.js", valid: false},
		{name: "star", key: "assets/a*b.js", valid: false},
		{name: "quotes", key: `assets/a"b.js`, valid: false},
		{name: "angle brackets", key: "assets/<a>.js", valid: false},
		{name: "pipe", key: "assets/a|b.js", valid: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := deploy.ValidateArchiveKey(tc.key)
			if tc.valid && err != nil {
				t.Fatalf("ValidateArchiveKey(%q) = %v, want nil", tc.key, err)
			}
			if !tc.valid && err == nil {
				t.Fatalf("ValidateArchiveKey(%q) = nil, want error", tc.key)
			}
		})
	}
}

func TestOpenZipEnforcesQuotas(t *testing.T) {
	cases := []struct {
		name   string
		limits deploy.Limits
		zip    []zipEntry
	}{
		{
			name:   "file count",
			limits: deploy.Limits{MaxFiles: 1, MaxFileBytes: 1 << 20, MaxTotalBytes: 1 << 20},
			zip:    []zipEntry{{name: "a.txt", body: "a"}, {name: "b.txt", body: "b"}},
		},
		{
			name:   "file size",
			limits: deploy.Limits{MaxFiles: 10, MaxFileBytes: 2, MaxTotalBytes: 1 << 20},
			zip:    []zipEntry{{name: "a.txt", body: "abcd"}},
		},
		{
			name:   "total size",
			limits: deploy.Limits{MaxFiles: 10, MaxFileBytes: 100, MaxTotalBytes: 5},
			zip:    []zipEntry{{name: "a.txt", body: "abc"}, {name: "b.txt", body: "def"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeZip(t, tc.zip)
			archive, err := deploy.OpenArchive(path, tc.limits, "auto")
			if err == nil {
				_ = archive.Close()
				t.Fatal("package exceeding the quota was accepted")
			}
			testutil.RequireCode(t, err, "PACKAGE_LIMIT_EXCEEDED")
		})
	}
}

func TestStorageKey(t *testing.T) {
	// Plain ASCII names pass through unchanged (spaces are valid UTF-8).
	if got := deploy.StorageKey("index.html"); got != "index.html" {
		t.Errorf("StorageKey(\"index.html\") = %q, want unchanged", got)
	}
	// Valid UTF-8 Chinese names pass through unchanged.
	if got := deploy.StorageKey("更新记录.html"); got != "更新记录.html" {
		t.Errorf("StorageKey(\"更新记录.html\") = %q, want unchanged", got)
	}
	// GBK-encoded bytes (invalid UTF-8) get percent-encoded.
	if got := deploy.StorageKey("\xb7\xa2\xb2\xbc.html"); got != "%B7%A2%B2%BC.html" {
		t.Errorf("StorageKey(GBK) = %q, want \"%%B7%%A2%%B2%%BC.html\"", got)
	}
	// Multi-segment: only the invalid-UTF-8 segment is changed.
	if got := deploy.StorageKey("dir/\xcf\xea\xc7\xe9.html"); got != "dir/%CF%EA%C7%E9.html" {
		t.Errorf("StorageKey(\"dir/GBK\") = %q, want \"dir/%%CF%%EA%%C7%%E9.html\"", got)
	}

	// Round-trip: StorageKey → FromStorageKey recovers the original.
	originals := []string{
		"index.html",
		"更新记录.html",
		"\xb7\xa2\xb2\xbc.html",
		"dir/\xcf\xea\xc7\xe9.html",
		"a/b/c.txt",
	}
	for _, orig := range originals {
		encoded := deploy.StorageKey(orig)
		decoded := deploy.FromStorageKey(encoded)
		if decoded != orig {
			t.Errorf("FromStorageKey(StorageKey(%q)) = %q, want %q", orig, decoded, orig)
		}
	}
}

// --- tar / tgz helpers and tests ---

type tarEntry struct {
	name string
	body string
	mode os.FileMode // for symlinks etc.; 0 for regular file
}

func writeTar(t *testing.T, entries []tarEntry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "package.tar")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create tar: %v", err)
	}
	w := tar.NewWriter(file)
	modTime := time.Now()
	for _, entry := range entries {
		var hdr tar.Header
		hdr.Name = entry.name
		hdr.ModTime = modTime
		if entry.mode != 0 {
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = entry.body
			hdr.Size = 0
		} else {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(entry.body))
		}
		if err := w.WriteHeader(&hdr); err != nil {
			t.Fatalf("write tar header %q: %v", entry.name, err)
		}
		if entry.mode == 0 {
			if _, err := w.Write([]byte(entry.body)); err != nil {
				t.Fatalf("write tar entry %q: %v", entry.name, err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close tar file: %v", err)
	}
	return path
}

func writeTgz(t *testing.T, entries []tarEntry) string {
	t.Helper()
	var tarBuf bytes.Buffer
	w := tar.NewWriter(&tarBuf)
	modTime := time.Now()
	for _, entry := range entries {
		hdr := tar.Header{Name: entry.name, Size: int64(len(entry.body)), ModTime: modTime}
		if err := w.WriteHeader(&hdr); err != nil {
			t.Fatalf("write tgz header %q: %v", entry.name, err)
		}
		if _, err := w.Write([]byte(entry.body)); err != nil {
			t.Fatalf("write tgz entry %q: %v", entry.name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	path := filepath.Join(t.TempDir(), "package.tar.gz")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create tgz: %v", err)
	}
	gzw := gzip.NewWriter(file)
	if _, err := gzw.Write(tarBuf.Bytes()); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close tgz file: %v", err)
	}
	return path
}

func TestOpenTarReadsEntriesAndConfig(t *testing.T) {
	path := writeTar(t, []tarEntry{
		{name: "index.html", body: "index"},
		{name: "assets/app.js", body: "console.log(1)"},
		{name: "pageforge.json", body: `{"index_doc":"home.html"}`},
	})
	archive, err := deploy.OpenArchive(path, testLimits(), "auto")
	if err != nil {
		t.Fatalf("open tar: %v", err)
	}
	defer archive.Close()

	if got := len(archive.Entries()); got != 2 {
		t.Fatalf("entries = %d, want 2", got)
	}
	config, ok := archive.Config()
	if !ok {
		t.Fatal("pageforge.json was not detected")
	}
	if config.Key != "pageforge.json" {
		t.Fatalf("config key = %q", config.Key)
	}
	for _, entry := range archive.Entries() {
		reader, err := entry.Open()
		if err != nil {
			t.Fatalf("open entry %q: %v", entry.Key, err)
		}
		data, err := readAllCloser(reader)
		if err != nil {
			t.Fatalf("read entry %q: %v", entry.Key, err)
		}
		if entry.Key == "index.html" && string(data) != "index" {
			t.Errorf("index.html = %q, want \"index\"", data)
		}
	}
}

func TestOpenTgzReadsEntries(t *testing.T) {
	path := writeTgz(t, []tarEntry{
		{name: "index.html", body: "<h1>hello</h1>"},
		{name: "assets/style.css", body: "body { margin: 0; }"},
	})
	archive, err := deploy.OpenArchive(path, testLimits(), "auto")
	if err != nil {
		t.Fatalf("open tgz: %v", err)
	}
	defer archive.Close()

	if got := len(archive.Entries()); got != 2 {
		t.Fatalf("entries = %d, want 2", got)
	}
	for _, entry := range archive.Entries() {
		reader, err := entry.Open()
		if err != nil {
			t.Fatalf("open entry %q: %v", entry.Key, err)
		}
		data, err := readAllCloser(reader)
		if err != nil {
			t.Fatalf("read entry %q: %v", entry.Key, err)
		}
		if len(data) == 0 {
			t.Errorf("entry %q has empty content", entry.Key)
		}
	}
}

func TestOpenTarRejectsMaliciousPackages(t *testing.T) {
	cases := []struct {
		name    string
		entries []tarEntry
	}{
		{name: "parent traversal", entries: []tarEntry{{name: "../evil.txt", body: "x"}}},
		{name: "absolute path", entries: []tarEntry{{name: "/etc/passwd", body: "x"}}},
		{name: "backslash", entries: []tarEntry{{name: `a\b.txt`, body: "x"}}},
		{name: "reserved system prefix", entries: []tarEntry{{name: "_system/manifests/root.json", body: "x"}}},
		{name: "case collision", entries: []tarEntry{
			{name: "Assets/A.js", body: "a"}, {name: "assets/a.js", body: "b"},
		}},
		{name: "duplicate entry", entries: []tarEntry{
			{name: "a.txt", body: "a"}, {name: "a.txt", body: "b"},
		}},
		{name: "symlink", entries: []tarEntry{{name: "link.txt", body: "/etc/passwd", mode: os.ModeSymlink}}},
		{name: "config in subdirectory", entries: []tarEntry{{name: "conf/pageforge.json", body: "{}"}}},
		{name: "colon in path segment", entries: []tarEntry{
			{name: "~demos/:id/index.html", body: "x"}, {name: "index.html", body: "i"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTar(t, tc.entries)
			archive, err := deploy.OpenArchive(path, testLimits(), "auto")
			if err == nil {
				_ = archive.Close()
				t.Fatal("malicious tar was accepted")
			}
			testutil.RequireCode(t, err, "INVALID_PACKAGE")
		})
	}
}

func TestOpenTarDecodesNonUTF8Names(t *testing.T) {
	// Legacy tar may contain non-UTF-8 filenames (e.g. GBK-encoded Chinese).
	// The GBK bytes for "详情" are 0xCF 0xEA 0xC7 0xE9.
	path := writeTar(t, []tarEntry{
		{name: "\xcf\xea\xc7\xe9.html", body: "content"},
		{name: "index.html", body: "index"},
	})
	archive, err := deploy.OpenArchive(path, testLimits(), "auto")
	if err != nil {
		t.Fatalf("open tar with non-UTF-8 names: %v", err)
	}
	defer archive.Close()

	want := []string{"详情.html", "index.html"}
	entries := archive.Entries()
	if len(entries) != len(want) {
		t.Fatalf("entries = %d, want %d", len(entries), len(want))
	}
	for i, entry := range entries {
		if entry.Key != want[i] {
			t.Errorf("entries[%d].Key = %q, want %q", i, entry.Key, want[i])
		}
	}
}

func TestOpenTarEnforcesQuotas(t *testing.T) {
	cases := []struct {
		name   string
		limits deploy.Limits
		tar    []tarEntry
	}{
		{
			name:   "file count",
			limits: deploy.Limits{MaxFiles: 1, MaxFileBytes: 1 << 20, MaxTotalBytes: 1 << 20},
			tar:    []tarEntry{{name: "a.txt", body: "a"}, {name: "b.txt", body: "b"}},
		},
		{
			name:   "file size",
			limits: deploy.Limits{MaxFiles: 10, MaxFileBytes: 2, MaxTotalBytes: 1 << 20},
			tar:    []tarEntry{{name: "a.txt", body: "abcd"}},
		},
		{
			name:   "total size",
			limits: deploy.Limits{MaxFiles: 10, MaxFileBytes: 100, MaxTotalBytes: 5},
			tar:    []tarEntry{{name: "a.txt", body: "abc"}, {name: "b.txt", body: "def"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTar(t, tc.tar)
			archive, err := deploy.OpenArchive(path, tc.limits, "auto")
			if err == nil {
				_ = archive.Close()
				t.Fatal("package exceeding quota was accepted")
			}
			testutil.RequireCode(t, err, "PACKAGE_LIMIT_EXCEEDED")
		})
	}
}

func TestOpenArchiveAutoDetect(t *testing.T) {
	// zip
	zipPath := writeZip(t, []zipEntry{{name: "a.html", body: "zip content"}})
	archive, err := deploy.OpenArchive(zipPath, testLimits(), "auto")
	if err != nil {
		t.Fatalf("auto-detect zip: %v", err)
	}
	_ = archive.Close()

	// tar
	tarPath := writeTar(t, []tarEntry{{name: "a.html", body: "tar content"}})
	archive, err = deploy.OpenArchive(tarPath, testLimits(), "auto")
	if err != nil {
		t.Fatalf("auto-detect tar: %v", err)
	}
	_ = archive.Close()

	// tgz
	tgzPath := writeTgz(t, []tarEntry{{name: "a.html", body: "tgz content"}})
	archive, err = deploy.OpenArchive(tgzPath, testLimits(), "auto")
	if err != nil {
		t.Fatalf("auto-detect tgz: %v", err)
	}
	_ = archive.Close()
}

func TestOpenArchiveModeMismatch(t *testing.T) {
	// A zip file with declared mode "tar" must be rejected.
	zipPath := writeZip(t, []zipEntry{{name: "a.html", body: "x"}})
	archive, err := deploy.OpenArchive(zipPath, testLimits(), "tar")
	if err == nil {
		_ = archive.Close()
		t.Fatal("zip file with declared tar mode was accepted")
	}
	testutil.RequireCode(t, err, "INVALID_PACKAGE")

	// A tar file with declared mode "tgz" must be rejected.
	tarPath := writeTar(t, []tarEntry{{name: "a.html", body: "x"}})
	archive, err = deploy.OpenArchive(tarPath, testLimits(), "tgz")
	if err == nil {
		_ = archive.Close()
		t.Fatal("tar file with declared tgz mode was accepted")
	}
	testutil.RequireCode(t, err, "INVALID_PACKAGE")

	// A tgz file with declared mode "zip" must be rejected.
	tgzPath := writeTgz(t, []tarEntry{{name: "a.html", body: "x"}})
	archive, err = deploy.OpenArchive(tgzPath, testLimits(), "zip")
	if err == nil {
		_ = archive.Close()
		t.Fatal("tgz file with declared zip mode was accepted")
	}
	testutil.RequireCode(t, err, "INVALID_PACKAGE")

	// Matching declared mode should pass.
	archive, err = deploy.OpenArchive(tgzPath, testLimits(), "tgz")
	if err != nil {
		t.Fatalf("tgz with matching declared mode: %v", err)
	}
	_ = archive.Close()
}

// readAllCloser reads all bytes and closes the reader.
func readAllCloser(r interface {
	Read([]byte) (int, error)
	Close() error
}) ([]byte, error) {
	defer r.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var _ = strings.TrimSpace
