package snapshot

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/pageforge/pageforge/internal/model"
	"github.com/pageforge/pageforge/internal/store"
)

// Generator builds snapshots from the control-plane database. It keeps the
// last serialized result and only bumps the generation when the content
// actually changes, so polling nodes keep receiving 304 responses.
type Generator struct {
	db        *store.Store
	warnBytes int
	logger    *slog.Logger

	mu         sync.Mutex
	cached     *Snapshot
	cachedJSON []byte
	cachedHash [32]byte
}

// NewGenerator builds a snapshot generator. warnBytes logs a warning when the
// serialized snapshot exceeds it; zero disables the warning.
func NewGenerator(db *store.Store, warnBytes int, logger *slog.Logger) *Generator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Generator{db: db, warnBytes: warnBytes, logger: logger}
}

// Current returns the newest snapshot and its JSON encoding. The generation is
// bumped only when the underlying content changed.
func (g *Generator) Current(ctx context.Context) (Snapshot, []byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	content, err := g.load(ctx, g.db)
	if err != nil {
		return Snapshot{}, nil, err
	}
	hash, err := contentHash(content)
	if err != nil {
		return Snapshot{}, nil, err
	}
	if g.cached != nil && g.cachedHash == hash {
		return *g.cached, g.cachedJSON, nil
	}
	return g.rebuild(ctx)
}

// Reload regenerates unconditionally, bumping the generation even when the
// content is unchanged.
func (g *Generator) Reload(ctx context.Context) (Snapshot, []byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.rebuild(ctx)
}

