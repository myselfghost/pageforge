// Package binding maps hosts and path prefixes to projects.
package binding

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/pageforge/pageforge/internal/httpx"
	"github.com/pageforge/pageforge/internal/ids"
	"github.com/pageforge/pageforge/internal/model"
	"github.com/pageforge/pageforge/internal/store"
)

// Binding is a URL route: host + path prefix -> project (+ optional pinned version).
type Binding struct {
	ID              string             `json:"id"`
	Host            string             `json:"host"`
	PathPrefix      string             `json:"path_prefix"`
	ProjectID       string             `json:"project_id"`
	PinnedVersionID string             `json:"pinned_version_id,omitempty"`
	AuthPolicyID    string             `json:"auth_policy_id,omitempty"`
	ResponseHeaders []model.HeaderRule `json:"response_headers"`
	Enabled         bool               `json:"enabled"`
	CreatedAt       time.Time          `json:"created_at"`
	UpdatedAt       time.Time          `json:"updated_at"`
}

// CreateInput is the POST /bindings payload.
type CreateInput struct {
	Host            string             `json:"host"`
	PathPrefix      string             `json:"path_prefix"`
	ProjectID       string             `json:"project_id"`
	PinnedVersionID string             `json:"pinned_version_id"`
	AuthPolicyID    string             `json:"auth_policy_id"`
	ResponseHeaders []model.HeaderRule `json:"response_headers"`
	Enabled         *bool              `json:"enabled"`
}

// UpdateInput is the PATCH /bindings/{id} payload; nil fields are unchanged.
type UpdateInput struct {
	Host            *string             `json:"host"`
	PathPrefix      *string             `json:"path_prefix"`
	ProjectID       *string             `json:"project_id"`
	PinnedVersionID *string             `json:"pinned_version_id"`
	AuthPolicyID    *string             `json:"auth_policy_id"`
	ResponseHeaders *[]model.HeaderRule `json:"response_headers"`
	Enabled         *bool               `json:"enabled"`
}

// Repo persists bindings.
type Repo struct {
	db             *store.Store
	reservedPrefix string
}

// NewRepo builds a binding repository; reservedPrefix is rejected in path prefixes.
func NewRepo(db *store.Store, reservedPrefix string) *Repo {
	return &Repo{db: db, reservedPrefix: reservedPrefix}
}

const bindingColumns = `id, host, path_prefix, project_id, pinned_version_id, auth_policy_id,
	response_headers, enabled, created_at, updated_at`

// Create validates and stores a new binding.
func (r *Repo) Create(ctx context.Context, in CreateInput) (Binding, error) {
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	b := Binding{
		ID:              ids.New(ids.Binding),
		Host:            in.Host,
		PathPrefix:      in.PathPrefix,
		ProjectID:       in.ProjectID,
		PinnedVersionID: in.PinnedVersionID,
		AuthPolicyID:    in.AuthPolicyID,
		ResponseHeaders: in.ResponseHeaders,
		Enabled:         enabled,
	}
	if b.ResponseHeaders == nil {
		b.ResponseHeaders = []model.HeaderRule{}
	}
	if err := r.normalize(ctx, &b); err != nil {
		return Binding{}, err
	}
	now := time.Now().UTC()
	b.CreatedAt, b.UpdatedAt = now, now

	headersJSON, _ := json.Marshal(b.ResponseHeaders)
	_, err := r.db.ExecContext(ctx, `INSERT INTO bindings (`+bindingColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.ID, b.Host, b.PathPrefix, b.ProjectID, nullString(b.PinnedVersionID), b.AuthPolicyID,
		string(headersJSON), b.Enabled, b.CreatedAt, b.UpdatedAt)
	if err != nil {
		if store.IsUniqueViolation(err) {
			return Binding{}, httpx.Conflict("BINDING_CONFLICT", "host and path prefix are already bound")
		}
		return Binding{}, fmt.Errorf("insert binding: %w", err)
	}
	return b, nil
}

