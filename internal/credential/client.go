// Package credential resolves S3 credentials for aliases from the directory the
// poller mirrors beside the snapshot, and caches the resulting object stores per
// credential version. It never talks to the control plane itself.
package credential

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/pageforge/pageforge/internal/crypto"
	"github.com/pageforge/pageforge/internal/objectstore"
	"github.com/pageforge/pageforge/internal/snapshot"
)

// DirectorySource is the credential directory the poller keeps in memory.
type DirectorySource interface {
	Credentials() *snapshot.CredentialDirectory
}

// Client resolves object stores from the mirrored credential directory. Secrets
// arrive AES-256-GCM sealed (identical to the control-plane database) and are
// decrypted in memory here, so plaintext never touches disk on either side.
type Client struct {
	Dir  DirectorySource
	Pool *objectstore.Pool
	// Cipher decrypts the mirrored secrets. It must match the control plane's
	// master key; a nil cipher makes every lookup fail.
	Cipher *crypto.Cipher

	// ConnectTimeout bounds TCP/TLS connect to the S3 endpoint.
	ConnectTimeout time.Duration
	// ReadTimeout bounds waiting for the first response byte from S3.
	ReadTimeout time.Duration

	Logger *slog.Logger
}

// NewClient builds a credential client reading dir and decrypting with cipher.
// A nil dir or cipher makes every lookup fail with ErrCredentialsUnavailable.
func NewClient(dir DirectorySource, cipher *crypto.Cipher) *Client {
	return &Client{
		Dir:    dir,
		Pool:   objectstore.NewPool(),
		Cipher: cipher,
		Logger: slog.Default(),
	}
}

var (
	// ErrCredentialsUnavailable means the alias has no mirrored credentials, which
	// usually means the alias was created after the last successful poll.
	ErrCredentialsUnavailable = errors.New("alias credentials are not loaded")
	// ErrMasterKeyMissing means no master key was configured, so mirrored
	// ciphertext cannot be decrypted.
	ErrMasterKeyMissing = errors.New("master_key is required to decrypt alias credentials")
)

// Store returns an object store for the alias at the snapshot's credential
// version. Decryption happens here in memory; no I/O reaches the control plane.
func (c *Client) Store(_ context.Context, aliasID string, view snapshot.AliasView) (objectstore.ObjectStore, error) {
	creds, ok := c.lookup(aliasID, view.CredentialVer)
	if !ok {
		return nil, fmt.Errorf("%w: alias %s", ErrCredentialsUnavailable, aliasID)
	}
	secret, err := c.decrypt(creds)
	if err != nil {
		return nil, fmt.Errorf("alias %s: %w", aliasID, err)
	}
	return c.Pool.Get(aliasID, creds.CredentialVer, objectstore.Credentials{
		Endpoint:       view.Endpoint,
		Region:         view.Region,
		AccessKey:      creds.AccessKey,
		SecretKey:      secret,
		PathStyle:      view.PathStyle,
		ConnectTimeout: c.ConnectTimeout,
		ReadTimeout:    c.ReadTimeout,
	})
}

// decrypt opens the sealed secret. Caught here so a mismatched master key
// surfaces as one clear error instead of a storage signature failure.
func (c *Client) decrypt(creds snapshot.AliasCredentials) (string, error) {
	if c.Cipher == nil {
		return "", ErrMasterKeyMissing
	}
	secret, err := c.Cipher.DecryptString(creds.SecretCipher)
	if err != nil {
		return "", fmt.Errorf("decrypt secret: %w", err)
	}
	return secret, nil
}

// lookup returns the mirrored credentials for the alias. Credentials older than
// the snapshot's version are rejected: they belong to a superseded key pair.
func (c *Client) lookup(aliasID string, version int64) (snapshot.AliasCredentials, bool) {
	if c.Dir == nil {
		return snapshot.AliasCredentials{}, false
	}
	dir := c.Dir.Credentials()
	if dir == nil {
		return snapshot.AliasCredentials{}, false
	}
	creds, ok := dir.Aliases[aliasID]
	if !ok || creds.CredentialVer < version {
		return snapshot.AliasCredentials{}, false
	}
	return creds, true
}

// Healthy reports whether the alias can be served, so callers can log or gate
// without building an object store.
func (c *Client) Healthy(aliasID string, version int64) bool {
	_, ok := c.lookup(aliasID, version)
	return ok && c.Cipher != nil
}
