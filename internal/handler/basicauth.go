package handler

import (
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/pageforge/pageforge/internal/snapshot"
)

const (
	basicAuthCacheTTL = 5 * time.Minute
	basicAuthCacheMax = 2048
)

// dummyHash makes unknown-user checks take the same bcrypt path as known ones.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("pageforge-dummy-password"), bcrypt.MinCost)

// authorizeBinding enforces URL-level Basic Auth (FR-08). A binding without an
// auth policy is public. It returns false after writing an error response.
func (h *Handler) authorizeBinding(w http.ResponseWriter, r *http.Request, snap *snapshot.Snapshot, binding snapshot.BindingView) bool {
	policyID := binding.AuthPolicyID
	if policyID == "" {
		return true
	}
	policy, ok := snap.AuthPolicies[policyID]
	if !ok {
		writeAuthUnavailable(w)
		return false
	}
	if policy.Mode != "basic" {
		writeAuthUnavailable(w)
		return false
	}
	dir := h.snapshots.Auth()
	if dir == nil {
		writeAuthUnavailable(w)
		return false
	}
	secrets, ok := dir.Policies[policyID]
	if !ok || secrets.SecretVersion != policy.SecretVersion {
		writeAuthUnavailable(w)
		return false
	}

	realm := policy.Realm
	if realm == "" {
		realm = secrets.Realm
	}
	if realm == "" {
		realm = "PageForge"
	}

	header := r.Header.Get("Authorization")
	username, password, ok := parseBasicCredentials(header)
	if !ok {
		writeBasicChallenge(w, realm)
		return false
	}
	key := policyID + "\x00" + fmt.Sprint(policy.SecretVersion) + "\x00" + header
	if h.authCache.allowed(key, time.Now()) {
		return true
	}

	matchedHash := dummyHash
	matched := false
	for name, hash := range secrets.Users {
		if subtle.ConstantTimeCompare([]byte(name), []byte(username)) == 1 {
			matchedHash = []byte(hash)
			matched = true
		}
	}
	if !matched || bcrypt.CompareHashAndPassword(matchedHash, []byte(password)) != nil {
		writeBasicChallenge(w, realm)
		return false
	}
	h.authCache.allow(key, time.Now())
	return true
}

func parseBasicCredentials(header string) (username, password string, ok bool) {
	if len(header) < 6 || !strings.EqualFold(header[:6], "basic ") {
		return "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header[6:]))
	if err != nil {
		return "", "", false
	}
	i := strings.IndexByte(string(decoded), ':')
	if i < 0 {
		return "", "", false
	}
	return string(decoded[:i]), string(decoded[i+1:]), true
}

func writeBasicChallenge(w http.ResponseWriter, realm string) {
	w.Header().Set("WWW-Authenticate", `Basic realm="`+sanitizeRealm(realm)+`"`)
	w.Header().Set("Cache-Control", "no-store")
	writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "basic authentication required")
}

func writeAuthUnavailable(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	writeError(w, http.StatusServiceUnavailable, "AUTH_UNAVAILABLE", "authentication data is not loaded")
}

// sanitizeRealm keeps the challenge header valid and free of header injection.
func sanitizeRealm(realm string) string {
	realm = strings.Map(func(r rune) rune {
		switch r {
		case '"', '\\':
			return -1
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, realm)
	if realm == "" {
		return "PageForge"
	}
	return realm
}

// basicAuthCache short-circuits bcrypt for repeated successful credentials.
type basicAuthCache struct {
	mu      sync.Mutex
	entries map[string]time.Time
}

func newBasicAuthCache() *basicAuthCache {
	return &basicAuthCache{entries: map[string]time.Time{}}
}

func (c *basicAuthCache) allowed(key string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, ok := c.entries[key]
	if !ok || now.After(exp) {
		if ok {
			delete(c.entries, key)
		}
		return false
	}
	return true
}

func (c *basicAuthCache) allow(key string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= basicAuthCacheMax {
		for k, exp := range c.entries {
			if now.After(exp) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= basicAuthCacheMax {
			c.entries = map[string]time.Time{}
		}
	}
	c.entries[key] = now.Add(basicAuthCacheTTL)
}
