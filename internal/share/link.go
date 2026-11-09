// Package share stores expiring, optionally password-protected share links
// that expose a project (or one version of it) independently of URL bindings.
package share

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/pageforge/pageforge/internal/httpx"
	"github.com/pageforge/pageforge/internal/ids"
	"github.com/pageforge/pageforge/internal/model"
	"github.com/pageforge/pageforge/internal/store"
)

// bcryptCost keeps share passwords as expensive to brute-force as admin users.
const bcryptCost = 12

// Status is the effective state of a share link at a point in time.
type Status string

const (
	StatusActive   Status = "active"
	StatusDisabled Status = "disabled"
	StatusExpired  Status = "expired"
)

// Link is a shareable view of a project.
type Link struct {
	ID          string            `json:"id"`
	Token       string            `json:"token"`
	ProjectID   string            `json:"project_id"`
	VersionID   string            `json:"version_id,omitempty"`
	PasswordSet bool              `json:"password_set"`
	ExpiresAt   *time.Time        `json:"expires_at,omitempty"`
	MaxViews    int64             `json:"max_views"`
	Views       int64             `json:"views"`
	Cache       model.CachePolicy `json:"cache"`
	Enabled     bool              `json:"enabled"`
	Status      Status            `json:"status"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`

	passwordHash string
}

// CreateInput is the POST /share-links payload.
type CreateInput struct {
	ProjectID string             `json:"project_id"`
	VersionID string             `json:"version_id"`
	ExpiresAt *time.Time         `json:"expires_at"`
	Password  string             `json:"password"`
	MaxViews  int64              `json:"max_views"`
	Cache     *model.CachePolicy `json:"cache"`
}

// UpdateInput is the PATCH /share-links/{id} payload; nil fields are unchanged.
type UpdateInput struct {
	VersionID   *string            `json:"version_id"`
	ExpiresAt   *time.Time         `json:"expires_at"`
	ClearExpiry bool               `json:"clear_expiry"`
	Password    *string            `json:"password"`
	MaxViews    *int64             `json:"max_views"`
	Cache       *model.CachePolicy `json:"cache"`
	Enabled     *bool              `json:"enabled"`
}

// Repo persists share links.
type Repo struct {
	db *store.Store
}

// NewRepo builds a share-link repository.
func NewRepo(db *store.Store) *Repo {
	return &Repo{db: db}
}

const linkColumns = `id, token, project_id, version_id, password_hash, expires_at,
	max_views, views, cache_policy, enabled, created_at, updated_at`

