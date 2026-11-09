// Package project stores static-site projects and their publish settings.
package project

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/pageforge/pageforge/internal/httpx"
	"github.com/pageforge/pageforge/internal/ids"
	"github.com/pageforge/pageforge/internal/model"
	"github.com/pageforge/pageforge/internal/store"
)

// Project is a publishable static site bound to an alias, bucket and version prefix.
type Project struct {
	ID               string             `json:"id"`
	Name             string             `json:"name"`
	Description      string             `json:"description"`
	AliasID          string             `json:"alias_id"`
	Bucket           string             `json:"bucket"`
	CurrentVersionID string             `json:"current_version_id,omitempty"`
	IndexDoc         string             `json:"index_doc"`
	ErrorDoc         string             `json:"error_doc,omitempty"`
	TryFilesEnabled  bool               `json:"try_files_enabled"`
	SPA              model.SPAConfig    `json:"spa"`
	Cache            model.CachePolicy  `json:"cache"`
	PackageRetention bool               `json:"package_retention"`
	ResponseHeaders  []model.HeaderRule `json:"response_headers"`
	// KeepVersions / KeepDays 为项目级版本保留策略，nil 表示继承全局 gc 配置。
	KeepVersions     *int               `json:"keep_versions"`
	KeepDays         *int               `json:"keep_days"`
	Status           string             `json:"status"`
	CreatedAt        time.Time          `json:"created_at"`
	UpdatedAt        time.Time          `json:"updated_at"`
}

// OptionalInt 区分 JSON 字段「未提供」「显式 null」「具体值」三种状态，
// 用于 PATCH 语义：未提供表示不变，null 表示清除（回退继承全局），值表示设置。
type OptionalInt struct {
	Set   bool
	Null  bool
	Value int
}

// UnmarshalJSON 实现三态解析。
func (o *OptionalInt) UnmarshalJSON(data []byte) error {
	o.Set = true
	if string(data) == "null" {
		o.Null = true
		return nil
	}
	var v int
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	o.Value = v
	return nil
}

// Ptr 返回设置后的值指针；未设置或显式 null 时返回 nil。
func (o OptionalInt) Ptr() *int {
	if !o.Set || o.Null {
		return nil
	}
	v := o.Value
	return &v
}

// CreateInput is the POST /projects payload.
type CreateInput struct {
	Name             string             `json:"name"`
	Description      string             `json:"description"`
	AliasID          string             `json:"alias_id"`
	Bucket           string             `json:"bucket"`
	IndexDoc         string             `json:"index_doc"`
	ErrorDoc         string             `json:"error_doc"`
	TryFilesEnabled  bool               `json:"try_files_enabled"`
	SPA              model.SPAConfig    `json:"spa"`
	Cache            model.CachePolicy  `json:"cache"`
	PackageRetention *bool              `json:"package_retention"`
	ResponseHeaders  []model.HeaderRule `json:"response_headers"`
	KeepVersions     *int               `json:"keep_versions"`
	KeepDays         *int               `json:"keep_days"`
}

// UpdateInput is the PATCH /projects/{id} payload; nil fields are unchanged.
type UpdateInput struct {
	Name             *string             `json:"name"`
	Description      *string             `json:"description"`
	IndexDoc         *string             `json:"index_doc"`
	ErrorDoc         *string             `json:"error_doc"`
	TryFilesEnabled  *bool               `json:"try_files_enabled"`
	SPA              *model.SPAConfig    `json:"spa"`
	Cache            *model.CachePolicy  `json:"cache"`
	PackageRetention *bool               `json:"package_retention"`
	ResponseHeaders  *[]model.HeaderRule `json:"response_headers"`
	KeepVersions     OptionalInt         `json:"keep_versions"`
	KeepDays         OptionalInt         `json:"keep_days"`
	Status           *string             `json:"status"`
}

// Repo persists projects.
type Repo struct {
	db *store.Store
}

// NewRepo builds a project repository.
func NewRepo(db *store.Store) *Repo {
	return &Repo{db: db}
}

const projectColumns = `id, name, description, alias_id, bucket, current_version_id, index_doc,
	error_doc, try_files_enabled, package_retention, spa_config, cache_policy,
	response_headers, keep_versions, keep_days, status, created_at, updated_at`

