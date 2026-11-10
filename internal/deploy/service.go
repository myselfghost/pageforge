package deploy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pageforge/pageforge/internal/httpx"
	"github.com/pageforge/pageforge/internal/ids"
	"github.com/pageforge/pageforge/internal/objectstore"
	"github.com/pageforge/pageforge/internal/project"
	"github.com/pageforge/pageforge/internal/store"
	"github.com/pageforge/pageforge/internal/version"
)

// Status is the deployment lifecycle state.
type Status string

const (
	StatusCreated    Status = "created"
	StatusUploading  Status = "uploading"
	StatusValidating Status = "validating"
	StatusPreparing  Status = "preparing"
	StatusReady      Status = "ready"
	StatusFailed     Status = "failed"
	StatusCanceled   Status = "canceled"
)

// ModeZip is the classic publish mode. ModeTar and ModeTgz are auto-detected
// from magic bytes when mode is omitted; they are accepted as explicit values
// for backwards compatibility.
const (
	ModeZip = "zip"
	ModeTar = "tar"
	ModeTgz = "tgz"
)

const defaultIndexDoc = "index.html"

// Config controls publishing quotas, staging and manifest sharding.
type Config struct {
	ManifestPrefix  string
	PackagePrefix   string
	TempDir         string
	MaxFiles        int
	MaxFileBytes    int64
	MaxTotalBytes   int64
	MaxPackageBytes int64
	IdempotencyTTL  time.Duration
	Workers         int
	ShardFiles      int
	ShardBytes      int
}

func (c Config) withDefaults() Config {
	if c.ManifestPrefix == "" {
		c.ManifestPrefix = "_system/manifests"
	}
	if c.PackagePrefix == "" {
		c.PackagePrefix = "_system/packages"
	}
	if c.TempDir == "" {
		c.TempDir = os.TempDir()
	}
	if c.MaxFiles <= 0 {
		c.MaxFiles = 200000
	}
	if c.MaxFileBytes <= 0 {
		c.MaxFileBytes = 2 << 30
	}
	if c.MaxTotalBytes <= 0 {
		c.MaxTotalBytes = 50 << 30
	}
	if c.MaxPackageBytes <= 0 {
		c.MaxPackageBytes = 4 << 30
	}
	if c.IdempotencyTTL <= 0 {
		c.IdempotencyTTL = 24 * time.Hour
	}
	if c.Workers <= 0 {
		c.Workers = 32
	}
	if c.ShardFiles <= 0 {
		c.ShardFiles = 20000
	}
	if c.ShardBytes <= 0 {
		c.ShardBytes = 2 << 20
	}
	return c
}

// StoreFactory returns the object store for a project's alias and bucket.
type StoreFactory func(ctx context.Context, p project.Project) (objectstore.ObjectStore, error)

// Options wires the publish service.
type Options struct {
	Config    Config
	Stores    StoreFactory
	Logger    *slog.Logger
	OnPublish func(ctx context.Context, projectID, versionID string)
}

// Service owns the publish state machine.
type Service struct {
	db        *store.Store
	projects  *project.Repo
	versions  *version.Repo
	stores    StoreFactory
	cfg       Config
	logger    *slog.Logger
	onPublish func(ctx context.Context, projectID, versionID string)
}

// NewService builds a publish service.
func NewService(db *store.Store, opts Options) *Service {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		db:        db,
		projects:  project.NewRepo(db),
		versions:  version.NewRepo(db),
		stores:    opts.Stores,
		cfg:       opts.Config.withDefaults(),
		logger:    logger,
		onPublish: opts.OnPublish,
	}
}

// Deployment is one publish attempt.
type Deployment struct {
	ID             string
	ProjectID      string
	VersionID      string
	Mode           string
	Status         Status
	IdempotencyKey string
	PackageKey     string
	StagingPrefix  string
	// AutoPublish 为 false 时，包发布完成后版本只进入 ready，
	// 不切换 project 的当前版本指针。
	AutoPublish  bool
	ErrorMessage string
	CreatedAt    time.Time
	CompletedAt  *time.Time
}

