// Package gc removes expired versions, orphaned objects and stale upload
// staging directories. Every operation is idempotent and supports dry-run.
package gc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/pageforge/pageforge/internal/objectstore"
	"github.com/pageforge/pageforge/internal/project"
	"github.com/pageforge/pageforge/internal/store"
	"github.com/pageforge/pageforge/internal/version"
)

// StoreFactory returns the object store for a project's alias and bucket.
type StoreFactory func(ctx context.Context, p project.Project) (objectstore.ObjectStore, error)

// Config controls retention.
type Config struct {
	Interval      time.Duration
	OrphanGrace   time.Duration
	KeepVersions  int
	KeepDays      int
	PackagePrefix string
}

func (c Config) withDefaults() Config {
	if c.Interval <= 0 {
		c.Interval = time.Hour
	}
	if c.OrphanGrace <= 0 {
		c.OrphanGrace = 24 * time.Hour
	}
	if c.PackagePrefix == "" {
		c.PackagePrefix = "_system/packages"
	}
	return c
}

// Service runs retention.
type Service struct {
	db       *store.Store
	projects *project.Repo
	versions *version.Repo
	stores   StoreFactory
	cfg      Config
	logger   *slog.Logger
}

// NewService builds a GC service.
func NewService(db *store.Store, stores StoreFactory, cfg Config, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		db:       db,
		projects: project.NewRepo(db),
		versions: version.NewRepo(db),
		stores:   stores,
		cfg:      cfg.withDefaults(),
		logger:   logger,
	}
}

// Report summarises one GC run.
type Report struct {
	DryRun        bool     `json:"dry_run"`
	Versions      int      `json:"versions_deleted"`
	Objects       int      `json:"objects_deleted"`
	OrphansPurged int      `json:"orphans_purged"`
	StagingDirs   int      `json:"staging_cleaned"`
	Errors        []string `json:"errors,omitempty"`
}

// RunLoop runs GC immediately and then on the configured interval until ctx is done.
func (s *Service) RunLoop(ctx context.Context) {
	s.runOnce(ctx)
	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runOnce(ctx)
		}
	}
}

func (s *Service) runOnce(ctx context.Context) {
	start := time.Now()
	s.logger.Info("gc run started", slog.Time("started_at", start.UTC()))
	report, err := s.Run(ctx, false)
	if err != nil {
		s.logger.Error("gc run failed",
			slog.Time("started_at", start.UTC()), slog.String("error", err.Error()))
		return
	}
	s.logger.Info("gc run finished",
		slog.Time("started_at", start.UTC()),
		slog.Duration("elapsed", time.Since(start)),
		slog.Int("versions_deleted", report.Versions),
		slog.Int("objects_deleted", report.Objects),
		slog.Int("orphans_purged", report.OrphansPurged),
		slog.Int("staging_cleaned", report.StagingDirs),
		slog.Int("errors", len(report.Errors)),
	)
	for _, e := range report.Errors {
		s.logger.Error("gc run error", slog.String("detail", e))
	}
}

// Run applies retention for every project. With dryRun set it only reports.
func (s *Service) Run(ctx context.Context, dryRun bool) (Report, error) {
	report := Report{DryRun: dryRun}
	projects, err := s.projects.List(ctx)
	if err != nil {
		return report, err
	}
	now := time.Now().UTC()
	for _, p := range projects {
		if err := s.collectProject(ctx, p, now, dryRun, &report); err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("project %s: %v", p.ID, err))
		}
	}
	if err := s.cleanStaging(ctx, now, dryRun, &report); err != nil {
		report.Errors = append(report.Errors, err.Error())
	}
	return report, nil
}

