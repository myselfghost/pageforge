// Package alias stores S3 connection settings with encrypted secrets.
package alias

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/pageforge/pageforge/internal/crypto"
	"github.com/pageforge/pageforge/internal/httpx"
	"github.com/pageforge/pageforge/internal/ids"
	"github.com/pageforge/pageforge/internal/snapshot"
	"github.com/pageforge/pageforge/internal/store"
)

// Alias is a stored S3 connection.
type Alias struct {
	ID              string
	Name            string
	Endpoint        string
	Region          string
	AccessKey       string
	SecretCipher    []byte
	PathStyle       bool
	BucketAllowlist []string
	TLSSkipVerify   bool
	CredentialVer   int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// View is the API representation; it never contains the secret.
type View struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	Endpoint          string    `json:"endpoint"`
	Region            string    `json:"region"`
	AccessKey         string    `json:"access_key"`
	SecretKeySet      bool      `json:"secret_key_set"`
	PathStyle         bool      `json:"path_style"`
	BucketAllowlist   []string  `json:"bucket_allowlist"`
	TLSSkipVerify     bool      `json:"tls_skip_verify"`
	CredentialVersion int64     `json:"credential_version"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// View converts an Alias to its API representation.
func (a Alias) View() View {
	allow := a.BucketAllowlist
	if allow == nil {
		allow = []string{}
	}
	return View{
		ID:                a.ID,
		Name:              a.Name,
		Endpoint:          a.Endpoint,
		Region:            a.Region,
		AccessKey:         a.AccessKey,
		SecretKeySet:      len(a.SecretCipher) > 0,
		PathStyle:         a.PathStyle,
		BucketAllowlist:   allow,
		TLSSkipVerify:     a.TLSSkipVerify,
		CredentialVersion: a.CredentialVer,
		CreatedAt:         a.CreatedAt,
		UpdatedAt:         a.UpdatedAt,
	}
}

// CreateInput is the POST /aliases payload.
type CreateInput struct {
	Name            string   `json:"name"`
	Endpoint        string   `json:"endpoint"`
	Region          string   `json:"region"`
	AccessKey       string   `json:"access_key"`
	SecretKey       string   `json:"secret_key"`
	PathStyle       *bool    `json:"path_style"`
	BucketAllowlist []string `json:"bucket_allowlist"`
	TLSSkipVerify   bool     `json:"tls_skip_verify"`
}

// UpdateInput is the PATCH /aliases/{id} payload; nil fields are unchanged.
type UpdateInput struct {
	Name            *string   `json:"name"`
	Endpoint        *string   `json:"endpoint"`
	Region          *string   `json:"region"`
	AccessKey       *string   `json:"access_key"`
	SecretKey       *string   `json:"secret_key"`
	PathStyle       *bool     `json:"path_style"`
	BucketAllowlist *[]string `json:"bucket_allowlist"`
	TLSSkipVerify   *bool     `json:"tls_skip_verify"`
}

// Repo persists aliases.
type Repo struct {
	db     *store.Store
	cipher *crypto.Cipher
}

// NewRepo builds an alias repository.
func NewRepo(db *store.Store, cipher *crypto.Cipher) *Repo {
	return &Repo{db: db, cipher: cipher}
}

const aliasColumns = `id, name, endpoint, region, access_key, secret_cipher, path_style,
	bucket_allowlist, tls_skip_verify, credential_ver, created_at, updated_at`

// Create validates and stores a new alias.
func (r *Repo) Create(ctx context.Context, in CreateInput) (Alias, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > 128 {
		return Alias{}, httpx.Invalid("INVALID_ALIAS", "name must be 1-128 characters")
	}
	endpoint, err := normalizeEndpoint(in.Endpoint)
	if err != nil {
		return Alias{}, err
	}
	region := strings.TrimSpace(in.Region)
	if region == "" {
		region = "us-east-1"
	}
	if len(region) > 64 {
		return Alias{}, httpx.Invalid("INVALID_ALIAS", "region must be at most 64 characters")
	}
	accessKey := strings.TrimSpace(in.AccessKey)
	secretKey := strings.TrimSpace(in.SecretKey)
	if accessKey == "" || secretKey == "" {
		return Alias{}, httpx.Invalid("INVALID_ALIAS", "access_key and secret_key are required")
	}
	allowlist, err := normalizeAllowlist(in.BucketAllowlist)
	if err != nil {
		return Alias{}, err
	}
	sealed, err := r.cipher.EncryptString(secretKey)
	if err != nil {
		return Alias{}, fmt.Errorf("encrypt secret: %w", err)
	}
	pathStyle := true
	if in.PathStyle != nil {
		pathStyle = *in.PathStyle
	}
	now := time.Now().UTC()
	a := Alias{
		ID:              ids.New(ids.Alias),
		Name:            name,
		Endpoint:        endpoint,
		Region:          region,
		AccessKey:       accessKey,
		SecretCipher:    sealed,
		PathStyle:       pathStyle,
		BucketAllowlist: allowlist,
		TLSSkipVerify:   in.TLSSkipVerify,
		CredentialVer:   1,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	allowJSON, _ := json.Marshal(allowlist)
	_, err = r.db.ExecContext(ctx, `INSERT INTO aliases (`+aliasColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.Name, a.Endpoint, a.Region, a.AccessKey, a.SecretCipher, a.PathStyle,
		string(allowJSON), a.TLSSkipVerify, a.CredentialVer, a.CreatedAt, a.UpdatedAt)
	if err != nil {
		if store.IsUniqueViolation(err) {
			return Alias{}, httpx.Conflict("ALIAS_NAME_TAKEN", "alias name is already in use")
		}
		return Alias{}, fmt.Errorf("insert alias: %w", err)
	}
	return a, nil
}

// Get returns one alias or ALIAS_NOT_FOUND.
func (r *Repo) Get(ctx context.Context, id string) (Alias, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+aliasColumns+` FROM aliases WHERE id = ?`, id)
	a, err := scan(row)
	if err == sql.ErrNoRows {
		return Alias{}, httpx.NotFound("ALIAS_NOT_FOUND", "alias not found")
	}
	if err != nil {
		return Alias{}, fmt.Errorf("get alias: %w", err)
	}
	return a, nil
}

// List returns all aliases ordered by name.
func (r *Repo) List(ctx context.Context) ([]Alias, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+aliasColumns+` FROM aliases ORDER BY name ASC`)
	if err != nil {
		return nil, fmt.Errorf("list aliases: %w", err)
	}
	defer rows.Close()

	var out []Alias
	for rows.Next() {
		a, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan alias: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Update applies a partial update. Changing the secret bumps credential_ver.
func (r *Repo) Update(ctx context.Context, id string, in UpdateInput) (Alias, error) {
	a, err := r.Get(ctx, id)
	if err != nil {
		return Alias{}, err
	}
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" || len(name) > 128 {
			return Alias{}, httpx.Invalid("INVALID_ALIAS", "name must be 1-128 characters")
		}
		a.Name = name
	}
	if in.Endpoint != nil {
		endpoint, err := normalizeEndpoint(*in.Endpoint)
		if err != nil {
			return Alias{}, err
		}
		a.Endpoint = endpoint
	}
	if in.Region != nil {
		region := strings.TrimSpace(*in.Region)
		if region == "" || len(region) > 64 {
			return Alias{}, httpx.Invalid("INVALID_ALIAS", "region must be 1-64 characters")
		}
		a.Region = region
	}
	if in.AccessKey != nil {
		accessKey := strings.TrimSpace(*in.AccessKey)
		if accessKey == "" {
			return Alias{}, httpx.Invalid("INVALID_ALIAS", "access_key must not be empty")
		}
		a.AccessKey = accessKey
	}
	if in.SecretKey != nil {
		secretKey := strings.TrimSpace(*in.SecretKey)
		if secretKey == "" {
			return Alias{}, httpx.Invalid("INVALID_ALIAS", "secret_key must not be empty")
		}
		sealed, err := r.cipher.EncryptString(secretKey)
		if err != nil {
			return Alias{}, fmt.Errorf("encrypt secret: %w", err)
		}
		a.SecretCipher = sealed
		a.CredentialVer++
	}
	if in.PathStyle != nil {
		a.PathStyle = *in.PathStyle
	}
	if in.BucketAllowlist != nil {
		allowlist, err := normalizeAllowlist(*in.BucketAllowlist)
		if err != nil {
			return Alias{}, err
		}
		a.BucketAllowlist = allowlist
	}
	if in.TLSSkipVerify != nil {
		a.TLSSkipVerify = *in.TLSSkipVerify
	}
	a.UpdatedAt = time.Now().UTC()

	allowJSON, _ := json.Marshal(a.BucketAllowlist)
	_, err = r.db.ExecContext(ctx, `UPDATE aliases SET name = ?, endpoint = ?, region = ?,
		access_key = ?, secret_cipher = ?, path_style = ?, bucket_allowlist = ?,
		tls_skip_verify = ?, credential_ver = ?, updated_at = ? WHERE id = ?`,
		a.Name, a.Endpoint, a.Region, a.AccessKey, a.SecretCipher, a.PathStyle,
		string(allowJSON), a.TLSSkipVerify, a.CredentialVer, a.UpdatedAt, a.ID)
	if err != nil {
		if store.IsUniqueViolation(err) {
			return Alias{}, httpx.Conflict("ALIAS_NAME_TAKEN", "alias name is already in use")
		}
		return Alias{}, fmt.Errorf("update alias: %w", err)
	}
	return a, nil
}

// Delete removes an alias that no live project references.
func (r *Repo) Delete(ctx context.Context, id string) error {
	var refs int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM projects WHERE alias_id = ? AND deleted_at IS NULL`, id).Scan(&refs); err != nil {
		return fmt.Errorf("count project references: %w", err)
	}
	if refs > 0 {
		return httpx.Conflict("ALIAS_IN_USE", "alias is referenced by projects")
	}
	res, err := r.db.ExecContext(ctx, `DELETE FROM aliases WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete alias: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete alias rows: %w", err)
	}
	if n == 0 {
		return httpx.NotFound("ALIAS_NOT_FOUND", "alias not found")
	}
	return nil
}

// Secret decrypts the stored secret for the internal credentials API.
func (r *Repo) Secret(ctx context.Context, id string) (string, string, error) {
	a, err := r.Get(ctx, id)
	if err != nil {
		return "", "", err
	}
	secret, err := r.cipher.DecryptString(a.SecretCipher)
	if err != nil {
		return "", "", fmt.Errorf("decrypt secret: %w", err)
	}
	return a.AccessKey, secret, nil
}

// Directory returns every alias's credentials exactly as stored: the secret
// stays AES-256-GCM sealed so the control plane never hands out plaintext. The
// data plane decrypts in memory with the shared master key. The result must
// never be logged.
func (r *Repo) Directory(ctx context.Context) (snapshot.CredentialDirectory, error) {
	aliases, err := r.List(ctx)
	if err != nil {
		return snapshot.CredentialDirectory{}, err
	}
	dir := snapshot.CredentialDirectory{Aliases: make(map[string]snapshot.AliasCredentials, len(aliases))}
	for _, a := range aliases {
		dir.Aliases[a.ID] = snapshot.AliasCredentials{
			AccessKey:     a.AccessKey,
			SecretCipher:  a.SecretCipher,
			CredentialVer: a.CredentialVer,
		}
	}
	return dir, nil
}

// AllowsBucket reports whether bucket is permitted by the alias allowlist.
func (a Alias) AllowsBucket(bucket string) bool {
	if len(a.BucketAllowlist) == 0 {
		return true
	}
	for _, allowed := range a.BucketAllowlist {
		if allowed == bucket {
			return true
		}
	}
	return false
}

func normalizeEndpoint(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	u, err := url.Parse(value)
	if err != nil || u.Host == "" {
		return "", httpx.Invalid("INVALID_ALIAS", "endpoint must be an absolute http(s) URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", httpx.Invalid("INVALID_ALIAS", "endpoint scheme must be http or https")
	}
	if u.User != nil {
		return "", httpx.Invalid("INVALID_ALIAS", "endpoint must not contain credentials")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", httpx.Invalid("INVALID_ALIAS", "endpoint must not contain a query or fragment")
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u.String(), nil
}

func normalizeAllowlist(values []string) ([]string, error) {
	if len(values) == 0 {
		return []string{}, nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, raw := range values {
		bucket := strings.TrimSpace(raw)
		if bucket == "" || strings.ContainsAny(bucket, "/\\ \t\r\n") {
			return nil, httpx.Invalid("INVALID_ALIAS", "bucket_allowlist contains an invalid bucket name")
		}
		if _, dup := seen[bucket]; dup {
			continue
		}
		seen[bucket] = struct{}{}
		out = append(out, bucket)
	}
	return out, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scan(row scanner) (Alias, error) {
	var a Alias
	var allowJSON string
	if err := row.Scan(&a.ID, &a.Name, &a.Endpoint, &a.Region, &a.AccessKey, &a.SecretCipher,
		&a.PathStyle, &allowJSON, &a.TLSSkipVerify, &a.CredentialVer, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return Alias{}, err
	}
	if allowJSON != "" {
		if err := json.Unmarshal([]byte(allowJSON), &a.BucketAllowlist); err != nil {
			return Alias{}, fmt.Errorf("decode bucket_allowlist: %w", err)
		}
	}
	if a.BucketAllowlist == nil {
		a.BucketAllowlist = []string{}
	}
	return a, nil
}