// ListView is one row of the deployment list, joining the project name and the
// latest version summary so the console can render the history without N+1
// requests.
type ListView struct {
	DeploymentID string     `json:"deployment_id"`
	VersionID    string     `json:"version_id,omitempty"`
	ProjectID    string     `json:"project_id"`
	ProjectName  string     `json:"project_name"`
	Mode         string     `json:"mode"`
	Status       Status     `json:"status"`
	FileCount    int64      `json:"file_count"`
	TotalBytes   int64      `json:"total_bytes"`
	ErrorMessage string     `json:"error_message,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
}

// View is the API representation of a deployment plus its version summary.
type View struct {
	DeploymentID   string     `json:"deployment_id"`
	VersionID      string     `json:"version_id,omitempty"`
	ProjectID      string     `json:"project_id"`
	Mode           string     `json:"mode"`
	Status         Status     `json:"status"`
	UploadURL      string     `json:"upload_url,omitempty"`
	AutoPublish    bool       `json:"auto_publish"`
	FileCount      int64      `json:"file_count,omitempty"`
	TotalBytes     int64      `json:"total_bytes,omitempty"`
	ManifestSHA256 string     `json:"manifest_sha256,omitempty"`
	ErrorMessage   string     `json:"error_message,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
}

// CreateInput is the POST /deployments payload.
// 目标 project 用 project_id 或 project_name 二选一指定（project_name 全局唯一）。
type CreateInput struct {
	ProjectID      string `json:"project_id"`
	ProjectName    string `json:"project_name"`
	Mode           string `json:"mode"`
	Label          string `json:"label"`
	IdempotencyKey string `json:"idempotency_key"`
	// AutoPublish 为 nil 时按 true 处理；显式传 false 表示发布成功后
	// 不自动把新版本切换为当前版本，后续需调用指针接口发布。
	AutoPublish *bool `json:"auto_publish,omitempty"`
}

const deploymentColumns = `id, project_id, mode, status, idempotency_key, package_key,
	staging_prefix, auto_publish, error_message, created_at, completed_at`

