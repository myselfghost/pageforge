// Package credential resolves S3 credentials for aliases from the control
// plane and caches the resulting object stores per credential version.
package credential

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pageforge/pageforge/internal/objectstore"
	"github.com/pageforge/pageforge/internal/snapshot"
)

// Credentials is the internal API response. It must never be logged.
type Credentials struct {
	AccessKey     string `json:"access_key"`
	SecretKey     string `json:"secret_key"`
	CredentialVer int64  `json:"credential_version"`
}

// Client fetches and caches alias credentials.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	Pool    *objectstore.Pool

	// ConnectTimeout bounds TCP/TLS connect to the S3 endpoint.
	ConnectTimeout time.Duration
	// ReadTimeout bounds waiting for the first response byte from S3.
	ReadTimeout time.Duration

	mu    sync.Mutex
	cache map[string]Credentials
}

// NewClient builds a credential client. baseURL points at the control plane.
func NewClient(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 10 * time.Second},
		Pool:    objectstore.NewPool(),
		cache:   map[string]Credentials{},
	}
}

// Store returns an object store for the alias, refetching credentials when the
// snapshot's credential version is newer than the cached one.
func (c *Client) Store(ctx context.Context, aliasID string, view snapshot.AliasView) (objectstore.ObjectStore, error) {
	creds, ok := c.cached(aliasID, view.CredentialVer)
	if !ok {
		fetched, err := c.Fetch(ctx, aliasID)
		if err != nil {
			return nil, err
		}
		creds = fetched
		c.remember(aliasID, fetched)
	}
	return c.Pool.Get(aliasID, creds.CredentialVer, objectstore.Credentials{
		Endpoint:       view.Endpoint,
		Region:         view.Region,
		AccessKey:      creds.AccessKey,
		SecretKey:      creds.SecretKey,
		PathStyle:      view.PathStyle,
		ConnectTimeout: c.ConnectTimeout,
		ReadTimeout:    c.ReadTimeout,
	})
}

// Fetch retrieves credentials from the control plane.
func (c *Client) Fetch(ctx context.Context, aliasID string) (Credentials, error) {
	url := c.BaseURL + "/internal/v1/aliases/" + aliasID + "/credentials"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Credentials{}, fmt.Errorf("build credentials request: %w", err)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Credentials{}, fmt.Errorf("fetch credentials: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Credentials{}, fmt.Errorf("credentials endpoint returned %s", resp.Status)
	}
	var creds Credentials
	if err := json.NewDecoder(resp.Body).Decode(&creds); err != nil {
		return Credentials{}, fmt.Errorf("decode credentials: %w", err)
	}
	if creds.AccessKey == "" || creds.SecretKey == "" {
		return Credentials{}, fmt.Errorf("credentials for alias %s are empty", aliasID)
	}
	return creds, nil
}

func (c *Client) cached(aliasID string, version int64) (Credentials, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	creds, ok := c.cache[aliasID]
	if !ok || creds.CredentialVer < version {
		return Credentials{}, false
	}
	return creds, true
}

func (c *Client) remember(aliasID string, creds Credentials) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == nil {
		c.cache = map[string]Credentials{}
	}
	if existing, ok := c.cache[aliasID]; ok && existing.CredentialVer > creds.CredentialVer {
		return
	}
	c.cache[aliasID] = creds
}
