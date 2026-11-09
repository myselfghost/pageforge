// Package deploy implements publish-package validation, unpacking and the
// control-plane publish pipeline.
package deploy

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pageforge/pageforge/internal/httpx"
	"github.com/pageforge/pageforge/internal/pageconfig"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// reservedRoot is the storage prefix that user packages must not write into.
const reservedRoot = "_system"

// Limits bounds what a single package may contain.
type Limits struct {
	MaxFiles      int
	MaxFileBytes  int64
	MaxTotalBytes int64
}

// ArchiveEntry is one regular file inside a publish package.
type ArchiveEntry struct {
	// Key is the project-relative object key using forward slashes.
	Key     string
	Size    int64
	ModTime time.Time
	open    func() (io.ReadCloser, error)
}

// Open returns a fresh reader for the entry. Zip entries support concurrent
// readers, which lets the upload pool decompress files in parallel.
func (e ArchiveEntry) Open() (io.ReadCloser, error) {
	return e.open()
}

// ArchiveReader is the format-independent unpacking contract. Both zip and
// tar/tgz implementations satisfy this interface.
type ArchiveReader interface {
	// Entries returns the deployable files in archive order.
	Entries() []ArchiveEntry
	// Config returns the package configuration file when present. The file is
	// never part of Entries and is not published.
	Config() (ArchiveEntry, bool)
	Close() error
}

// Archive is the validated zip representation of a publish package.
type Archive struct {
	file    *zip.ReadCloser
	entries []ArchiveEntry
	config  *ArchiveEntry
	total   int64
}

var _ ArchiveReader = (*Archive)(nil)

// ValidateArchiveKey validates a zip entry name as a project-relative object
// key. Names are decoded to UTF-8 before this check (see decodeEntryName), so
// every stored key can later be matched against a URL request path.
func ValidateArchiveKey(name string) error {
	if name == "" {
		return fmt.Errorf("key must not be empty")
	}
	if strings.HasPrefix(name, "/") {
		return fmt.Errorf("key must not start with /")
	}
	if strings.ContainsAny(name, "\\\x00") {
		return fmt.Errorf("key contains a backslash or NUL character")
	}
	if strings.HasSuffix(name, "/") {
		return fmt.Errorf("key must not end with /")
	}
	for _, seg := range strings.Split(name, "/") {
		switch seg {
		case "":
			return fmt.Errorf("empty path segment is not allowed")
		case ".", "..":
			return fmt.Errorf("relative path segment is not allowed")
		}
		for i := 0; i < len(seg); i++ {
			if seg[i] < 0x20 || seg[i] == 0x7f {
				return fmt.Errorf("path segment %q contains a control character", seg)
			}
		}
		// Object stores backed by a Windows filesystem reject names containing
		// these characters (MinIO answers "Object name contains unsupported
		// characters"), so catch them during validation instead of failing
		// midway through object upload.
		if i := strings.IndexAny(seg, `<>:"|?*`); i >= 0 {
			return fmt.Errorf("path segment %q contains the unsupported character %q", seg, seg[i])
		}
	}
	return nil
}

func invalidPackage(format string, args ...any) error {
	return httpx.Invalid("INVALID_PACKAGE", fmt.Sprintf(format, args...))
}

func limitExceeded(format string, args ...any) error {
	return httpx.Errorf(http.StatusRequestEntityTooLarge, "PACKAGE_LIMIT_EXCEEDED", format, args...)
}

// detectFormat reads the first two magic bytes of path and returns the
// detected format string ("zip", "tgz", or "tar").
func detectFormat(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", invalidPackage("cannot open package: %v", err)
	}
	defer f.Close()
	var buf [2]byte
	_, _ = f.Read(buf[:])
	switch {
	case buf[0] == 'P' && buf[1] == 'K':
		return "zip", nil
	case buf[0] == 0x1f && buf[1] == 0x8b:
		return "tgz", nil
	default:
		return "tar", nil
	}
}

// OpenArchive opens path and validates it against limits. The archive format
// is auto-detected from magic bytes: PK\x03\x04 → zip, \x1f\x8b → tgz,
// otherwise → tar. If declaredMode is non-empty and not "auto", the detected
// format must match or INVALID_PACKAGE is returned.
func OpenArchive(path string, limits Limits, declaredMode string) (ArchiveReader, error) {
	detected, err := detectFormat(path)
	if err != nil {
		return nil, err
	}
	if declaredMode != "" && declaredMode != "auto" && declaredMode != detected {
		return nil, invalidPackage("declared mode %q does not match detected format %q", declaredMode, detected)
	}
	switch detected {
	case "zip":
		return OpenZip(path, limits)
	case "tgz":
		return OpenTgz(path, limits)
	default:
		return OpenTar(path, limits)
	}
}