// Create records a deployment plus its uploading version in one transaction.
// It reports whether an idempotent replay returned the original deployment.
func (s *Service) Create(ctx context.Context, in CreateInput) (View, bool, error) {
	projectID := strings.TrimSpace(in.ProjectID)
	projectName := strings.TrimSpace(in.ProjectName)
	if projectID != "" && projectName != "" {
		return View{}, false, httpx.Invalid("INVALID_DEPLOYMENT", "project_id and project_name are mutually exclusive")
	}
	if projectName != "" {
		p, err := s.projects.GetByName(ctx, projectName)
		if err != nil {
			return View{}, false, err
		}
		projectID = p.ID
	}
	if projectID == "" {
		return View{}, false, httpx.Invalid("INVALID_DEPLOYMENT", "project_id or project_name is required")
	}
	mode := strings.TrimSpace(in.Mode)
	switch mode {
	case "", "auto":
		// Auto-detect: format is determined from magic bytes at publish time.
		mode = "auto"
	case ModeZip, ModeTar, ModeTgz:
		// Explicit mode: validated at publish time against the actual magic
		// bytes; a mismatch returns INVALID_PACKAGE.
	default:
		return View{}, false, httpx.Invalid("INVALID_DEPLOYMENT",
			"mode must be one of zip, tar, tgz, auto or omitted")
	}
	label := strings.TrimSpace(in.Label)
	if len(label) > 128 {
		return View{}, false, httpx.Invalid("INVALID_DEPLOYMENT", "label must be at most 128 characters")
	}
	if _, err := s.projects.Get(ctx, projectID); err != nil {
		return View{}, false, err
	}
	autoPublish := true
	if in.AutoPublish != nil {
		autoPublish = *in.AutoPublish
	}

	key := strings.TrimSpace(in.IdempotencyKey)
	if key != "" {
		existing, err := s.findByIdempotency(ctx, projectID, key)
		if err != nil {
			return View{}, false, err
		}
		if existing != nil {
			view, err := s.viewOf(ctx, *existing)
			return view, true, err
		}
	}

	now := time.Now().UTC()
	dep := Deployment{
		ID:             ids.New(ids.Deployment),
		ProjectID:      projectID,
		Mode:           mode,
		Status:         StatusCreated,
		IdempotencyKey: key,
		AutoPublish:    autoPublish,
		CreatedAt:      now,
	}
	versionID := ids.New(ids.Version)
	ver := version.Version{
		ID:             versionID,
		ProjectID:      projectID,
		DeploymentID:   dep.ID,
		Label:          label,
		Status:         version.StatusUploading,
		ManifestPrefix: s.manifestPrefix(projectID, versionID),
		CreatedAt:      now,
	}

	var keyArg any
	if key != "" {
		keyArg = key
	}
	var labelArg any
	if label != "" {
		labelArg = label
	}
	err := s.db.WithTx(ctx, func(tx *store.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO deployments (`+deploymentColumns+`)
			VALUES (?, ?, ?, ?, ?, '', '', ?, '', ?, NULL)`,
			dep.ID, dep.ProjectID, dep.Mode, dep.Status, keyArg, dep.AutoPublish, dep.CreatedAt)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO versions
			(id, project_id, deployment_id, label, status, manifest_prefix, manifest_sha256,
			 file_count, total_bytes, error_message, protected, created_at, ready_at, published_at)
			VALUES (?, ?, ?, ?, ?, ?, '', 0, 0, '', FALSE, ?, NULL, NULL)`,
			ver.ID, ver.ProjectID, ver.DeploymentID, labelArg, ver.Status,
			ver.ManifestPrefix, ver.CreatedAt)
		return err
	})
	if err != nil {
		if store.IsUniqueViolation(err) {
			if key != "" {
				if existing, findErr := s.findByIdempotency(ctx, projectID, key); findErr == nil && existing != nil {
					view, viewErr := s.viewOf(ctx, *existing)
					return view, true, viewErr
				}
			}
			return View{}, false, httpx.Conflict("VERSION_LABEL_TAKEN", "label is already used in this project")
		}
		return View{}, false, fmt.Errorf("create deployment: %w", err)
	}

	stored, err := s.get(ctx, dep.ID)
	if err != nil {
		return View{}, false, err
	}
	view, err := s.viewOf(ctx, stored)
	return view, false, err
}

// PutPackage streams the uploaded zip into a local staging directory. The
// deployment moves to uploading.
func (s *Service) PutPackage(ctx context.Context, deploymentID string, body io.Reader, declaredSize int64) (View, error) {
	dep, err := s.get(ctx, deploymentID)
	if err != nil {
		return View{}, err
	}
	if dep.Status != StatusCreated && dep.Status != StatusUploading {
		return View{}, httpx.Conflict("DEPLOYMENT_NOT_UPLOADABLE",
			fmt.Sprintf("deployment is %s", dep.Status))
	}
	if declaredSize > s.cfg.MaxPackageBytes {
		return View{}, limitExceeded("package exceeds the upload limit of %d bytes", s.cfg.MaxPackageBytes)
	}

	dir := s.stagingDir(dep.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return View{}, fmt.Errorf("create staging directory: %w", err)
	}
	path := filepath.Join(dir, packageFileBase+".zip")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return View{}, fmt.Errorf("open staging file: %w", err)
	}
	written, copyErr := io.Copy(file, io.LimitReader(body, s.cfg.MaxPackageBytes+1))
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(path)
		return View{}, fmt.Errorf("write package: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return View{}, fmt.Errorf("write package: %w", closeErr)
	}
	if written > s.cfg.MaxPackageBytes {
		_ = os.Remove(path)
		return View{}, limitExceeded("package exceeds the upload limit of %d bytes", s.cfg.MaxPackageBytes)
	}

	_, err = s.db.ExecContext(ctx, `UPDATE deployments
		SET status = ?, staging_prefix = ?, error_message = ''
		WHERE id = ? AND status IN (?, ?)`,
		StatusUploading, dir, dep.ID, StatusCreated, StatusUploading)
	if err != nil {
		return View{}, fmt.Errorf("update deployment status: %w", err)
	}
	dep.Status = StatusUploading
	dep.StagingPrefix = dir
	return s.viewOf(ctx, dep)
}