func (s *Service) collectProject(ctx context.Context, p project.Project, now time.Time, dryRun bool, report *Report) error {
	versions, err := s.versions.List(ctx, p.ID)
	if err != nil {
		return err
	}
	pinned, err := s.pinnedVersions(ctx, p.ID)
	if err != nil {
		return err
	}
	// 活跃分享链接指向的版本同样受保护，避免 GC 后分享链接失效。
	shared, err := s.shareLinkedVersions(ctx, p.ID, now)
	if err != nil {
		return err
	}

	var objects objectstore.ObjectStore
	storeFor := func() (objectstore.ObjectStore, error) {
		if objects != nil {
			return objects, nil
		}
		if s.stores == nil {
			return nil, errors.New("object store is not configured")
		}
		objects, err = s.stores(ctx, p)
		return objects, err
	}

	// 已清理过对象的版本（objects_purged_at 非空）无需重复处理。
	purged, err := s.purgedVersionIDs(ctx, p.ID)
	if err != nil {
		return err
	}

	// Failed or abandoned uploads only leave orphan objects behind.
	for _, v := range versions {
		if v.Status == version.StatusReady || now.Sub(v.CreatedAt) < s.cfg.OrphanGrace {
			continue
		}
		if _, ok := purged[v.ID]; ok {
			continue
		}
		st, err := storeFor()
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("project %s version %s: %v", p.ID, v.ID, err))
			s.logger.Error("gc remove orphan objects failed",
				slog.String("project_id", p.ID), slog.String("version_id", v.ID),
				slog.String("status", string(v.Status)), slog.String("error", err.Error()))
			continue
		}
		count, err := s.removeVersionObjects(ctx, st, p, v, dryRun)
		report.Objects += count
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("project %s version %s: %v", p.ID, v.ID, err))
			s.logger.Error("gc remove orphan objects failed",
				slog.String("project_id", p.ID), slog.String("version_id", v.ID),
				slog.Int("objects", count), slog.String("error", err.Error()))
			continue
		}
		if !dryRun {
			if err := s.markObjectsPurged(ctx, v.ID, now); err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("project %s version %s: %v", p.ID, v.ID, err))
				s.logger.Error("gc mark orphan objects purged failed",
					slog.String("project_id", p.ID), slog.String("version_id", v.ID), slog.String("error", err.Error()))
				continue
			}
		}
		s.logger.Info("gc purged orphan objects",
			slog.String("project_id", p.ID), slog.String("version_id", v.ID),
			slog.String("status", string(v.Status)), slog.Int("objects", count))
		report.OrphansPurged++
	}

	// 手动软删除（DELETE /versions/{id}）的版本：超过 orphan_grace 后移除其对象。
	deleted, err := s.softDeletedVersions(ctx, p.ID, now.Add(-s.cfg.OrphanGrace))
	if err != nil {
		return err
	}
	for _, v := range deleted {
		st, err := storeFor()
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("project %s version %s: %v", p.ID, v.ID, err))
			s.logger.Error("gc purge soft-deleted objects failed",
				slog.String("project_id", p.ID), slog.String("version_id", v.ID), slog.String("error", err.Error()))
			continue
		}
		count, err := s.removeVersionObjects(ctx, st, p, v, dryRun)
		report.Objects += count
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("project %s version %s: %v", p.ID, v.ID, err))
			s.logger.Error("gc purge soft-deleted objects failed",
				slog.String("project_id", p.ID), slog.String("version_id", v.ID),
				slog.Int("objects", count), slog.String("error", err.Error()))
			continue
		}
		if !dryRun {
			if err := s.markObjectsPurged(ctx, v.ID, now); err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("project %s version %s: %v", p.ID, v.ID, err))
				s.logger.Error("gc mark soft-deleted objects purged failed",
					slog.String("project_id", p.ID), slog.String("version_id", v.ID), slog.String("error", err.Error()))
				continue
			}
		}
		s.logger.Info("gc purged soft-deleted objects",
			slog.String("project_id", p.ID), slog.String("version_id", v.ID), slog.Int("objects", count))
	}

	// 项目级保留策略优先于全局配置；未设置时（nil）继承全局。
	keepVersions := s.cfg.KeepVersions
	if p.KeepVersions != nil {
		keepVersions = *p.KeepVersions
	}
	keepDays := s.cfg.KeepDays
	if p.KeepDays != nil {
		keepDays = *p.KeepDays
	}

	deletable := retentionSet(versions, p, pinned, shared, keepVersions, keepDays, now)
	if len(deletable) == 0 {
		return nil
	}
	st, err := storeFor()
	if err != nil {
		return err
	}
	for _, v := range deletable {
		count, err := s.removeVersionObjects(ctx, st, p, v, dryRun)
		report.Objects += count
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("project %s version %s: %v", p.ID, v.ID, err))
			s.logger.Error("gc delete version objects failed",
				slog.String("project_id", p.ID), slog.String("version_id", v.ID),
				slog.Int("objects", count), slog.String("error", err.Error()))
			continue
		}
		report.Versions++
		s.logger.Info("gc deleted version",
			slog.String("project_id", p.ID), slog.String("version_id", v.ID),
			slog.Int("objects", count), slog.Bool("dry_run", dryRun))
		if dryRun {
			continue
		}
		// 对象已清空；一并写入软删除标记与清理标记，保证幂等、避免下轮重复处理。
		if err := s.markVersionDeleted(ctx, v.ID, now); err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("project %s version %s: %v", p.ID, v.ID, err))
			s.logger.Error("gc mark version deleted failed",
				slog.String("project_id", p.ID), slog.String("version_id", v.ID), slog.String("error", err.Error()))
		}
	}
	return nil
}