// validateRetention 校验项目级保留策略：两者均为可选，非 nil 时必须为非负数。
func validateRetention(keepVersions, keepDays *int) error {
	if keepVersions != nil && *keepVersions < 0 {
		return httpx.Invalid("INVALID_PROJECT", "keep_versions must be zero or greater")
	}
	if keepDays != nil && *keepDays < 0 {
		return httpx.Invalid("INVALID_PROJECT", "keep_days must be zero or greater")
	}
	return nil
}

// namePattern 规定 project name：1-128 个字符，只允许字母、数字及 `-`、`_`、`.`，
// 且首字符必须是字母或数字。
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func validateName(name string) error {
	if !namePattern.MatchString(name) {
		return httpx.Invalid("INVALID_PROJECT",
			"name must be 1-128 characters of letters, digits, '-', '_', '.', starting with a letter or digit")
	}
	return nil
}

// Create validates and stores a new project.
func (r *Repo) Create(ctx context.Context, in CreateInput) (Project, error) {
	name := strings.TrimSpace(in.Name)
	if err := validateName(name); err != nil {
		return Project{}, err
	}
	if len(in.Description) > 1024 {
		return Project{}, httpx.Invalid("INVALID_PROJECT", "description must be at most 1024 characters")
	}
	aliasID := strings.TrimSpace(in.AliasID)
	if aliasID == "" {
		return Project{}, httpx.Invalid("INVALID_PROJECT", "alias_id is required")
	}
	bucket := strings.TrimSpace(in.Bucket)
	if err := validateBucket(bucket); err != nil {
		return Project{}, err
	}
	if err := r.ensureAliasAllowsBucket(ctx, aliasID, bucket); err != nil {
		return Project{}, err
	}
	indexDoc := strings.TrimSpace(in.IndexDoc)
	if indexDoc == "" {
		indexDoc = "index.html"
	}
	if err := httpx.ValidateRelKey(indexDoc); err != nil {
		return Project{}, err
	}
	errorDoc := strings.TrimSpace(in.ErrorDoc)
	if errorDoc != "" {
		if err := httpx.ValidateRelKey(errorDoc); err != nil {
			return Project{}, err
		}
	}
	if err := model.ValidateHeaderRules(in.ResponseHeaders); err != nil {
		return Project{}, err
	}
	if err := validateSPA(in.SPA, in.TryFilesEnabled); err != nil {
		return Project{}, err
	}
	if err := validateRetention(in.KeepVersions, in.KeepDays); err != nil {
		return Project{}, err
	}
	retention := true
	if in.PackageRetention != nil {
		retention = *in.PackageRetention
	}
	headers := in.ResponseHeaders
	if headers == nil {
		headers = []model.HeaderRule{}
	}
	now := time.Now().UTC()
	p := Project{
		ID:               ids.New(ids.Project),
		Name:             name,
		Description:      in.Description,
		AliasID:          aliasID,
		Bucket:           bucket,
		IndexDoc:         indexDoc,
		ErrorDoc:         errorDoc,
		TryFilesEnabled:  in.TryFilesEnabled,
		SPA:              in.SPA,
		Cache:            in.Cache.WithDefaults(),
		PackageRetention: retention,
		ResponseHeaders:  headers,
		KeepVersions:     in.KeepVersions,
		KeepDays:         in.KeepDays,
		Status:           model.ProjectActive,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	spaJSON, _ := json.Marshal(p.SPA)
	cacheJSON, _ := json.Marshal(p.Cache)
	headersJSON, _ := json.Marshal(p.ResponseHeaders)
	_, err := r.db.ExecContext(ctx, `INSERT INTO projects (`+projectColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Name, p.Description, p.AliasID, p.Bucket, nil, p.IndexDoc, p.ErrorDoc,
		p.TryFilesEnabled, p.PackageRetention, string(spaJSON), string(cacheJSON),
		string(headersJSON), p.KeepVersions, p.KeepDays, p.Status, p.CreatedAt, p.UpdatedAt)
	if err != nil {
		if store.IsUniqueViolation(err) {
			return Project{}, httpx.Conflict("PROJECT_NAME_TAKEN", "project name is already in use")
		}
		return Project{}, fmt.Errorf("insert project: %w", err)
	}
	return p, nil
}

// GetByName 按全局唯一 name 查找未软删除的 project，用于发布接口按 name 定位目标。
func (r *Repo) GetByName(ctx context.Context, name string) (Project, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+projectColumns+` FROM projects WHERE name = ? AND deleted_at IS NULL`, name)
	p, err := scan(row)
	if err == sql.ErrNoRows {
		return Project{}, httpx.NotFound("PROJECT_NOT_FOUND", "project not found")
	}
	if err != nil {
		return Project{}, fmt.Errorf("get project by name: %w", err)
	}
	return p, nil
}

