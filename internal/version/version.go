// Package version stores immutable publish artefacts and the project pointer.
package version

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/pageforge/pageforge/internal/httpx"
	"github.com/pageforge/pageforge/internal/ids"
	"github.com/pageforge/pageforge/internal/store"
)

// Status is the lifecycle state of a version.
type Status string

const (
	StatusUploading Status = "uploading"
	StatusReady     Status = "ready"
	StatusFailed    Status = "failed"
)

// Version is one immutable publish snapshot of a project.
type Version struct {
	ID             string     `json:"id"`
	ProjectID      string     `json:"project_id"`
	DeploymentID   string     `json:"deployment_id,omitempty"`
	Label          string     `json:"label,omitempty"`
	Status         Status     `json:"status"`
	ManifestPrefix string     `json:"manifest_prefix"`
	ManifestSHA256 string     `json:"manifest_sha256,omitempty"`
	FileCount      int64      `json:"file_count"`
	TotalBytes     int64      `json:"total_bytes"`
	ErrorMessage   string     `json:"error_message,omitempty"`
	Protected      bool       `json:"protected"`
	CreatedAt      time.Time  `json:"created_at"`
	ReadyAt        *time.Time `json:"ready_at,omitempty"`
	PublishedAt    *time.Time `json:"published_at,omitempty"`
}

// ErrPointerConflict reports a failed compare-and-swap on the project pointer.
func ErrPointerConflict(current, expected string) error {
	return httpx.Conflict("POINTER_CONFLICT",
		fmt.Sprintf("current version is %q, expected %q", current, expected))
}

// Repo persists versions and pointer changes.
type Repo struct {
	db *store.Store
}

// NewRepo builds a version repository.
func NewRepo(db *store.Store) *Repo {
	return &Repo{db: db}
}

const versionColumns = `id, project_id, deployment_id, label, status, manifest_prefix,
	manifest_sha256, file_count, total_bytes, error_message, protected, created_at,
	ready_at, published_at`