// Get returns one deployment.
func (s *Service) Get(ctx context.Context, deploymentID string) (View, error) {
	dep, err := s.get(ctx, deploymentID)
	if err != nil {
		return View{}, err
	}
	return s.viewOf(ctx, dep)
}

// Complete validates the uploaded package, publishes the objects and switches
// the project pointer (unless the deployment was created with auto_publish=false,
// in which case the version only becomes ready). It is idempotent once the
// deployment is ready.
func (s *Service) Complete(ctx context.Context, deploymentID string) (View, error) {
	dep, err := s.get(ctx, deploymentID)
	if err != nil {
		return View{}, err
	}
	switch dep.Status {
	case StatusReady:
		return s.viewOf(ctx, dep)
	case StatusCreated:
		return View{}, httpx.Conflict("PACKAGE_NOT_UPLOADED", "package has not been uploaded")
	case StatusValidating, StatusPreparing:
		return View{}, httpx.Conflict("DEPLOYMENT_IN_PROGRESS", "deployment is already being published")
	case StatusFailed, StatusCanceled:
		return View{}, httpx.Conflict("DEPLOYMENT_NOT_COMPLETABLE",
			fmt.Sprintf("deployment is %s", dep.Status))
	}

	moved, err := s.transition(ctx, dep.ID, StatusUploading, StatusValidating)
	if err != nil {
		return View{}, err
	}
	if !moved {
		return View{}, httpx.Conflict("DEPLOYMENT_IN_PROGRESS", "deployment state changed concurrently")
	}
	dep.Status = StatusValidating

	p, err := s.projects.Get(ctx, dep.ProjectID)
	if err != nil {
		s.fail(ctx, dep, err)
		return View{}, err
	}
	ver, err := s.versions.Get(ctx, p.ID, dep.VersionID)
	if err != nil {
		s.fail(ctx, dep, err)
		return View{}, err
	}
	if err := s.publish(ctx, dep, p, ver); err != nil {
		s.fail(ctx, dep, err)
		return View{}, err
	}
	if dep.AutoPublish && s.onPublish != nil {
		s.onPublish(ctx, p.ID, ver.ID)
	}

	updated, err := s.get(ctx, dep.ID)
	if err != nil {
		return View{}, err
	}
	return s.viewOf(ctx, updated)
}