// OpenZip opens and fully validates a zip package. All entries are checked
// before any content is uploaded, so a malicious archive is rejected as a whole.
func OpenZip(path string, limits Limits) (*Archive, error) {
	file, err := zip.OpenReader(path)
	if err != nil {
		return nil, invalidPackage("package is not a valid zip archive: %v", err)
	}
	archive := &Archive{file: file}
	if err := archive.validate(limits); err != nil {
		_ = file.Close()
		return nil, err
	}
	return archive, nil
}

// OpenTar opens and fully validates an uncompressed tar package.
func OpenTar(path string, limits Limits) (*TarArchive, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, invalidPackage("cannot open package: %v", err)
	}
	arc, err := openTarFromReader(f, f, limits)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return arc, nil
}

// OpenTgz opens and fully validates a gzip-compressed tar package.
func OpenTgz(path string, limits Limits) (*TarArchive, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, invalidPackage("cannot open package: %v", err)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		_ = f.Close()
		return nil, invalidPackage("package is not a valid gzip stream: %v", err)
	}
	arc, err := openTarFromReader(gz, f, limits)
	if err != nil {
		_ = gz.Close()
		_ = f.Close()
		return nil, err
	}
	return arc, nil
}

// openTarFromReader validates a tar stream from reader and builds the
// TarArchive. closer releases the underlying resources when the archive is
// closed. For plain tar, reader and closer are the same *os.File; for tgz,
// reader is the *gzip.Reader and closer is the *os.File (closing the file
// implicitly closes the gzip reader).
func openTarFromReader(reader io.Reader, closer io.Closer, limits Limits) (*TarArchive, error) {
	arc := &TarArchive{reader: tar.NewReader(reader), closer: closer}
	if err := arc.validate(limits); err != nil {
		return nil, err
	}
	return arc, nil
}

// TarArchive is the validated tar or tgz representation of a publish package.
// Tar is a sequential format, so entry content is read eagerly into memory
// during validation. Each ArchiveEntry's open closure captures the in-memory
// data, making concurrent pipeline uploads safe without needing to seek the
// underlying tar.Reader.
type TarArchive struct {
	reader  *tar.Reader
	closer  io.Closer
	entries []ArchiveEntry
	config  *ArchiveEntry
	total   int64
}

var _ ArchiveReader = (*TarArchive)(nil)