func (g *Generator) rebuild(ctx context.Context) (Snapshot, []byte, error) {
	var snap Snapshot
	err := g.db.WithTx(ctx, func(tx *store.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE snapshot_state SET generation = generation + 1, updated_at = ? WHERE id = 1`,
			time.Now().UTC()); err != nil {
			return fmt.Errorf("bump snapshot generation: %w", err)
		}
		var generation uint64
		if err := tx.QueryRowContext(ctx,
			`SELECT generation FROM snapshot_state WHERE id = 1`).Scan(&generation); err != nil {
			return fmt.Errorf("read snapshot generation: %w", err)
		}
		content, err := g.load(ctx, tx)
		if err != nil {
			return err
		}
		content.Generation = generation
		content.PublishedAt = time.Now().UTC()
		snap = content
		return nil
	})
	if err != nil {
		return Snapshot{}, nil, err
	}
	body, err := json.Marshal(snap)
	if err != nil {
		return Snapshot{}, nil, fmt.Errorf("encode snapshot: %w", err)
	}
	if g.warnBytes > 0 && len(body) > g.warnBytes {
		g.logger.Warn("snapshot exceeds the configured warning size",
			slog.Int("bytes", len(body)), slog.Int("warn_bytes", g.warnBytes))
	}
	hash, err := contentHash(snap)
	if err != nil {
		return Snapshot{}, nil, err
	}
	cached := snap
	g.cached = &cached
	g.cachedJSON = body
	g.cachedHash = hash
	return snap, body, nil
}

// contentHash hashes the generation-independent part of a snapshot.
func contentHash(snap Snapshot) ([32]byte, error) {
	snap.Generation = 0
	snap.PublishedAt = time.Time{}
	raw, err := json.Marshal(snap)
	if err != nil {
		return [32]byte{}, fmt.Errorf("encode snapshot for hashing: %w", err)
	}
	return sha256.Sum256(raw), nil
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (g *Generator) load(ctx context.Context, q queryer) (Snapshot, error) {
	snap := Snapshot{
		Aliases:      map[string]AliasView{},
		Projects:     map[string]ProjectView{},
		Bindings:     []BindingView{},
		AuthPolicies: map[string]AuthPolicyView{},
	}
	if err := loadAliases(ctx, q, &snap); err != nil {
		return Snapshot{}, err
	}
	if err := loadProjects(ctx, q, &snap); err != nil {
		return Snapshot{}, err
	}
	if err := loadBindings(ctx, q, &snap); err != nil {
		return Snapshot{}, err
	}
	if err := loadAuthPolicies(ctx, q, &snap); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

func loadAliases(ctx context.Context, q queryer, snap *Snapshot) error {
	rows, err := q.QueryContext(ctx,
		`SELECT id, endpoint, region, path_style, bucket_allowlist, credential_ver FROM aliases`)
	if err != nil {
		return fmt.Errorf("query aliases: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var view AliasView
		var allowJSON string
		if err := rows.Scan(&id, &view.Endpoint, &view.Region, &view.PathStyle, &allowJSON, &view.CredentialVer); err != nil {
			return fmt.Errorf("scan alias: %w", err)
		}
		view.BucketAllow = []string{}
		if allowJSON != "" {
			if err := json.Unmarshal([]byte(allowJSON), &view.BucketAllow); err != nil {
				return fmt.Errorf("decode alias bucket_allowlist: %w", err)
			}
			if view.BucketAllow == nil {
				view.BucketAllow = []string{}
			}
		}
		snap.Aliases[id] = view
	}
	return rows.Err()
}

func loadProjects(ctx context.Context, q queryer, snap *Snapshot) error {
	rows, err := q.QueryContext(ctx, `SELECT id, alias_id, bucket, current_version_id, index_doc,
		error_doc, try_files_enabled, spa_config, cache_policy, response_headers, status
		FROM projects WHERE deleted_at IS NULL`)
	if err != nil {
		return fmt.Errorf("query projects: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var current sql.NullString
		var view ProjectView
		var spaJSON, cacheJSON, headersJSON string
		if err := rows.Scan(&id, &view.AliasID, &view.Bucket, &current, &view.IndexDoc,
			&view.ErrorDoc, &view.TryFilesEnabled, &spaJSON, &cacheJSON, &headersJSON, &view.Status); err != nil {
			return fmt.Errorf("scan project: %w", err)
		}
		view.CurrentVersionID = current.String
		if spaJSON != "" {
			if err := json.Unmarshal([]byte(spaJSON), &view.SPA); err != nil {
				return fmt.Errorf("decode project spa_config: %w", err)
			}
		}
		if cacheJSON != "" {
			if err := json.Unmarshal([]byte(cacheJSON), &view.Cache); err != nil {
				return fmt.Errorf("decode project cache_policy: %w", err)
			}
			view.Cache = view.Cache.WithDefaults()
		}
		if headersJSON != "" {
			if err := json.Unmarshal([]byte(headersJSON), &view.ResponseHeaders); err != nil {
				return fmt.Errorf("decode project response_headers: %w", err)
			}
		}
		if view.ResponseHeaders == nil {
			view.ResponseHeaders = []model.HeaderRule{}
		}
		snap.Projects[id] = view
	}
	return rows.Err()
}

func loadBindings(ctx context.Context, q queryer, snap *Snapshot) error {
	rows, err := q.QueryContext(ctx, `SELECT id, host, path_prefix, project_id, pinned_version_id,
		auth_policy_id, response_headers, enabled FROM bindings WHERE deleted_at IS NULL
		ORDER BY host ASC, path_prefix ASC`)
	if err != nil {
		return fmt.Errorf("query bindings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var view BindingView
		var pinned sql.NullString
		var headersJSON string
		if err := rows.Scan(&view.ID, &view.Host, &view.PathPrefix, &view.ProjectID, &pinned,
			&view.AuthPolicyID, &headersJSON, &view.Enabled); err != nil {
			return fmt.Errorf("scan binding: %w", err)
		}
		view.PinnedVersionID = pinned.String
		if headersJSON != "" {
			if err := json.Unmarshal([]byte(headersJSON), &view.ResponseHeaders); err != nil {
				return fmt.Errorf("decode binding response_headers: %w", err)
			}
		}
		if view.ResponseHeaders == nil {
			view.ResponseHeaders = []model.HeaderRule{}
		}
		snap.Bindings = append(snap.Bindings, view)
	}
	return rows.Err()
}

func loadAuthPolicies(ctx context.Context, q queryer, snap *Snapshot) error {
	rows, err := q.QueryContext(ctx, `SELECT id, name, mode, secret_version FROM auth_policies`)
	if err != nil {
		return fmt.Errorf("query auth policies: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var view AuthPolicyView
		if err := rows.Scan(&view.ID, &view.Realm, &view.Mode, &view.SecretVersion); err != nil {
			return fmt.Errorf("scan auth policy: %w", err)
		}
		snap.AuthPolicies[view.ID] = view
	}
	return rows.Err()
}