// Cancel stops a deployment before object upload starts.
func (s *Service) Cancel(ctx context.Context, deploymentID string) (View, error) {
	dep, err := s.get(ctx, deploymentID)
	if err != nil {
		return View{}, err
	}
	switch dep.Status {
	case StatusCreated, StatusUploading, StatusValidating:
	case StatusCanceled:
		return s.viewOf(ctx, dep)
	default:
		return View{}, httpx.Conflict("DEPLOYMENT_NOT_CANCELABLE",
			fmt.Sprintf("deployment is %s", dep.Status))
	}

	err = s.db.WithTx(ctx, func(tx *store.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE deployments SET status = ?, completed_at = ?
			WHERE id = ? AND status IN (?, ?, ?)`,
			StatusCanceled, time.Now().UTC(), dep.ID, StatusCreated, StatusUploading, StatusValidating)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return httpx.Conflict("DEPLOYMENT_NOT_CANCELABLE", "deployment can no longer be canceled")
		}
		_, err = tx.ExecContext(ctx, `UPDATE versions SET status = ?, error_message = ?
			WHERE deployment_id = ? AND status = ?`,
			version.StatusFailed, "deployment canceled", dep.ID, version.StatusUploading)
		return err
	})
	if err != nil {
		return View{}, err
	}
	_ = os.RemoveAll(s.stagingDir(dep.ID))
	updated, err := s.get(ctx, dep.ID)
	if err != nil {
		return View{}, err
	}
	return s.viewOf(ctx, updated)
}

func (s *Service) get(ctx context.Context, deploymentID string) (Deployment, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+deploymentColumns+` FROM deployments WHERE id = ?`, deploymentID)
	var dep Deployment
	var key, packageKey, staging sql.NullString
	var completed sql.NullTime
	if err := row.Scan(&dep.ID, &dep.ProjectID, &dep.Mode, &dep.Status, &key, &packageKey,
		&staging, &dep.AutoPublish, &dep.ErrorMessage, &dep.CreatedAt, &completed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Deployment{}, httpx.NotFound("DEPLOYMENT_NOT_FOUND", "deployment not found")
		}
		return Deployment{}, fmt.Errorf("get deployment: %w", err)
	}
	dep.IdempotencyKey = key.String
	dep.PackageKey = packageKey.String
	dep.StagingPrefix = staging.String
	if completed.Valid {
		t := completed.Time
		dep.CompletedAt = &t
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM versions WHERE deployment_id = ? AND deleted_at IS NULL
		 ORDER BY created_at DESC LIMIT 1`, deploymentID).Scan(&dep.VersionID); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, fmt.Errorf("get deployment version: %w", err)
	}
	return dep, nil
}

func (s *Service) viewOf(ctx context.Context, dep Deployment) (View, error) {
	view := View{
		DeploymentID: dep.ID,
		VersionID:    dep.VersionID,
		ProjectID:    dep.ProjectID,
		Mode:         dep.Mode,
		Status:       dep.Status,
		AutoPublish:  dep.AutoPublish,
		ErrorMessage: dep.ErrorMessage,
		CreatedAt:    dep.CreatedAt,
		CompletedAt:  dep.CompletedAt,
	}
	if dep.Status == StatusCreated || dep.Status == StatusUploading {
		view.UploadURL = "/api/v1/deployments/" + dep.ID + "/package"
	}
	if dep.VersionID != "" {
		ver, err := s.versions.Get(ctx, dep.ProjectID, dep.VersionID)
		if err != nil {
			return View{}, err
		}
		view.FileCount = ver.FileCount
		view.TotalBytes = ver.TotalBytes
		view.ManifestSHA256 = ver.ManifestSHA256
	}
	return view, nil
}