// validate reads every tar header, validates it, and eagerly reads the file
// content into memory. Because tar is a sequential format, this is the only
// point where the underlying tar.Reader is accessed. After validate returns,
// the reader is at the end of the archive and is no longer needed.
func (a *TarArchive) validate(limits Limits) error {
	seen := make(map[string]string, 128)
	for {
		header, err := a.reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return invalidPackage("malformed tar archive: %v", err)
		}

		name := header.Name
		// Handle PAX/GNU long name extensions: the Go stdlib already resolves
		// them into header.Name, so no extra handling is needed.

		// Decode non-UTF-8 filenames (defensive; tar PAX uses UTF-8 but
		// legacy tar may contain raw bytes).
		name, err = decodeEntryName(name)
		if err != nil {
			return invalidPackage("entry %q: %v", header.Name, err)
		}

		// Skip directory entries.
		if header.Typeflag == tar.TypeDir || strings.HasSuffix(name, "/") {
			// Drain the directory entry's (empty) content.
			_, _ = io.Copy(io.Discard, a.reader)
			continue
		}

		switch header.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			// regular file — OK
		case tar.TypeSymlink, tar.TypeLink:
			return invalidPackage("entry %q is a symbolic or hard link", name)
		default:
			return invalidPackage("entry %q is not a regular file (type 0x%02x)", name, header.Typeflag)
		}

		if len(name) > httpx.MaxKeyLen {
			return invalidPackage("entry %q exceeds the maximum key length", name)
		}
		if strings.ContainsAny(name, "\\\x00") {
			return invalidPackage("entry %q contains a backslash or NUL character", name)
		}
		if strings.HasPrefix(name, "/") {
			return invalidPackage("entry %q uses an absolute path", name)
		}
		if err := ValidateArchiveKey(name); err != nil {
			return invalidPackage("entry %q is not a valid package path: %v", name, err)
		}
		if pageconfig.IsConfigName(path.Base(name)) && !isRootFile(name) {
			return invalidPackage("%s must be at the package root", pageconfig.FileName)
		}
		if isReserved(name) {
			return invalidPackage("entry %q writes into the reserved %s prefix", name, reservedRoot)
		}
		if header.Size > int64(limits.MaxFileBytes) {
			return limitExceeded("entry %q exceeds the per-file size limit of %d bytes",
				name, limits.MaxFileBytes)
		}
		if previous, exists := seen[casefold(name)]; exists {
			return invalidPackage("entries %q and %q differ only by case or are duplicated", previous, name)
		}
		seen[casefold(name)] = name

		// Read content eagerly into memory (tar is sequential; concurrent
		// access is not possible without buffering).
		data, err := io.ReadAll(io.LimitReader(a.reader, header.Size+1))
		if err != nil {
			return invalidPackage("cannot read entry %q: %v", name, err)
		}
		if int64(len(data)) > header.Size {
			return invalidPackage("entry %q is larger than its header declares", name)
		}

		modTime := header.ModTime
		entry := ArchiveEntry{
			Key:     name,
			Size:    header.Size,
			ModTime: modTime,
			open:    func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil },
		}

		if pageconfig.IsConfigName(name) {
			a.config = &entry
			continue
		}

		a.total += header.Size
		if a.total > limits.MaxTotalBytes {
			return limitExceeded("package exceeds the total size limit of %d bytes", limits.MaxTotalBytes)
		}
		a.entries = append(a.entries, entry)
		if limits.MaxFiles > 0 && len(a.entries) > limits.MaxFiles {
			return limitExceeded("package exceeds the file count limit of %d", limits.MaxFiles)
		}
	}
	return nil
}

// Entries returns the deployable files.
func (a *TarArchive) Entries() []ArchiveEntry {
	return a.entries
}

// Config returns the package configuration file when present.
func (a *TarArchive) Config() (ArchiveEntry, bool) {
	if a.config == nil {
		return ArchiveEntry{}, false
	}
	return *a.config, true
}

// FileCount returns the number of deployable files.
func (a *TarArchive) FileCount() int {
	return len(a.entries)
}

// TotalBytes returns the sum of the declared file sizes.
func (a *TarArchive) TotalBytes() int64 {
	return a.total
}

// Close releases the underlying archive file.
func (a *TarArchive) Close() error {
	if a.closer == nil {
		return nil
	}
	return a.closer.Close()
}

func (a *Archive) validate(limits Limits) error {
	seen := make(map[string]string, len(a.file.File))
	for _, f := range a.file.File {
		name, err := decodeEntryName(f.Name)
		if err != nil {
			return invalidPackage("entry %q: %v", f.Name, err)
		}
		if strings.HasSuffix(name, "/") || f.FileInfo().IsDir() {
			continue
		}
		if f.Flags&0x1 != 0 {
			return invalidPackage("entry %q is encrypted", name)
		}
		if mode := f.Mode(); mode&os.ModeSymlink != 0 {
			return invalidPackage("entry %q is a symbolic link", name)
		} else if !mode.IsRegular() {
			return invalidPackage("entry %q is not a regular file", name)
		}
		if len(name) > httpx.MaxKeyLen {
			return invalidPackage("entry %q exceeds the maximum key length", name)
		}
		if strings.ContainsAny(name, "\\\x00") {
			return invalidPackage("entry %q contains a backslash or NUL character", name)
		}
		if strings.HasPrefix(name, "/") {
			return invalidPackage("entry %q uses an absolute path", name)
		}
		if err := ValidateArchiveKey(name); err != nil {
			return invalidPackage("entry %q is not a valid package path: %v", name, err)
		}
		if pageconfig.IsConfigName(path.Base(name)) && !isRootFile(name) {
			return invalidPackage("%s must be at the package root", pageconfig.FileName)
		}
		if isReserved(name) {
			return invalidPackage("entry %q writes into the reserved %s prefix", name, reservedRoot)
		}
		if f.UncompressedSize64 > uint64(limits.MaxFileBytes) {
			return limitExceeded("entry %q exceeds the per-file size limit of %d bytes",
				name, limits.MaxFileBytes)
		}
		if previous, exists := seen[casefold(name)]; exists {
			return invalidPackage("entries %q and %q differ only by case or are duplicated", previous, name)
		}
		seen[casefold(name)] = name

		if pageconfig.IsConfigName(name) {
			if !isRootFile(name) {
				return invalidPackage("%s must be at the package root", pageconfig.FileName)
			}
			entry := ArchiveEntry{Key: name, Size: int64(f.UncompressedSize64), ModTime: f.Modified, open: f.Open}
			a.config = &entry
			continue
		}

		a.total += int64(f.UncompressedSize64)
		if a.total > limits.MaxTotalBytes {
			return limitExceeded("package exceeds the total size limit of %d bytes", limits.MaxTotalBytes)
		}
		a.entries = append(a.entries, ArchiveEntry{
			Key: name, Size: int64(f.UncompressedSize64), ModTime: f.Modified, open: f.Open,
		})
		if limits.MaxFiles > 0 && len(a.entries) > limits.MaxFiles {
			return limitExceeded("package exceeds the file count limit of %d", limits.MaxFiles)
		}
	}
	return nil
}