// Get returns a live binding or BINDING_NOT_FOUND.
func (r *Repo) Get(ctx context.Context, id string) (Binding, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+bindingColumns+` FROM bindings WHERE id = ? AND deleted_at IS NULL`, id)
	b, err := scan(row)
	if err == sql.ErrNoRows {
		return Binding{}, httpx.NotFound("BINDING_NOT_FOUND", "binding not found")
	}
	if err != nil {
		return Binding{}, fmt.Errorf("get binding: %w", err)
	}
	return b, nil
}

// List returns live bindings, optionally filtered by project.
func (r *Repo) List(ctx context.Context, projectID string) ([]Binding, error) {
	query := `SELECT ` + bindingColumns + ` FROM bindings WHERE deleted_at IS NULL`
	args := []any{}
	if projectID != "" {
		query += ` AND project_id = ?`
		args = append(args, projectID)
	}
	query += ` ORDER BY host ASC, path_prefix ASC`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list bindings: %w", err)
	}
	defer rows.Close()

	out := []Binding{}
	for rows.Next() {
		b, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan binding: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Update applies a partial update.
func (r *Repo) Update(ctx context.Context, id string, in UpdateInput) (Binding, error) {
	b, err := r.Get(ctx, id)
	if err != nil {
		return Binding{}, err
	}
	if in.Host != nil {
		b.Host = *in.Host
	}
	if in.PathPrefix != nil {
		b.PathPrefix = *in.PathPrefix
	}
	if in.ProjectID != nil {
		b.ProjectID = *in.ProjectID
	}
	if in.PinnedVersionID != nil {
		b.PinnedVersionID = *in.PinnedVersionID
	}
	if in.AuthPolicyID != nil {
		b.AuthPolicyID = *in.AuthPolicyID
	}
	if in.ResponseHeaders != nil {
		b.ResponseHeaders = *in.ResponseHeaders
		if b.ResponseHeaders == nil {
			b.ResponseHeaders = []model.HeaderRule{}
		}
	}
	if in.Enabled != nil {
		b.Enabled = *in.Enabled
	}
	if err := r.normalize(ctx, &b); err != nil {
		return Binding{}, err
	}
	b.UpdatedAt = time.Now().UTC()

	headersJSON, _ := json.Marshal(b.ResponseHeaders)
	_, err = r.db.ExecContext(ctx, `UPDATE bindings SET host = ?, path_prefix = ?, project_id = ?,
		pinned_version_id = ?, auth_policy_id = ?, response_headers = ?, enabled = ?, updated_at = ?
		WHERE id = ? AND deleted_at IS NULL`,
		b.Host, b.PathPrefix, b.ProjectID, nullString(b.PinnedVersionID), b.AuthPolicyID,
		string(headersJSON), b.Enabled, b.UpdatedAt, b.ID)
	if err != nil {
		if store.IsUniqueViolation(err) {
			return Binding{}, httpx.Conflict("BINDING_CONFLICT", "host and path prefix are already bound")
		}
		return Binding{}, fmt.Errorf("update binding: %w", err)
	}
	return b, nil
}

// Delete soft-deletes a binding.
func (r *Repo) Delete(ctx context.Context, id string) error {
	now := time.Now().UTC()
	res, err := r.db.ExecContext(ctx,
		`UPDATE bindings SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		now, now, id)
	if err != nil {
		return fmt.Errorf("soft delete binding: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("soft delete binding rows: %w", err)
	}
	if n == 0 {
		return httpx.NotFound("BINDING_NOT_FOUND", "binding not found")
	}
	return nil
}

// Validate normalizes and validates a binding without persisting it.
func (r *Repo) Validate(ctx context.Context, in CreateInput) (Binding, error) {
	b := Binding{
		Host:            in.Host,
		PathPrefix:      in.PathPrefix,
		ProjectID:       in.ProjectID,
		PinnedVersionID: in.PinnedVersionID,
		AuthPolicyID:    in.AuthPolicyID,
		ResponseHeaders: in.ResponseHeaders,
		Enabled:         in.Enabled == nil || *in.Enabled,
	}
	if b.ResponseHeaders == nil {
		b.ResponseHeaders = []model.HeaderRule{}
	}
	if err := r.normalize(ctx, &b); err != nil {
		return Binding{}, err
	}
	return b, nil
}

// normalize applies defaults and cross-entity checks in place.
func (r *Repo) normalize(ctx context.Context, b *Binding) error {
	host, err := httpx.NormalizeHost(b.Host)
	if err != nil {
		return err
	}
	prefix, err := httpx.NormalizePathPrefix(b.PathPrefix)
	if err != nil {
		return err
	}
	if httpx.ConflictsReserved(prefix, r.reservedPrefix) {
		return httpx.Conflict("RESERVED_PREFIX_CONFLICT", "path prefix conflicts with the reserved prefix")
	}
	b.Host = host
	b.PathPrefix = prefix

	b.ProjectID = strings.TrimSpace(b.ProjectID)
	if b.ProjectID == "" {
		return httpx.Invalid("INVALID_BINDING", "project_id is required")
	}
	var exists int
	err = r.db.QueryRowContext(ctx,
		`SELECT 1 FROM projects WHERE id = ? AND deleted_at IS NULL`, b.ProjectID).Scan(&exists)
	if err == sql.ErrNoRows {
		return httpx.NotFound("PROJECT_NOT_FOUND", "project not found")
	}
	if err != nil {
		return fmt.Errorf("check project: %w", err)
	}

	b.PinnedVersionID = strings.TrimSpace(b.PinnedVersionID)
	if b.PinnedVersionID != "" {
		err = r.db.QueryRowContext(ctx,
			`SELECT 1 FROM versions WHERE id = ? AND project_id = ? AND status = 'ready' AND deleted_at IS NULL`,
			b.PinnedVersionID, b.ProjectID).Scan(&exists)
		if err == sql.ErrNoRows {
			return httpx.Invalid("VERSION_NOT_AVAILABLE", "pinned version is not available for this project")
		}
		if err != nil {
			return fmt.Errorf("check pinned version: %w", err)
		}
	}

	b.AuthPolicyID = strings.TrimSpace(b.AuthPolicyID)
	if b.AuthPolicyID != "" {
		err = r.db.QueryRowContext(ctx,
			`SELECT 1 FROM auth_policies WHERE id = ?`, b.AuthPolicyID).Scan(&exists)
		if err == sql.ErrNoRows {
			return httpx.NotFound("AUTH_POLICY_NOT_FOUND", "auth policy not found")
		}
		if err != nil {
			return fmt.Errorf("check auth policy: %w", err)
		}
	}

	if err := model.ValidateHeaderRules(b.ResponseHeaders); err != nil {
		return err
	}

	var conflictID string
	err = r.db.QueryRowContext(ctx,
		`SELECT id FROM bindings WHERE host = ? AND path_prefix = ? AND deleted_at IS NULL AND id <> ?`,
		b.Host, b.PathPrefix, b.ID).Scan(&conflictID)
	if err == nil {
		return httpx.Conflict("BINDING_CONFLICT", "host and path prefix are already bound")
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("check binding conflict: %w", err)
	}
	return nil
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

type scanner interface {
	Scan(dest ...any) error
}

func scan(row scanner) (Binding, error) {
	var b Binding
	var pinned sql.NullString
	var headersJSON string
	if err := row.Scan(&b.ID, &b.Host, &b.PathPrefix, &b.ProjectID, &pinned, &b.AuthPolicyID,
		&headersJSON, &b.Enabled, &b.CreatedAt, &b.UpdatedAt); err != nil {
		return Binding{}, err
	}
	b.PinnedVersionID = pinned.String
	if headersJSON != "" {
		if err := json.Unmarshal([]byte(headersJSON), &b.ResponseHeaders); err != nil {
			return Binding{}, fmt.Errorf("decode response_headers: %w", err)
		}
	}
	if b.ResponseHeaders == nil {
		b.ResponseHeaders = []model.HeaderRule{}
	}
	return b, nil
}