// Get returns a live project or PROJECT_NOT_FOUND.
func (r *Repo) Get(ctx context.Context, id string) (Project, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+projectColumns+` FROM projects WHERE id = ? AND deleted_at IS NULL`, id)
	p, err := scan(row)
	if err == sql.ErrNoRows {
		return Project{}, httpx.NotFound("PROJECT_NOT_FOUND", "project not found")
	}
	if err != nil {
		return Project{}, fmt.Errorf("get project: %w", err)
	}
	return p, nil
}

// List returns live projects ordered by name.
func (r *Repo) List(ctx context.Context) ([]Project, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+projectColumns+` FROM projects WHERE deleted_at IS NULL ORDER BY name ASC`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	out := []Project{}
	for rows.Next() {
		p, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Update applies a partial update.
func (r *Repo) Update(ctx context.Context, id string, in UpdateInput) (Project, error) {
	p, err := r.Get(ctx, id)
	if err != nil {
		return Project{}, err
	}
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if err := validateName(name); err != nil {
			return Project{}, err
		}
		p.Name = name
	}
	if in.Description != nil {
		if len(*in.Description) > 1024 {
			return Project{}, httpx.Invalid("INVALID_PROJECT", "description must be at most 1024 characters")
		}
		p.Description = *in.Description
	}
	if in.IndexDoc != nil {
		indexDoc := strings.TrimSpace(*in.IndexDoc)
		if indexDoc == "" {
			return Project{}, httpx.Invalid("INVALID_PROJECT", "index_doc must not be empty")
		}
		if err := httpx.ValidateRelKey(indexDoc); err != nil {
			return Project{}, err
		}
		p.IndexDoc = indexDoc
	}
	if in.ErrorDoc != nil {
		errorDoc := strings.TrimSpace(*in.ErrorDoc)
		if errorDoc != "" {
			if err := httpx.ValidateRelKey(errorDoc); err != nil {
				return Project{}, err
			}
		}
		p.ErrorDoc = errorDoc
	}
	if in.TryFilesEnabled != nil {
		p.TryFilesEnabled = *in.TryFilesEnabled
	}
	if in.SPA != nil {
		p.SPA = *in.SPA
	}
	if in.Cache != nil {
		p.Cache = in.Cache.WithDefaults()
	}
	if in.PackageRetention != nil {
		p.PackageRetention = *in.PackageRetention
	}
	if in.ResponseHeaders != nil {
		if err := model.ValidateHeaderRules(*in.ResponseHeaders); err != nil {
			return Project{}, err
		}
		p.ResponseHeaders = *in.ResponseHeaders
	}
	if in.Status != nil {
		status := strings.TrimSpace(*in.Status)
		if status != model.ProjectActive && status != model.ProjectDisabled {
			return Project{}, httpx.Invalid("INVALID_PROJECT", "status must be active or disabled")
		}
		p.Status = status
	}
	if in.KeepVersions.Set {
		p.KeepVersions = in.KeepVersions.Ptr()
	}
	if in.KeepDays.Set {
		p.KeepDays = in.KeepDays.Ptr()
	}
	if err := validateRetention(p.KeepVersions, p.KeepDays); err != nil {
		return Project{}, err
	}
	if err := validateSPA(p.SPA, p.TryFilesEnabled); err != nil {
		return Project{}, err
	}
	p.UpdatedAt = time.Now().UTC()

	spaJSON, _ := json.Marshal(p.SPA)
	cacheJSON, _ := json.Marshal(p.Cache)
	headersJSON, _ := json.Marshal(p.ResponseHeaders)
	_, err = r.db.ExecContext(ctx, `UPDATE projects SET name = ?, description = ?, index_doc = ?,
		error_doc = ?, try_files_enabled = ?, package_retention = ?, spa_config = ?,
		cache_policy = ?, response_headers = ?, keep_versions = ?, keep_days = ?,
		status = ?, updated_at = ?
		WHERE id = ? AND deleted_at IS NULL`,
		p.Name, p.Description, p.IndexDoc, p.ErrorDoc, p.TryFilesEnabled, p.PackageRetention,
		string(spaJSON), string(cacheJSON), string(headersJSON), p.KeepVersions, p.KeepDays,
		p.Status, p.UpdatedAt, p.ID)
	if err != nil {
		if store.IsUniqueViolation(err) {
			return Project{}, httpx.Conflict("PROJECT_NAME_TAKEN", "project name is already in use")
		}
		return Project{}, fmt.Errorf("update project: %w", err)
	}
	return p, nil
}

// Delete soft-deletes a project and its bindings in one transaction.
func (r *Repo) Delete(ctx context.Context, id string) error {
	return r.db.WithTx(ctx, func(tx *store.Tx) error {
		now := time.Now().UTC()
		res, err := tx.ExecContext(ctx,
			`UPDATE projects SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
			now, now, id)
		if err != nil {
			return fmt.Errorf("soft delete project: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("soft delete project rows: %w", err)
		}
		if n == 0 {
			return httpx.NotFound("PROJECT_NOT_FOUND", "project not found")
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE bindings SET deleted_at = ?, updated_at = ? WHERE project_id = ? AND deleted_at IS NULL`,
			now, now, id); err != nil {
			return fmt.Errorf("soft delete bindings: %w", err)
		}
		return nil
	})
}

func (r *Repo) ensureAliasAllowsBucket(ctx context.Context, aliasID, bucket string) error {
	var allowJSON string
	err := r.db.QueryRowContext(ctx,
		`SELECT bucket_allowlist FROM aliases WHERE id = ?`, aliasID).Scan(&allowJSON)
	if err == sql.ErrNoRows {
		return httpx.NotFound("ALIAS_NOT_FOUND", "alias not found")
	}
	if err != nil {
		return fmt.Errorf("get alias allowlist: %w", err)
	}
	var allowlist []string
	if allowJSON != "" {
		if err := json.Unmarshal([]byte(allowJSON), &allowlist); err != nil {
			return fmt.Errorf("decode bucket_allowlist: %w", err)
		}
	}
	if len(allowlist) == 0 {
		return nil
	}
	for _, allowed := range allowlist {
		if allowed == bucket {
			return nil
		}
	}
	return httpx.Invalid("BUCKET_NOT_ALLOWED", "bucket is not in the alias allowlist")
}

// validateSPA 校验 SPA 与 try_files 的依赖关系：SPA fallback 只在 try_files
// 流程内执行，try_files 关闭时 SPA 配置不会生效，因此拒绝保存这种组合。
func validateSPA(spa model.SPAConfig, tryFiles bool) error {
	if spa.Enabled && !tryFiles {
		return httpx.Invalid("INVALID_PROJECT",
			"spa.enabled requires try_files_enabled=true")
	}
	return nil
}

func validateBucket(bucket string) error {
	if bucket == "" || len(bucket) > 255 {
		return httpx.Invalid("INVALID_PROJECT", "bucket must be 1-255 characters")
	}
	if strings.ContainsAny(bucket, "/\\ \t\r\n") {
		return httpx.Invalid("INVALID_PROJECT", "bucket contains an invalid character")
	}
	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scan(row scanner) (Project, error) {
	var p Project
	var currentVersion sql.NullString
	var keepVersions, keepDays sql.NullInt64
	var spaJSON, cacheJSON, headersJSON string
	if err := row.Scan(&p.ID, &p.Name, &p.Description, &p.AliasID, &p.Bucket, &currentVersion,
		&p.IndexDoc, &p.ErrorDoc, &p.TryFilesEnabled, &p.PackageRetention, &spaJSON, &cacheJSON,
		&headersJSON, &keepVersions, &keepDays, &p.Status, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return Project{}, err
	}
	p.CurrentVersionID = currentVersion.String
	if keepVersions.Valid {
		v := int(keepVersions.Int64)
		p.KeepVersions = &v
	}
	if keepDays.Valid {
		v := int(keepDays.Int64)
		p.KeepDays = &v
	}
	if spaJSON != "" {
		if err := json.Unmarshal([]byte(spaJSON), &p.SPA); err != nil {
			return Project{}, fmt.Errorf("decode spa_config: %w", err)
		}
	}
	if cacheJSON != "" {
		if err := json.Unmarshal([]byte(cacheJSON), &p.Cache); err != nil {
			return Project{}, fmt.Errorf("decode cache_policy: %w", err)
		}
	}
	if headersJSON != "" {
		if err := json.Unmarshal([]byte(headersJSON), &p.ResponseHeaders); err != nil {
			return Project{}, fmt.Errorf("decode response_headers: %w", err)
		}
	}
	if p.ResponseHeaders == nil {
		p.ResponseHeaders = []model.HeaderRule{}
	}
	return p, nil
}
