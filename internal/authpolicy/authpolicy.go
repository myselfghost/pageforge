// Package authpolicy stores URL-level authentication policies and their users.
package authpolicy

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/pageforge/pageforge/internal/httpx"
	"github.com/pageforge/pageforge/internal/ids"
	"github.com/pageforge/pageforge/internal/snapshot"
	"github.com/pageforge/pageforge/internal/store"
)

// Mode is the authentication scheme of a policy.
type Mode string

const (
	ModeBasic Mode = "basic"
)

const bcryptCost = 10

// Policy is an authentication policy. It never carries password material.
type Policy struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Mode          Mode      `json:"mode"`
	SecretVersion int64     `json:"secret_version"`
	UserCount     int       `json:"user_count"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// UserView is a user entry without the password hash.
type UserView struct {
	Username        string `json:"username"`
	PasswordHashSet bool   `json:"password_hash_set"`
}

// Detail is a policy together with its users.
type Detail struct {
	Policy
	Users []UserView `json:"users"`
}

// CreateInput is the POST /auth-policies payload.
type CreateInput struct {
	Name string `json:"name"`
	Mode string `json:"mode"`
}

// UpdateInput is the PATCH /auth-policies/{id} payload.
type UpdateInput struct {
	Name *string `json:"name"`
}

// UserInput is the PUT /auth-policies/{id}/users/{username} payload.
type UserInput struct {
	Password string `json:"password"`
}

// Repo persists authentication policies.
type Repo struct {
	db *store.Store
}

// NewRepo builds an authentication policy repository.
func NewRepo(db *store.Store) *Repo {
	return &Repo{db: db}
}

const policyColumns = `id, name, mode, secret_version, created_at, updated_at`

// List returns all policies with their user counts.
func (r *Repo) List(ctx context.Context) ([]Policy, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT p.id, p.name, p.mode, p.secret_version,
		p.created_at, p.updated_at, COUNT(u.username)
		FROM auth_policies p LEFT JOIN auth_users u ON u.policy_id = p.id
		GROUP BY p.id, p.name, p.mode, p.secret_version, p.created_at, p.updated_at
		ORDER BY p.name ASC`)
	if err != nil {
		return nil, fmt.Errorf("list auth policies: %w", err)
	}
	defer rows.Close()

	out := []Policy{}
	for rows.Next() {
		var p Policy
		var mode string
		if err := rows.Scan(&p.ID, &p.Name, &mode, &p.SecretVersion,
			&p.CreatedAt, &p.UpdatedAt, &p.UserCount); err != nil {
			return nil, fmt.Errorf("scan auth policy: %w", err)
		}
		p.Mode = Mode(mode)
		out = append(out, p)
	}
	return out, rows.Err()
}

// Get returns a policy with its usernames (never password hashes).
func (r *Repo) Get(ctx context.Context, id string) (Detail, error) {
	var p Detail
	var mode string
	err := r.db.QueryRowContext(ctx, `SELECT `+policyColumns+` FROM auth_policies WHERE id = ?`, id).
		Scan(&p.ID, &p.Name, &mode, &p.SecretVersion, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return Detail{}, httpx.NotFound("AUTH_POLICY_NOT_FOUND", "auth policy not found")
	}
	if err != nil {
		return Detail{}, fmt.Errorf("get auth policy: %w", err)
	}
	p.Mode = Mode(mode)

	rows, err := r.db.QueryContext(ctx,
		`SELECT username FROM auth_users WHERE policy_id = ? ORDER BY username ASC`, id)
	if err != nil {
		return Detail{}, fmt.Errorf("list auth users: %w", err)
	}
	defer rows.Close()
	p.Users = []UserView{}
	for rows.Next() {
		var username string
		if err := rows.Scan(&username); err != nil {
			return Detail{}, fmt.Errorf("scan auth user: %w", err)
		}
		p.Users = append(p.Users, UserView{Username: username, PasswordHashSet: true})
	}
	if err := rows.Err(); err != nil {
		return Detail{}, err
	}
	p.UserCount = len(p.Users)
	return p, nil
}

