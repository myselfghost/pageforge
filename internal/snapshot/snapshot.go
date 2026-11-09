// Package snapshot defines the read-only view the control plane publishes to
// the data plane and the machinery that generates, polls and stores it.
package snapshot

import (
	"fmt"
	"strings"
	"time"

	"github.com/pageforge/pageforge/internal/model"
)

// Snapshot is the complete data-plane view of aliases, projects and routes.
type Snapshot struct {
	Generation   uint64                    `json:"generation"`
	PublishedAt  time.Time                 `json:"published_at"`
	Aliases      map[string]AliasView      `json:"aliases"`
	Projects     map[string]ProjectView    `json:"projects"`
	Bindings     []BindingView             `json:"bindings"`
	AuthPolicies map[string]AuthPolicyView `json:"auth_policies"`
}

// AliasView never contains S3 credentials; the data plane fetches those from
// the internal credentials API using credential_version.
type AliasView struct {
	Endpoint      string   `json:"endpoint"`
	Region        string   `json:"region"`
	PathStyle     bool     `json:"path_style"`
	BucketAllow   []string `json:"bucket_allowlist"`
	CredentialVer int64    `json:"credential_version"`
}

// ProjectView is the project field set visible to the data plane.
type ProjectView struct {
	AliasID          string             `json:"alias_id"`
	Bucket           string             `json:"bucket"`
	CurrentVersionID string             `json:"current_version_id"`
	IndexDoc         string             `json:"index_doc"`
	ErrorDoc         string             `json:"error_doc"`
	TryFilesEnabled  bool               `json:"try_files_enabled"`
	SPA              model.SPAConfig    `json:"spa"`
	Cache            model.CachePolicy  `json:"cache"`
	ResponseHeaders  []model.HeaderRule `json:"response_headers"`
	Status           string             `json:"status"`
}

// BindingView is a live binding visible to the data plane.
type BindingView struct {
	ID              string             `json:"id"`
	Host            string             `json:"host"`
	PathPrefix      string             `json:"path_prefix"`
	ProjectID       string             `json:"project_id"`
	PinnedVersionID string             `json:"pinned_version_id"`
	AuthPolicyID    string             `json:"auth_policy_id"`
	ResponseHeaders []model.HeaderRule `json:"response_headers"`
	Enabled         bool               `json:"enabled"`
}

// AuthPolicyView carries no users or hashes; hashes are fetched separately.
type AuthPolicyView struct {
	ID            string `json:"id"`
	Realm         string `json:"realm"`
	Mode          string `json:"mode"`
	SecretVersion int64  `json:"secret_version"`
}

// AuthDirectory carries password hashes fetched separately from the snapshot
// (FR-08). It is never serialized into the snapshot body.
type AuthDirectory struct {
	Policies map[string]AuthPolicySecrets `json:"policies"`
}

// AuthPolicySecrets is one policy's password material, keyed by username.
type AuthPolicySecrets struct {
	Realm         string            `json:"realm"`
	Mode          string            `json:"mode"`
	SecretVersion int64             `json:"secret_version"`
	Users         map[string]string `json:"users"`
}

// Validate rejects snapshots a data plane must not activate.
func (s *Snapshot) Validate() error {
	if s.Generation == 0 {
		return fmt.Errorf("snapshot generation must be positive")
	}
	seen := make(map[string]struct{}, len(s.Bindings))
	for _, b := range s.Bindings {
		if b.ID == "" {
			return fmt.Errorf("binding id must not be empty")
		}
		if _, dup := seen[b.ID]; dup {
			return fmt.Errorf("duplicate binding id %q", b.ID)
		}
		seen[b.ID] = struct{}{}
		if b.Host == "" {
			return fmt.Errorf("binding %s has an empty host", b.ID)
		}
		if !strings.HasPrefix(b.PathPrefix, "/") {
			return fmt.Errorf("binding %s path prefix %q must start with /", b.ID, b.PathPrefix)
		}
	}
	for id, p := range s.Projects {
		if p.AliasID == "" || p.Bucket == "" {
			return fmt.Errorf("project %s is missing alias_id or bucket", id)
		}
	}
	return nil
}