// Create records a new uploading version.
func (r *Repo) Create(ctx context.Context, projectID, deploymentID, label, manifestPrefix string) (Version, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return Version{}, httpx.Invalid("INVALID_VERSION", "project_id is required")
	}
	var exists int
	err := r.db.QueryRowContext(ctx,
		`SELECT 1 FROM projects WHERE id = ? AND deleted_at IS NULL`, projectID).Scan(&exists)
	if err == sql.ErrNoRows {
		return Version{}, httpx.NotFound("PROJECT_NOT_FOUND", "project not found")
	}
	if err != nil {
		return Version{}, fmt.Errorf("check project: %w", err)
	}

	v := Version{
		ID:             ids.New(ids.Version),
		ProjectID:      projectID,
		DeploymentID:   strings.TrimSpace(deploymentID),
		Label:          strings.TrimSpace(label),
		Status:         StatusUploading,
		ManifestPrefix: manifestPrefix,
		CreatedAt:      time.Now().UTC(),
	}
	var labelArg any
	if v.Label != "" {
		labelArg = v.Label
	}
	_, err = r.db.ExecContext(ctx, `INSERT INTO versions (`+versionColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, '', 0, 0, '', FALSE, ?, NULL, NULL)`,
		v.ID, v.ProjectID, v.DeploymentID, labelArg, v.Status, v.ManifestPrefix, v.CreatedAt)
	if err != nil {
		if store.IsUniqueViolation(err) {
			return Version{}, httpx.Conflict("VERSION_LABEL_TAKEN", "label is already used in this project")
		}
		return Version{}, fmt.Errorf("insert version: %w", err)
	}
	return v, nil
}

// Get returns one live version of a project.
func (r *Repo) Get(ctx context.Context, projectID, versionID string) (Version, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+versionColumns+` FROM versions
		 WHERE id = ? AND project_id = ? AND deleted_at IS NULL`, versionID, projectID)
	v, err := scan(row)
	if err == sql.ErrNoRows {
		return Version{}, httpx.NotFound("VERSION_NOT_FOUND", "version not found")
	}
	if err != nil {
		return Version{}, fmt.Errorf("get version: %w", err)
	}
	return v, nil
}

// List returns live versions of a project, newest first.
func (r *Repo) List(ctx context.Context, projectID string) ([]Version, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+versionColumns+` FROM versions
		 WHERE project_id = ? AND deleted_at IS NULL
		 ORDER BY created_at DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list versions: %w", err)
	}
	defer rows.Close()

	out := []Version{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan version: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// MarkReady moves an uploading version to ready.
func (r *Repo) MarkReady(ctx context.Context, projectID, versionID, manifestSHA256 string, fileCount, totalBytes int64) (Version, error) {
	now := time.Now().UTC()
	res, err := r.db.ExecContext(ctx, `UPDATE versions
		SET status = ?, manifest_sha256 = ?, file_count = ?, total_bytes = ?,
		    error_message = '', ready_at = ?, published_at = ?
		WHERE id = ? AND project_id = ? AND status = ? AND deleted_at IS NULL`,
		StatusReady, manifestSHA256, fileCount, totalBytes, now, now,
		versionID, projectID, StatusUploading)
	if err != nil {
		return Version{}, fmt.Errorf("mark version ready: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Version{}, fmt.Errorf("mark version ready rows: %w", err)
	}
	if n == 0 {
		return Version{}, httpx.Conflict("VERSION_NOT_AVAILABLE", "version is not in uploading state")
	}
	return r.Get(ctx, projectID, versionID)
}

// MarkFailed records a failure for an uploading version.
func (r *Repo) MarkFailed(ctx context.Context, projectID, versionID, message string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE versions SET status = ?, error_message = ?
		WHERE id = ? AND project_id = ? AND status = ? AND deleted_at IS NULL`,
		StatusFailed, message, versionID, projectID, StatusUploading)
	if err != nil {
		return fmt.Errorf("mark version failed: %w", err)
	}
	return nil
}

// SetPointer moves the project pointer to a ready version. expectedCurrent, when
// non-empty, performs a compare-and-swap.
func (r *Repo) SetPointer(ctx context.Context, projectID, versionID, expectedCurrent string) (Version, error) {
	var v Version
	err := r.db.WithTx(ctx, func(tx *store.Tx) error {
		var current sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT current_version_id FROM projects WHERE id = ? AND deleted_at IS NULL`,
			projectID).Scan(&current)
		if err == sql.ErrNoRows {
			return httpx.NotFound("PROJECT_NOT_FOUND", "project not found")
		}
		if err != nil {
			return fmt.Errorf("get project pointer: %w", err)
		}
		if expectedCurrent != "" && current.String != expectedCurrent {
			return ErrPointerConflict(current.String, expectedCurrent)
		}

		var ready int
		err = tx.QueryRowContext(ctx,
			`SELECT 1 FROM versions WHERE id = ? AND project_id = ? AND status = ? AND deleted_at IS NULL`,
			versionID, projectID, StatusReady).Scan(&ready)
		if err == sql.ErrNoRows {
			return httpx.Invalid("VERSION_NOT_AVAILABLE", "version is not ready and available")
		}
		if err != nil {
			return fmt.Errorf("check version: %w", err)
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE projects SET current_version_id = ?, updated_at = ? WHERE id = ?`,
			versionID, time.Now().UTC(), projectID); err != nil {
			return fmt.Errorf("update project pointer: %w", err)
		}

		// 版本首次上线（例如以 auto_publish=false 上传后手动发布）时补写发布时间。
		if _, err := tx.ExecContext(ctx,
			`UPDATE versions SET published_at = ? WHERE id = ? AND project_id = ? AND published_at IS NULL`,
			time.Now().UTC(), versionID, projectID); err != nil {
			return fmt.Errorf("stamp version published_at: %w", err)
		}

		row := tx.QueryRowContext(ctx, `SELECT `+versionColumns+` FROM versions WHERE id = ? AND project_id = ?`,
			versionID, projectID)
		v, err = scan(row)
		if err != nil {
			return fmt.Errorf("load version after pointer switch: %w", err)
		}
		return nil
	})
	if err != nil {
		return Version{}, err
	}
	return v, nil
}

// Delete 手动软删除一个版本。当前指针指向的、被 binding 固定的、受保护的或仍在
// 上传中的版本不可删除；删除后该版本不再可见，其对象由 GC 异步清理。
func (r *Repo) Delete(ctx context.Context, projectID, versionID string) error {
	v, err := r.Get(ctx, projectID, versionID)
	if err != nil {
		return err
	}
	if v.Status == StatusUploading {
		return httpx.Conflict("VERSION_NOT_DELETABLE", "uploading version cannot be deleted")
	}
	if v.Protected {
		return httpx.Conflict("VERSION_PROTECTED", "version is protected")
	}
	var current sql.NullString
	if err := r.db.QueryRowContext(ctx,
		`SELECT current_version_id FROM projects WHERE id = ? AND deleted_at IS NULL`, projectID).
		Scan(&current); err != nil {
		return fmt.Errorf("get project pointer: %w", err)
	}
	if current.String == versionID {
		return httpx.Conflict("VERSION_IN_USE", "version is the current pointer")
	}
	var pinned int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM bindings WHERE pinned_version_id = ? AND deleted_at IS NULL`,
		versionID).Scan(&pinned); err != nil {
		return fmt.Errorf("check pinned bindings: %w", err)
	}
	if pinned > 0 {
		return httpx.Conflict("VERSION_IN_USE", "version is pinned by a binding")
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE versions SET deleted_at = ? WHERE id = ? AND project_id = ? AND deleted_at IS NULL`,
		time.Now().UTC(), versionID, projectID)
	if err != nil {
		return fmt.Errorf("delete version: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete version rows: %w", err)
	}
	if n == 0 {
		return httpx.NotFound("VERSION_NOT_FOUND", "version not found")
	}
	return nil
}

// SetProtected toggles the GC protection flag.
func (r *Repo) SetProtected(ctx context.Context, projectID, versionID string, protected bool) (Version, error) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE versions SET protected = ? WHERE id = ? AND project_id = ? AND deleted_at IS NULL`,
		protected, versionID, projectID)
	if err != nil {
		return Version{}, fmt.Errorf("set version protection: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Version{}, fmt.Errorf("set version protection rows: %w", err)
	}
	if n == 0 {
		return Version{}, httpx.NotFound("VERSION_NOT_FOUND", "version not found")
	}
	return r.Get(ctx, projectID, versionID)
}

type scanner interface {
	Scan(dest ...any) error
}

func scan(row scanner) (Version, error) {
	var v Version
	var label sql.NullString
	var readyAt, publishedAt sql.NullTime
	if err := row.Scan(&v.ID, &v.ProjectID, &v.DeploymentID, &label, &v.Status, &v.ManifestPrefix,
		&v.ManifestSHA256, &v.FileCount, &v.TotalBytes, &v.ErrorMessage, &v.Protected,
		&v.CreatedAt, &readyAt, &publishedAt); err != nil {
		return Version{}, err
	}
	v.Label = label.String
	if readyAt.Valid {
		t := readyAt.Time
		v.ReadyAt = &t
	}
	if publishedAt.Valid {
		t := publishedAt.Time
		v.PublishedAt = &t
	}
	return v, nil
}