// Create validates and stores a new policy.
func (r *Repo) Create(ctx context.Context, in CreateInput) (Policy, error) {
	name, err := validateName(in.Name)
	if err != nil {
		return Policy{}, err
	}
	mode := Mode(strings.ToLower(strings.TrimSpace(in.Mode)))
	if mode == "" {
		mode = ModeBasic
	}
	if mode != ModeBasic {
		return Policy{}, httpx.Invalid("INVALID_AUTH_POLICY", "mode must be basic")
	}
	now := time.Now().UTC()
	p := Policy{
		ID:            ids.New(ids.AuthPolicy),
		Name:          name,
		Mode:          mode,
		SecretVersion: 1,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if _, err := r.db.ExecContext(ctx, `INSERT INTO auth_policies
		(id, name, mode, config, secret_version, created_at, updated_at)
		VALUES (?, ?, ?, '{}', ?, ?, ?)`,
		p.ID, p.Name, string(p.Mode), p.SecretVersion, p.CreatedAt, p.UpdatedAt); err != nil {
		return Policy{}, fmt.Errorf("insert auth policy: %w", err)
	}
	return p, nil
}

// Update changes the policy name.
func (r *Repo) Update(ctx context.Context, id string, in UpdateInput) (Policy, error) {
	p, err := r.Get(ctx, id)
	if err != nil {
		return Policy{}, err
	}
	if in.Name != nil {
		name, err := validateName(*in.Name)
		if err != nil {
			return Policy{}, err
		}
		p.Name = name
	}
	p.UpdatedAt = time.Now().UTC()
	if _, err := r.db.ExecContext(ctx, `UPDATE auth_policies SET name = ?, updated_at = ? WHERE id = ?`,
		p.Name, p.UpdatedAt, p.ID); err != nil {
		return Policy{}, fmt.Errorf("update auth policy: %w", err)
	}
	return p.Policy, nil
}

// Delete removes a policy that is not referenced by any binding.
func (r *Repo) Delete(ctx context.Context, id string) error {
	if _, err := r.Get(ctx, id); err != nil {
		return err
	}
	var used int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM bindings WHERE auth_policy_id = ? AND deleted_at IS NULL`, id).Scan(&used); err != nil {
		return fmt.Errorf("count policy bindings: %w", err)
	}
	if used > 0 {
		return httpx.Conflict("AUTH_POLICY_IN_USE", "auth policy is referenced by bindings")
	}
	return r.db.WithTx(ctx, func(tx *store.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM auth_users WHERE policy_id = ?`, id); err != nil {
			return fmt.Errorf("delete auth users: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM auth_policies WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete auth policy: %w", err)
		}
		return nil
	})
}

// UpsertUser creates or replaces a user's Basic credentials.
func (r *Repo) UpsertUser(ctx context.Context, policyID, username string, in UserInput) (UserView, error) {
	if _, err := r.Get(ctx, policyID); err != nil {
		return UserView{}, err
	}
	username, err := validateUsername(username)
	if err != nil {
		return UserView{}, err
	}
	hash, err := hashPassword(in.Password)
	if err != nil {
		return UserView{}, err
	}

	err = r.db.WithTx(ctx, func(tx *store.Tx) error {
		var exists int
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM auth_users WHERE policy_id = ? AND username = ?`, policyID, username).Scan(&exists)
		switch {
		case err == sql.ErrNoRows:
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO auth_users (policy_id, username, password_hash) VALUES (?, ?, ?)`,
				policyID, username, hash); err != nil {
				return fmt.Errorf("insert auth user: %w", err)
			}
		case err != nil:
			return fmt.Errorf("check auth user: %w", err)
		default:
			if _, err := tx.ExecContext(ctx,
				`UPDATE auth_users SET password_hash = ? WHERE policy_id = ? AND username = ?`,
				hash, policyID, username); err != nil {
				return fmt.Errorf("update auth user: %w", err)
			}
		}
		return bumpSecretVersion(ctx, tx, policyID)
	})
	if err != nil {
		return UserView{}, err
	}
	return UserView{Username: username, PasswordHashSet: true}, nil
}