// markObjectsPurged 记录对象已被物理清理，使后续 GC 跳过该版本。
func (s *Service) markObjectsPurged(ctx context.Context, versionID string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE versions SET objects_purged_at = ? WHERE id = ? AND objects_purged_at IS NULL`,
		now, versionID)
	return err
}

// markVersionDeleted 在对象清理成功后写入软删除标记与清理标记（幂等）。
func (s *Service) markVersionDeleted(ctx context.Context, versionID string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE versions SET deleted_at = ?, objects_purged_at = ?
		 WHERE id = ? AND deleted_at IS NULL`, now, now, versionID)
	return err
}

// retentionSet picks ready versions outside the retention window. Versions that
// are current, pinned by a binding, referenced by an active share link or
// explicitly protected are never deleted.
func retentionSet(versions []version.Version, p project.Project, pinned, shared map[string]struct{},
	keepVersions, keepDays int, now time.Time) []version.Version {
	if keepVersions <= 0 && keepDays <= 0 {
		return nil
	}
	var out []version.Version
	index := 0
	for _, v := range versions {
		if v.Status != version.StatusReady {
			continue
		}
		position := index
		index++
		if keepVersions > 0 && position < keepVersions {
			continue
		}
		if keepDays > 0 && now.Sub(v.CreatedAt) < time.Duration(keepDays)*24*time.Hour {
			continue
		}
		if v.ID == p.CurrentVersionID || v.Protected {
			continue
		}
		if _, ok := pinned[v.ID]; ok {
			continue
		}
		if _, ok := shared[v.ID]; ok {
			continue
		}
		out = append(out, v)
	}
	return out
}

// purgedVersionIDs 返回该项目中 objects_purged_at 非空（对象已物理清理）的版本 ID，
// 供孤儿对象清理循环跳过，避免每轮 GC 重复扫描与重复日志。
func (s *Service) purgedVersionIDs(ctx context.Context, projectID string) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM versions WHERE project_id = ? AND objects_purged_at IS NOT NULL`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list purged versions: %w", err)
	}
	defer rows.Close()

	out := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan purged version: %w", err)
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

// softDeletedVersions 返回已软删除、删除时间早于 cutoff 且对象尚未物理清理的版本
// （只需 id 与 manifest_prefix）。objects_purged_at 非空表示对象已清理，跳过以避免重复劳动。
func (s *Service) softDeletedVersions(ctx context.Context, projectID string, cutoff time.Time) ([]version.Version, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, manifest_prefix FROM versions
		 WHERE project_id = ? AND deleted_at IS NOT NULL AND deleted_at < ?
		   AND objects_purged_at IS NULL`, projectID, cutoff)
	if err != nil {
		return nil, fmt.Errorf("list soft-deleted versions: %w", err)
	}
	defer rows.Close()

	out := []version.Version{}
	for rows.Next() {
		var v version.Version
		if err := rows.Scan(&v.ID, &v.ManifestPrefix); err != nil {
			return nil, fmt.Errorf("scan soft-deleted version: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Service) pinnedVersions(ctx context.Context, projectID string) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT pinned_version_id FROM bindings
		 WHERE project_id = ? AND deleted_at IS NULL AND pinned_version_id IS NOT NULL
		   AND pinned_version_id <> ''`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list pinned versions: %w", err)
	}
	defer rows.Close()

	pinned := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan pinned version: %w", err)
		}
		pinned[id] = struct{}{}
	}
	return pinned, rows.Err()
}

// shareLinkedVersions returns versions referenced by an enabled, unexpired and
// not-deleted share link. Expired or disabled links do not hold a version back.
func (s *Service) shareLinkedVersions(ctx context.Context, projectID string, now time.Time) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT version_id FROM share_links
		 WHERE project_id = ? AND deleted_at IS NULL AND enabled = ?
		   AND version_id IS NOT NULL AND version_id <> ''
		   AND (expires_at IS NULL OR expires_at > ?)`,
		projectID, true, now)
	if err != nil {
		return nil, fmt.Errorf("list share-linked versions: %w", err)
	}
	defer rows.Close()

	shared := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan share-linked version: %w", err)
		}
		shared[id] = struct{}{}
	}
	return shared, rows.Err()
}

