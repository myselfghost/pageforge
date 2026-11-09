package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pageforge/pageforge/internal/httpx"
	"github.com/pageforge/pageforge/internal/manifest"
	"github.com/pageforge/pageforge/internal/objectstore"
	"github.com/pageforge/pageforge/internal/pageconfig"
	"github.com/pageforge/pageforge/internal/project"
	"github.com/pageforge/pageforge/internal/store"
	"github.com/pageforge/pageforge/internal/version"
)

const packageFileBase = "package"

// packageContentType returns the MIME type for the stored package based on
// the detected format.
func packageContentType(format string) string {
	switch format {
	case "tgz":
		return "application/gzip"
	case "tar":
		return "application/x-tar"
	default:
		return "application/zip"
	}
}

// publish runs after the deployment reached validating. It validates the
// archive, unpacks objects, writes the manifest, archives the package and
// finally switches version, deployment and project pointer in one transaction.
func (s *Service) publish(ctx context.Context, dep Deployment, p project.Project, ver version.Version) error {
	// Detect the actual format before opening the archive, so we can use the
	// correct extension for staging and archival.
	format, err := detectFormat(filepath.Join(dep.StagingPrefix, packageFileBase+".zip"))
	if err != nil {
		// Staging file was written as package.zip by PutPackage; if detection
		// fails the file is likely missing or corrupted.
		if os.IsNotExist(err) {
			return httpx.Conflict("PACKAGE_NOT_UPLOADED", "package file is missing; upload it again")
		}
		return err
	}

	// The staging file is always named package.zip by PutPackage (it doesn't
	// know the format yet). OpenArchive auto-detects from magic bytes.
	packagePath := filepath.Join(dep.StagingPrefix, packageFileBase+".zip")
	info, err := os.Stat(packagePath)
	if err != nil {
		return httpx.Conflict("PACKAGE_NOT_UPLOADED", "package file is missing; upload it again")
	}
	if info.Size() > s.cfg.MaxPackageBytes {
		return limitExceeded("package exceeds the upload limit of %d bytes", s.cfg.MaxPackageBytes)
	}

	archive, err := OpenArchive(packagePath, Limits{
		MaxFiles:      s.cfg.MaxFiles,
		MaxFileBytes:  s.cfg.MaxFileBytes,
		MaxTotalBytes: s.cfg.MaxTotalBytes,
	}, dep.Mode)
	if err != nil {
		return err
	}
	defer func() { _ = archive.Close() }()

	pageCfg, err := s.readPageConfig(archive, p, ver)
	if err != nil {
		return err
	}
	indexDoc, errorDoc := resolveDocs(p, pageCfg)

	objects, err := s.stores(ctx, p)
	if err != nil {
		return httpx.Errorf(503, "STORAGE_UNAVAILABLE", "object store is unavailable: %v", err)
	}

	moved, err := s.transition(ctx, dep.ID, StatusValidating, StatusPreparing)
	if err != nil {
		return err
	}
	if !moved {
		return httpx.Conflict("DEPLOYMENT_IN_PROGRESS", "deployment state changed concurrently")
	}

	builder, err := manifest.NewBuilder(ver.ManifestPrefix, manifest.Limits{
		ShardFiles: s.cfg.ShardFiles,
		ShardBytes: s.cfg.ShardBytes,
	}, filepath.Dir(packagePath))
	if err != nil {
		return err
	}
	defer func() { _ = builder.Close() }()

	objectPrefix := versionPrefix(p.ID, ver.ID)
	if err := s.uploadArchive(ctx, objects, p.Bucket, objectPrefix, archive.Entries(), pageCfg.ContentTypes, builder); err != nil {
		return err
	}

	result, err := builder.Build(ctx, objects, p.Bucket, manifest.RootOptions{
		ProjectID: p.ID,
		VersionID: ver.ID,
		IndexDoc:  indexDoc,
		ErrorDoc:  errorDoc,
	})
	if err != nil {
		return err
	}
	if err := s.verifyUploads(ctx, objects, p.Bucket, objectPrefix, result); err != nil {
		return err
	}

	packageKey, err := s.archivePackage(ctx, objects, p.Bucket, p, ver.ID, packagePath, info.Size(), format)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	err = s.db.WithTx(ctx, func(tx *store.Tx) error {
		// published_at 只在版本真正上线（切换指针）时写入；auto_publish=false
		// 时版本保持 ready 未发布状态，等待后续通过指针接口上线。
		var publishedAt any
		if dep.AutoPublish {
			publishedAt = now
		}
		res, err := tx.ExecContext(ctx, `UPDATE versions
			SET status = ?, manifest_sha256 = ?, file_count = ?, total_bytes = ?,
			    error_message = '', ready_at = ?, published_at = ?
			WHERE id = ? AND project_id = ? AND status = ? AND deleted_at IS NULL`,
			version.StatusReady, result.RootSHA256, result.FileCount, result.TotalBytes,
			now, publishedAt, ver.ID, p.ID, version.StatusUploading)
		if err != nil {
			return fmt.Errorf("mark version ready: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("mark version ready rows: %w", err)
		}
		if n == 0 {
			return httpx.Conflict("VERSION_NOT_AVAILABLE", "version is not in uploading state")
		}
		if dep.AutoPublish {
			if _, err := tx.ExecContext(ctx,
				`UPDATE projects SET current_version_id = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
				ver.ID, now, p.ID); err != nil {
				return fmt.Errorf("switch project pointer: %w", err)
			}
		}
		res, err = tx.ExecContext(ctx, `UPDATE deployments
			SET status = ?, package_key = ?, error_message = '', completed_at = ?
			WHERE id = ? AND status = ?`,
			StatusReady, packageKey, now, dep.ID, StatusPreparing)
		if err != nil {
			return fmt.Errorf("mark deployment ready: %w", err)
		}
		n, err = res.RowsAffected()
		if err != nil {
			return fmt.Errorf("mark deployment ready rows: %w", err)
		}
		if n == 0 {
			return httpx.Conflict("DEPLOYMENT_IN_PROGRESS", "deployment state changed concurrently")
		}
		return nil
	})
	if err != nil {
		return err
	}
	_ = os.RemoveAll(s.stagingDir(dep.ID))
	return nil
}

// readPageConfig loads pageforge.json from the package root when present.
func (s *Service) readPageConfig(archive ArchiveReader, p project.Project, ver version.Version) (pageconfig.Config, error) {
	entry, ok := archive.Config()
	if !ok {
		return pageconfig.Config{ContentTypes: map[string]string{}}, nil
	}
	reader, err := entry.Open()
	if err != nil {
		return pageconfig.Config{}, httpx.Invalid("INVALID_PAGE_CONFIG", "cannot read "+pageconfig.FileName)
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, pageconfig.MaxBytes+1))
	closeErr := reader.Close()
	if readErr != nil {
		return pageconfig.Config{}, httpx.Invalid("INVALID_PAGE_CONFIG", "cannot read "+pageconfig.FileName)
	}
	if closeErr != nil {
		return pageconfig.Config{}, httpx.Invalid("INVALID_PAGE_CONFIG", "cannot read "+pageconfig.FileName)
	}
	if len(data) > pageconfig.MaxBytes {
		return pageconfig.Config{}, httpx.Invalid("INVALID_PAGE_CONFIG",
			pageconfig.FileName+" exceeds the size limit")
	}
	cfg, warnings, err := pageconfig.Parse(data)
	if err != nil {
		return pageconfig.Config{}, err
	}
	for _, warning := range warnings {
		s.logger.Warn(warning,
			slog.String("project_id", p.ID),
			slog.String("version_id", ver.ID),
		)
	}
	return cfg, nil
}

// resolveDocs applies project > package > default precedence. Projects always
// carry a value for index_doc because it has a default, so a project value equal
// to the system default yields to the package.
func resolveDocs(p project.Project, cfg pageconfig.Config) (string, string) {
	indexDoc := p.IndexDoc
	if (indexDoc == "" || indexDoc == defaultIndexDoc) && cfg.IndexDoc != nil {
		indexDoc = *cfg.IndexDoc
	}
	if indexDoc == "" {
		indexDoc = defaultIndexDoc
	}
	errorDoc := p.ErrorDoc
	if errorDoc == "" && cfg.ErrorDoc != nil {
		errorDoc = *cfg.ErrorDoc
	}
	return indexDoc, errorDoc
}

// uploadArchive unpacks every entry into the version prefix while hashing it.
func (s *Service) uploadArchive(ctx context.Context, objects objectstore.ObjectStore, bucket, prefix string,
	entries []ArchiveEntry, overrides map[string]string, builder *manifest.Builder) error {
	workers := s.cfg.Workers
	if workers > len(entries) {
		workers = len(entries)
	}
	if workers == 0 {
		return nil
	}

	uploadCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan int)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	record := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		mu.Unlock()
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				if err := s.uploadEntry(uploadCtx, objects, bucket, prefix, entries[index], overrides, builder, &mu); err != nil {
					record(err)
					return
				}
			}
		}()
	}

send:
	for index := range entries {
		select {
		case jobs <- index:
		case <-uploadCtx.Done():
			break send
		}
	}
	close(jobs)
	wg.Wait()

	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

func (s *Service) uploadEntry(ctx context.Context, objects objectstore.ObjectStore, bucket, prefix string,
	entry ArchiveEntry, overrides map[string]string, builder *manifest.Builder, mu *sync.Mutex) error {
	reader, err := entry.Open()
	if err != nil {
		return fmt.Errorf("open package entry %q: %w", entry.Key, err)
	}
	defer func() { _ = reader.Close() }()

	digest := sha256.New()
	contentType, encoding := contentTypeFor(entry.Key, overrides)
	s3Key := prefix + "/" + StorageKey(entry.Key)
	err = objects.Put(ctx, bucket, s3Key, io.TeeReader(reader, digest), entry.Size,
		objectstore.PutOptions{ContentType: contentType, Encoding: encoding})
	if err != nil {
		return fmt.Errorf("upload %q: %w", entry.Key, err)
	}

	modTime := entry.ModTime
	if modTime.IsZero() {
		modTime = time.Now().UTC()
	}
	mu.Lock()
	defer mu.Unlock()
	return builder.Add(manifest.Entry{
		Key:         entry.Key,
		Size:        entry.Size,
		SHA256:      hex.EncodeToString(digest.Sum(nil)),
		ContentType: contentType,
		ModTime:     modTime.Unix(),
		Encoding:    encoding,
	})
}

// contentTypeFor resolves the object content type, honouring package overrides
// and pre-compressed .br/.gz suffixes.
func contentTypeFor(key string, overrides map[string]string) (string, string) {
	base := key
	encoding := ""
	lower := strings.ToLower(key)
	switch {
	case strings.HasSuffix(lower, ".br"):
		encoding = "br"
		base = key[:len(key)-len(".br")]
	case strings.HasSuffix(lower, ".gz"):
		encoding = "gz"
		base = key[:len(key)-len(".gz")]
	}
	ext := strings.TrimPrefix(strings.ToLower(path.Ext(base)), ".")
	if ext != "" {
		if contentType, ok := overrides[ext]; ok && contentType != "" {
			return contentType, encoding
		}
		if contentType := mime.TypeByExtension("." + ext); contentType != "" {
			return contentType, encoding
		}
	}
	return "application/octet-stream", encoding
}

// verifyUploads lists the version prefix and compares counts and sizes with the
// manifest that was just generated.
func (s *Service) verifyUploads(ctx context.Context, objects objectstore.ObjectStore, bucket, prefix string,
	result manifest.Result) error {
	var count, bytes int64
	err := objects.List(ctx, bucket, prefix+"/", func(info objectstore.ObjectInfo) error {
		count++
		bytes += info.Size
		return nil
	})
	if err != nil {
		return fmt.Errorf("verify uploaded objects: %w", err)
	}
	if count != result.FileCount || bytes != result.TotalBytes {
		return httpx.Errorf(500, "UPLOAD_VERIFY_FAILED",
			"uploaded %d objects/%d bytes, expected %d/%d",
			count, bytes, result.FileCount, result.TotalBytes)
	}
	return nil
}

// archivePackage stores the original package under _system/packages when the
// project keeps packages. format is the detected archive format ("zip", "tar",
// or "tgz") and determines the storage key extension and content type.
func (s *Service) archivePackage(ctx context.Context, objects objectstore.ObjectStore, bucket string,
	p project.Project, versionID, packagePath string, size int64, format string) (string, error) {
	if !p.PackageRetention {
		return "", nil
	}
	file, err := os.Open(packagePath)
	if err != nil {
		return "", fmt.Errorf("open package for archiving: %w", err)
	}
	defer func() { _ = file.Close() }()

	key := strings.Trim(s.cfg.PackagePrefix, "/") + "/" + p.ID + "/" + versionID + packageExtensionForFormat(format)
	if err := objects.Put(ctx, bucket, key, file, size, objectstore.PutOptions{ContentType: packageContentType(format)}); err != nil {
		return "", fmt.Errorf("archive package: %w", err)
	}
	return key, nil
}

// packageExtensionForFormat returns the storage key suffix for the detected
// format.
func packageExtensionForFormat(format string) string {
	switch format {
	case "tar":
		return ".tar"
	case "tgz":
		return ".tar.gz"
	default:
		return ".zip"
	}
}

func versionPrefix(projectID, versionID string) string {
	return "projects/" + projectID + "/" + versionID
}