// List returns deployment history newest first. When projectID is empty it
// returns deployments across all live projects; otherwise it is scoped to that
// project. limit caps the result (defaults to 100, max 500).
func (s *Service) List(ctx context.Context, projectID string, limit int) ([]ListView, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	query := `SELECT d.id, d.project_id, p.name, d.mode, d.status, d.error_message,
			d.created_at, d.completed_at,
			v.id, v.file_count, v.total_bytes
		FROM deployments d
		JOIN projects p ON p.id = d.project_id
		LEFT JOIN versions v ON v.id = (
			SELECT id FROM versions
			WHERE deployment_id = d.id AND deleted_at IS NULL
			ORDER BY created_at DESC LIMIT 1
		)
		WHERE p.deleted_at IS NULL`
	args := []any{}
	if projectID != "" {
		query += ` AND d.project_id = ?`
		args = append(args, projectID)
	}
	query += ` ORDER BY d.created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	defer rows.Close()

	out := []ListView{}
	for rows.Next() {
		var item ListView
		var versionID sql.NullString
		var fileCount, totalBytes sql.NullInt64
		var completed sql.NullTime
		if err := rows.Scan(&item.DeploymentID, &item.ProjectID, &item.ProjectName,
			&item.Mode, &item.Status, &item.ErrorMessage, &item.CreatedAt, &completed,
			&versionID, &fileCount, &totalBytes); err != nil {
			return nil, fmt.Errorf("scan deployment: %w", err)
		}
		item.VersionID = versionID.String
		item.FileCount = fileCount.Int64
		item.TotalBytes = totalBytes.Int64
		if completed.Valid {
			t := completed.Time
			item.CompletedAt = &t
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Service) findByIdempotency(ctx context.Context, projectID, key string) (*Deployment, error) {
	dep, err := s.getByIdempotency(ctx, projectID, key)
	if err != nil || dep == nil {
		return dep, err
	}
	if time.Since(dep.CreatedAt) < s.cfg.IdempotencyTTL {
		return dep, nil
	}
	// Expired keys must not block a fresh deployment under the same key.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE deployments SET idempotency_key = NULL WHERE id = ?`, dep.ID); err != nil {
		return nil, fmt.Errorf("expire idempotency key: %w", err)
	}
	return nil, nil
}

func (s *Service) getByIdempotency(ctx context.Context, projectID, key string) (*Deployment, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+deploymentColumns+` FROM deployments
		 WHERE project_id = ? AND idempotency_key = ?`, projectID, key)
	var dep Deployment
	var depKey, packageKey, staging sql.NullString
	var completed sql.NullTime
	err := row.Scan(&dep.ID, &dep.ProjectID, &dep.Mode, &dep.Status, &depKey, &packageKey,
		&staging, &dep.AutoPublish, &dep.ErrorMessage, &dep.CreatedAt, &completed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get deployment by idempotency key: %w", err)
	}
	dep.IdempotencyKey = depKey.String
	dep.PackageKey = packageKey.String
	dep.StagingPrefix = staging.String
	if completed.Valid {
		t := completed.Time
		dep.CompletedAt = &t
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM versions WHERE deployment_id = ? AND deleted_at IS NULL
		 ORDER BY created_at DESC LIMIT 1`, dep.ID).Scan(&dep.VersionID); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("get deployment version: %w", err)
	}
	return &dep, nil
}

func (s *Service) transition(ctx context.Context, id string, from, to Status) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE deployments SET status = ? WHERE id = ? AND status = ?`, to, id, from)
	if err != nil {
		return false, fmt.Errorf("update deployment status: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("update deployment status rows: %w", err)
	}
	return n > 0, nil
}

func (s *Service) fail(ctx context.Context, dep Deployment, cause error) {
	message := cause.Error()
	if len(message) > 1024 {
		message = message[:1024]
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(cleanupCtx, `UPDATE deployments
		SET status = ?, error_message = ?, completed_at = ?
		WHERE id = ? AND status IN (?, ?)`,
		StatusFailed, message, now, dep.ID, StatusValidating, StatusPreparing); err != nil {
		s.logger.Error("mark deployment failed", slog.String("deployment_id", dep.ID), slog.String("error", err.Error()))
	}
	if _, err := s.db.ExecContext(cleanupCtx, `UPDATE versions
		SET status = ?, error_message = ?
		WHERE deployment_id = ? AND status = ?`,
		version.StatusFailed, message, dep.ID, version.StatusUploading); err != nil {
		s.logger.Error("mark version failed", slog.String("deployment_id", dep.ID), slog.String("error", err.Error()))
	}
	_ = os.RemoveAll(s.stagingDir(dep.ID))
}

func (s *Service) stagingDir(deploymentID string) string {
	return filepath.Join(s.cfg.TempDir, "pageforge-staging", deploymentID)
}

func (s *Service) manifestPrefix(projectID, versionID string) string {
	return strings.Trim(s.cfg.ManifestPrefix, "/") + "/" + projectID + "/" + versionID
}