// Create validates and stores a new share link.
func (r *Repo) Create(ctx context.Context, in CreateInput) (Link, error) {
	projectID := strings.TrimSpace(in.ProjectID)
	if projectID == "" {
		return Link{}, httpx.Invalid("INVALID_SHARE_LINK", "project_id is required")
	}
	if in.MaxViews < 0 {
		return Link{}, httpx.Invalid("INVALID_SHARE_LINK", "max_views must not be negative")
	}
	if err := r.checkProjectVersion(ctx, projectID, in.VersionID); err != nil {
		return Link{}, err
	}
	expires, err := normalizeExpiry(in.ExpiresAt)
	if err != nil {
		return Link{}, err
	}
	hash, err := hashPassword(in.Password)
	if err != nil {
		return Link{}, err
	}

	now := time.Now().UTC()
	link := Link{
		ID:           ids.New("shr"),
		Token:        ids.New("shr"),
		ProjectID:    projectID,
		VersionID:    strings.TrimSpace(in.VersionID),
		ExpiresAt:    expires,
		MaxViews:     in.MaxViews,
		Cache:        cacheOrDefault(in.Cache),
		Enabled:      true,
		CreatedAt:    now,
		UpdatedAt:    now,
		passwordHash: hash,
	}
	link.PasswordSet = hash != ""

	var versionArg, expiresArg any
	if link.VersionID != "" {
		versionArg = link.VersionID
	}
	if link.ExpiresAt != nil {
		expiresArg = *link.ExpiresAt
	}
	cacheJSON, err := json.Marshal(link.Cache)
	if err != nil {
		return Link{}, fmt.Errorf("encode share cache policy: %w", err)
	}
	_, err = r.db.ExecContext(ctx, `INSERT INTO share_links (`+linkColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?)`,
		link.ID, link.Token, link.ProjectID, versionArg, link.passwordHash, expiresArg,
		link.MaxViews, string(cacheJSON), link.Enabled, link.CreatedAt, link.UpdatedAt)
	if err != nil {
		return Link{}, fmt.Errorf("insert share link: %w", err)
	}
	link.Status = link.deriveStatus(now)
	return link, nil
}

// List returns share links, optionally filtered by project, newest first.
func (r *Repo) List(ctx context.Context, projectID string) ([]Link, error) {
	query := `SELECT ` + linkColumns + ` FROM share_links WHERE deleted_at IS NULL`
	args := []any{}
	if projectID = strings.TrimSpace(projectID); projectID != "" {
		query += ` AND project_id = ?`
		args = append(args, projectID)
	}
	query += ` ORDER BY created_at DESC`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list share links: %w", err)
	}
	defer rows.Close()

	out := []Link{}
	now := time.Now().UTC()
	for rows.Next() {
		link, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan share link: %w", err)
		}
		link.Status = link.deriveStatus(now)
		out = append(out, link)
	}
	return out, rows.Err()
}

// Get returns a live share link by id.
func (r *Repo) Get(ctx context.Context, id string) (Link, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+linkColumns+` FROM share_links WHERE id = ? AND deleted_at IS NULL`, id)
	link, err := scan(row)
	if err == sql.ErrNoRows {
		return Link{}, httpx.NotFound("SHARE_LINK_NOT_FOUND", "share link not found")
	}
	if err != nil {
		return Link{}, fmt.Errorf("get share link: %w", err)
	}
	link.Status = link.deriveStatus(time.Now().UTC())
	return link, nil
}

// Update changes mutable attributes of a share link.
func (r *Repo) Update(ctx context.Context, id string, in UpdateInput) (Link, error) {
	current, err := r.Get(ctx, id)
	if err != nil {
		return Link{}, err
	}
	now := time.Now().UTC()

	if in.VersionID != nil {
		versionID := strings.TrimSpace(*in.VersionID)
		if err := r.checkProjectVersion(ctx, current.ProjectID, versionID); err != nil {
			return Link{}, err
		}
		current.VersionID = versionID
	}
	if in.MaxViews != nil {
		if *in.MaxViews < 0 {
			return Link{}, httpx.Invalid("INVALID_SHARE_LINK", "max_views must not be negative")
		}
		current.MaxViews = *in.MaxViews
	}
	if in.Cache != nil {
		current.Cache = cacheOrDefault(in.Cache)
	}
	if in.Enabled != nil {
		current.Enabled = *in.Enabled
	}
	if in.ClearExpiry {
		current.ExpiresAt = nil
	} else if in.ExpiresAt != nil {
		expires, err := normalizeExpiry(in.ExpiresAt)
		if err != nil {
			return Link{}, err
		}
		current.ExpiresAt = expires
	}
	if in.Password != nil {
		hash, err := hashPassword(*in.Password)
		if err != nil {
			return Link{}, err
		}
		current.passwordHash = hash
	}

	cacheJSON, err := json.Marshal(current.Cache)
	if err != nil {
		return Link{}, fmt.Errorf("encode share cache policy: %w", err)
	}
	var versionArg, expiresArg any
	if current.VersionID != "" {
		versionArg = current.VersionID
	}
	if current.ExpiresAt != nil {
		expiresArg = *current.ExpiresAt
	}
	res, err := r.db.ExecContext(ctx, `UPDATE share_links
		SET version_id = ?, password_hash = ?, expires_at = ?, max_views = ?, cache_policy = ?,
		    enabled = ?, updated_at = ?
		WHERE id = ? AND deleted_at IS NULL`,
		versionArg, current.passwordHash, expiresArg, current.MaxViews, string(cacheJSON),
		current.Enabled, now, id)
	if err != nil {
		return Link{}, fmt.Errorf("update share link: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return Link{}, fmt.Errorf("update share link rows: %w", err)
	} else if n == 0 {
		return Link{}, httpx.NotFound("SHARE_LINK_NOT_FOUND", "share link not found")
	}
	current.UpdatedAt = now
	current.PasswordSet = current.passwordHash != ""
	current.Status = current.deriveStatus(now)
	return current, nil
}

// Delete soft-deletes a share link; its token immediately stops resolving.
func (r *Repo) Delete(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE share_links SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		time.Now().UTC(), time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("delete share link: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("delete share link rows: %w", err)
	} else if n == 0 {
		return httpx.NotFound("SHARE_LINK_NOT_FOUND", "share link not found")
	}
	return nil
}

// Authorize validates token, password, expiry and view budget for the data
// plane. countView increments the access counter inside the same transaction so
// max_views cannot be overshot by concurrent requests.
func (r *Repo) Authorize(ctx context.Context, token, password string, countView bool) (Link, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return Link{}, httpx.NotFound("SHARE_LINK_NOT_FOUND", "share link not found")
	}
	var link Link
	err := r.db.WithTx(ctx, func(tx *store.Tx) error {
		row := tx.QueryRowContext(ctx,
			`SELECT `+linkColumns+` FROM share_links WHERE token = ? AND deleted_at IS NULL`, token)
		found, err := scan(row)
		if err == sql.ErrNoRows {
			return httpx.NotFound("SHARE_LINK_NOT_FOUND", "share link not found")
		}
		if err != nil {
			return fmt.Errorf("load share link by token: %w", err)
		}
		now := time.Now().UTC()
		if !found.Enabled {
			return httpx.Errorf(http.StatusForbidden, "SHARE_DISABLED", "share link is disabled")
		}
		if found.ExpiresAt != nil && !now.Before(*found.ExpiresAt) {
			return httpx.Errorf(http.StatusGone, "SHARE_EXPIRED", "share link has expired")
		}
		if found.passwordHash != "" {
			if password == "" {
				return httpx.Errorf(http.StatusUnauthorized, "SHARE_PASSWORD_REQUIRED", "share link requires a password")
			}
			if bcrypt.CompareHashAndPassword([]byte(found.passwordHash), []byte(password)) != nil {
				return httpx.Errorf(http.StatusUnauthorized, "SHARE_PASSWORD_INVALID", "share link password is invalid")
			}
		}
		if found.MaxViews > 0 && found.Views >= found.MaxViews {
			return httpx.Errorf(http.StatusForbidden, "SHARE_VIEWS_EXCEEDED", "share link view limit reached")
		}
		if countView {
			if _, err := tx.ExecContext(ctx,
				`UPDATE share_links SET views = views + 1, updated_at = ? WHERE id = ?`,
				now, found.ID); err != nil {
				return fmt.Errorf("increment share views: %w", err)
			}
			found.Views++
		}
		found.UpdatedAt = now
		found.Status = found.deriveStatus(now)
		link = found
		return nil
	})
	if err != nil {
		return Link{}, err
	}
	return link, nil
}

func (r *Repo) checkProjectVersion(ctx context.Context, projectID, versionID string) error {
	var exists int
	err := r.db.QueryRowContext(ctx,
		`SELECT 1 FROM projects WHERE id = ? AND deleted_at IS NULL`, projectID).Scan(&exists)
	if err == sql.ErrNoRows {
		return httpx.NotFound("PROJECT_NOT_FOUND", "project not found")
	}
	if err != nil {
		return fmt.Errorf("check share project: %w", err)
	}
	versionID = strings.TrimSpace(versionID)
	if versionID == "" {
		return nil
	}
	err = r.db.QueryRowContext(ctx,
		`SELECT 1 FROM versions WHERE id = ? AND project_id = ? AND status = 'ready' AND deleted_at IS NULL`,
		versionID, projectID).Scan(&exists)
	if err == sql.ErrNoRows {
		return httpx.Invalid("VERSION_NOT_AVAILABLE", "version is not ready and available")
	}
	if err != nil {
		return fmt.Errorf("check share version: %w", err)
	}
	return nil
}

func (l Link) deriveStatus(now time.Time) Status {
	if !l.Enabled {
		return StatusDisabled
	}
	if l.ExpiresAt != nil && !now.Before(*l.ExpiresAt) {
		return StatusExpired
	}
	return StatusActive
}

func normalizeExpiry(value *time.Time) (*time.Time, error) {
	if value == nil {
		return nil, nil
	}
	expires := value.UTC()
	if !expires.After(time.Now().UTC()) {
		return nil, httpx.Invalid("INVALID_SHARE_LINK", "expires_at must be in the future")
	}
	return &expires, nil
}

func cacheOrDefault(policy *model.CachePolicy) model.CachePolicy {
	if policy == nil {
		return model.CachePolicy{}.WithDefaults()
	}
	return policy.WithDefaults()
}

func hashPassword(password string) (string, error) {
	if password == "" {
		return "", nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("hash share password: %w", err)
	}
	return string(hash), nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scan(row scanner) (Link, error) {
	var link Link
	var versionID sql.NullString
	var expiresAt sql.NullTime
	var cacheJSON string
	if err := row.Scan(&link.ID, &link.Token, &link.ProjectID, &versionID, &link.passwordHash,
		&expiresAt, &link.MaxViews, &link.Views, &cacheJSON, &link.Enabled, &link.CreatedAt, &link.UpdatedAt); err != nil {
		return Link{}, err
	}
	link.VersionID = versionID.String
	link.PasswordSet = link.passwordHash != ""
	if expiresAt.Valid {
		utc := expiresAt.Time.UTC()
		link.ExpiresAt = &utc
	}
	if cacheJSON != "" {
		if err := json.Unmarshal([]byte(cacheJSON), &link.Cache); err != nil {
			return Link{}, fmt.Errorf("decode share cache policy: %w", err)
		}
	}
	link.Cache = link.Cache.WithDefaults()
	return link, nil
}