// decodeEntryName normalizes a zip entry name to UTF-8. Windows tools often
// store Chinese filenames in the local ANSI codepage (GBK) without the zip
// UTF-8 flag, so Go returns the raw bytes. Keeping those bytes would produce
// manifest keys and object keys that no UTF-8 URL request can ever match.
// GB18030 is a superset of GBK, so it covers both encodings.
func decodeEntryName(name string) (string, error) {
	if utf8.ValidString(name) {
		return name, nil
	}
	decoded, err := simplifiedchinese.GB18030.NewDecoder().String(name)
	if err != nil {
		return "", fmt.Errorf("name is neither valid UTF-8 nor GB18030: %v", err)
	}
	if strings.ContainsRune(decoded, '\uFFFD') {
		return "", fmt.Errorf("name is neither valid UTF-8 nor GB18030")
	}
	return decoded, nil
}

// casefold returns a case-insensitive comparison key. Entry names are always
// UTF-8 after decodeEntryName, so lowercasing detects Windows case collisions.
func casefold(s string) string {
	return strings.ToLower(s)
}

func isRootFile(name string) bool {
	return !strings.Contains(name, "/")
}

func isReserved(name string) bool {
	first, _, _ := strings.Cut(name, "/")
	return strings.EqualFold(first, reservedRoot)
}

// Entries returns the deployable files.
func (a *Archive) Entries() []ArchiveEntry {
	return a.entries
}

// Config returns the package configuration file when present.
func (a *Archive) Config() (ArchiveEntry, bool) {
	if a.config == nil {
		return ArchiveEntry{}, false
	}
	return *a.config, true
}

// FileCount returns the number of deployable files.
func (a *Archive) FileCount() int {
	return len(a.entries)
}

// TotalBytes returns the sum of the declared uncompressed file sizes.
func (a *Archive) TotalBytes() int64 {
	return a.total
}

// StorageKey returns a minio-go-compatible S3 object key for a project-relative
// entry name. Valid-UTF-8 names (including Chinese, Japanese, etc.) pass through
// unchanged. Segments containing invalid-UTF-8 bytes (e.g. GBK-encoded filenames
// from Windows zips) get all non-ASCII bytes percent-encoded, producing a
// valid-UTF-8 key that minio-go will accept.
func StorageKey(name string) string {
	if utf8.ValidString(name) {
		return name
	}
	segs := strings.Split(name, "/")
	for i, seg := range segs {
		if utf8.ValidString(seg) {
			continue
		}
		var buf strings.Builder
		for j := 0; j < len(seg); j++ {
			c := seg[j]
			if c < 0x80 {
				buf.WriteByte(c)
			} else {
				fmt.Fprintf(&buf, "%%%02X", c)
			}
		}
		segs[i] = buf.String()
	}
	return strings.Join(segs, "/")
}

// FromStorageKey reverses StorageKey: it percent-decodes each path segment.
// Segments that are not valid percent-encoding are returned unchanged.
func FromStorageKey(key string) string {
	segs := strings.Split(key, "/")
	for i, seg := range segs {
		if decoded, err := url.PathUnescape(seg); err == nil {
			segs[i] = decoded
		}
	}
	return strings.Join(segs, "/")
}

// Close releases the underlying archive file.
func (a *Archive) Close() error {
	if a.file == nil {
		return nil
	}
	return a.file.Close()
}