func (s *Service) removeVersionObjects(ctx context.Context, st objectstore.ObjectStore, p project.Project,
	v version.Version, dryRun bool) (int, error) {
	total := 0
	prefixes := []string{
		"projects/" + p.ID + "/" + v.ID + "/",
		strings.Trim(v.ManifestPrefix, "/") + "/",
	}
	for _, prefix := range prefixes {
		count, err := s.removePrefix(ctx, st, p.Bucket, prefix, dryRun)
		total += count
		if err != nil {
			return total, err
		}
	}
	packageKey := strings.Trim(s.cfg.PackagePrefix, "/") + "/" + p.ID + "/" + v.ID + ".zip"
	count, err := s.removeKey(ctx, st, p.Bucket, packageKey, dryRun)
	total += count
	if err != nil {
		return total, err
	}
	return total, nil
}

func (s *Service) removePrefix(ctx context.Context, st objectstore.ObjectStore, bucket, prefix string, dryRun bool) (int, error) {
	var keys []string
	if err := st.List(ctx, bucket, prefix, func(info objectstore.ObjectInfo) error {
		keys = append(keys, info.Key)
		return nil
	}); err != nil {
		return 0, fmt.Errorf("list %s: %w", prefix, err)
	}
	if dryRun {
		return len(keys), nil
	}
	for _, key := range keys {
		if err := st.Remove(ctx, bucket, key); err != nil && !errors.Is(err, objectstore.ErrNotFound) {
			return 0, fmt.Errorf("remove %s: %w", key, err)
		}
	}
	return len(keys), nil
}

func (s *Service) removeKey(ctx context.Context, st objectstore.ObjectStore, bucket, key string, dryRun bool) (int, error) {
	if _, err := st.Stat(ctx, bucket, key); err != nil {
		if errors.Is(err, objectstore.ErrNotFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("stat %s: %w", key, err)
	}
	if dryRun {
		return 1, nil
	}
	if err := st.Remove(ctx, bucket, key); err != nil && !errors.Is(err, objectstore.ErrNotFound) {
		return 0, fmt.Errorf("remove %s: %w", key, err)
	}
	return 1, nil
}

// cleanStaging drops local upload directories left by finished or abandoned
// deployments.
func (s *Service) cleanStaging(ctx context.Context, now time.Time, dryRun bool, report *Report) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, status, staging_prefix FROM deployments
		 WHERE staging_prefix <> '' AND created_at < ?`, now.Add(-s.cfg.OrphanGrace))
	if err != nil {
		return fmt.Errorf("list staging deployments: %w", err)
	}
	type staged struct {
		id     string
		status string
		dir    string
	}
	var items []staged
	for rows.Next() {
		var item staged
		if err := rows.Scan(&item.id, &item.status, &item.dir); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan staging deployment: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for _, item := range items {
		switch item.status {
		case "ready", "failed", "canceled":
		case "uploading":
			if dryRun {
				break
			}
			// Abandoned upload: 在同一事务内标记 deployment 与 version 为失败，避免出现
			// deployment=failed 但 version 仍 uploading 的中间态。
			if err := s.db.WithTx(ctx, func(tx *store.Tx) error {
				if _, err := tx.ExecContext(ctx, `UPDATE deployments
					SET status = 'failed', error_message = 'upload expired', completed_at = ?
					WHERE id = ? AND status = 'uploading'`, now, item.id); err != nil {
					return err
				}
				_, err := tx.ExecContext(ctx, `UPDATE versions
					SET status = 'failed', error_message = 'upload expired'
					WHERE deployment_id = ? AND status = 'uploading'`, item.id)
				return err
			}); err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("deployment %s: %v", item.id, err))
				s.logger.Error("gc fail abandoned upload failed",
					slog.String("deployment_id", item.id), slog.String("error", err.Error()))
				continue
			}
		default:
			continue
		}
		report.StagingDirs++
		if dryRun {
			continue
		}
		if err := os.RemoveAll(item.dir); err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("staging %s: %v", item.dir, err))
			s.logger.Error("gc clean staging failed",
				slog.String("deployment_id", item.id), slog.String("staging_prefix", item.dir),
				slog.String("error", err.Error()))
			continue
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE deployments SET staging_prefix = '' WHERE id = ?`, item.id); err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("deployment %s: %v", item.id, err))
			s.logger.Error("gc clear staging prefix failed",
				slog.String("deployment_id", item.id), slog.String("error", err.Error()))
			continue
		}
		s.logger.Info("gc cleaned staging",
			slog.String("deployment_id", item.id), slog.String("staging_prefix", item.dir))
	}
	return nil
}