// DeleteUser removes one user and bumps the policy secret version.
func (r *Repo) DeleteUser(ctx context.Context, policyID, username string) error {
	if _, err := r.Get(ctx, policyID); err != nil {
		return err
	}
	username, err := validateUsername(username)
	if err != nil {
		return err
	}
	return r.db.WithTx(ctx, func(tx *store.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM auth_users WHERE policy_id = ? AND username = ?`, policyID, username)
		if err != nil {
			return fmt.Errorf("delete auth user: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("delete auth user rows: %w", err)
		}
		if n == 0 {
			return httpx.NotFound("AUTH_USER_NOT_FOUND", "auth user not found")
		}
		return bumpSecretVersion(ctx, tx, policyID)
	})
}

// Directory returns the password material consumed by data-plane nodes. It
// must never be exposed through the snapshot or logged.
func (r *Repo) Directory(ctx context.Context) (snapshot.AuthDirectory, error) {
	dir := snapshot.AuthDirectory{Policies: map[string]snapshot.AuthPolicySecrets{}}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, name, mode, secret_version FROM auth_policies ORDER BY id ASC`)
	if err != nil {
		return snapshot.AuthDirectory{}, fmt.Errorf("query auth policies: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, name, mode string
		var version int64
		if err := rows.Scan(&id, &name, &mode, &version); err != nil {
			return snapshot.AuthDirectory{}, fmt.Errorf("scan auth policy: %w", err)
		}
		dir.Policies[id] = snapshot.AuthPolicySecrets{
			Realm:         name,
			Mode:          mode,
			SecretVersion: version,
			Users:         map[string]string{},
		}
	}
	if err := rows.Err(); err != nil {
		return snapshot.AuthDirectory{}, err
	}

	userRows, err := r.db.QueryContext(ctx,
		`SELECT policy_id, username, password_hash FROM auth_users ORDER BY policy_id ASC, username ASC`)
	if err != nil {
		return snapshot.AuthDirectory{}, fmt.Errorf("query auth users: %w", err)
	}
	defer userRows.Close()
	for userRows.Next() {
		var policyID, username, hash string
		if err := userRows.Scan(&policyID, &username, &hash); err != nil {
			return snapshot.AuthDirectory{}, fmt.Errorf("scan auth user: %w", err)
		}
		policy, ok := dir.Policies[policyID]
		if !ok {
			continue
		}
		policy.Users[username] = hash
		dir.Policies[policyID] = policy
	}
	return dir, userRows.Err()
}

func bumpSecretVersion(ctx context.Context, tx *store.Tx, policyID string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE auth_policies
		SET secret_version = secret_version + 1, updated_at = ? WHERE id = ?`,
		time.Now().UTC(), policyID); err != nil {
		return fmt.Errorf("bump auth policy secret version: %w", err)
	}
	return nil
}

func hashPassword(password string) (string, error) {
	if password == "" {
		return "", httpx.Invalid("INVALID_AUTH_USER", "password is required")
	}
	if len(password) > 72 {
		return "", httpx.Invalid("INVALID_AUTH_USER", "password must be at most 72 bytes")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(hash), nil
}

func validateName(value string) (string, error) {
	name := strings.TrimSpace(value)
	if name == "" || len(name) > 128 {
		return "", httpx.Invalid("INVALID_AUTH_POLICY", "name must be 1-128 characters")
	}
	if strings.ContainsAny(name, "\r\n\t") {
		return "", httpx.Invalid("INVALID_AUTH_POLICY", "name must not contain control characters")
	}
	return name, nil
}

func validateUsername(value string) (string, error) {
	username := strings.TrimSpace(value)
	if username == "" || len(username) > 64 {
		return "", httpx.Invalid("INVALID_AUTH_USER", "username must be 1-64 characters")
	}
	if strings.ContainsAny(username, ":\r\n\t") {
		return "", httpx.Invalid("INVALID_AUTH_USER", "username must not contain ':' or control characters")
	}
	return username, nil
}
