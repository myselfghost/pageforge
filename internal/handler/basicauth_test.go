package handler_test

import (
	"encoding/base64"
	"net/http"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/pageforge/pageforge/internal/snapshot"
)

func basicHeader(username, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
}

func TestBindingBasicAuth(t *testing.T) {
	f := newFixture(t, func(s *snapshot.Snapshot) {
		s.Bindings[0].AuthPolicyID = "pol_1"
		s.AuthPolicies["pol_1"] = snapshot.AuthPolicyView{
			ID: "pol_1", Realm: "internal", Mode: "basic", SecretVersion: 7,
		}
	})
	hash, err := bcrypt.GenerateFromPassword([]byte("s3cret!"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	f.snapshots.ReplaceAuth(&snapshot.AuthDirectory{Policies: map[string]snapshot.AuthPolicySecrets{
		"pol_1": {Realm: "internal", Mode: "basic", SecretVersion: 7,
			Users: map[string]string{"alice": string(hash)}},
	}})

	resp := f.do(http.MethodGet, "/assets/app.js", nil)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("missing credentials status = %d", resp.Code)
	}
	if got := resp.Header().Get("WWW-Authenticate"); got != `Basic realm="internal"` {
		t.Fatalf("WWW-Authenticate = %q", got)
	}
	if got := resp.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}

	if got := f.do(http.MethodGet, "/assets/app.js", map[string]string{
		"Authorization": basicHeader("alice", "wrong"),
	}).Code; got != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d", got)
	}
	if got := f.do(http.MethodGet, "/assets/app.js", map[string]string{
		"Authorization": basicHeader("bob", "s3cret!"),
	}).Code; got != http.StatusUnauthorized {
		t.Fatalf("unknown user status = %d", got)
	}

	ok := f.do(http.MethodGet, "/assets/app.js", map[string]string{
		"Authorization": basicHeader("alice", "s3cret!"),
	})
	if ok.Code != http.StatusOK || ok.Body.String() != "console.log('app');" {
		t.Fatalf("authorized status=%d body=%q", ok.Code, ok.Body.String())
	}
	// 成功凭证命中缓存，重复请求仍然放行。
	if got := f.do(http.MethodHead, "/assets/app.js", map[string]string{
		"Authorization": basicHeader("alice", "s3cret!"),
	}).Code; got != http.StatusOK {
		t.Fatalf("cached credentials status = %d", got)
	}
}

func TestBindingBasicAuthUnavailable(t *testing.T) {
	f := newFixture(t, func(s *snapshot.Snapshot) {
		s.Bindings[0].AuthPolicyID = "pol_1"
		s.AuthPolicies["pol_1"] = snapshot.AuthPolicyView{
			ID: "pol_1", Realm: "internal", Mode: "basic", SecretVersion: 7,
		}
	})
	// 尚未拉取密码目录：失败关闭，不提供受保护内容。
	resp := f.do(http.MethodGet, "/assets/app.js", map[string]string{
		"Authorization": basicHeader("alice", "s3cret!"),
	})
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.Code)
	}

	// Secret 版本不一致同样视为未就绪。
	f.snapshots.ReplaceAuth(&snapshot.AuthDirectory{Policies: map[string]snapshot.AuthPolicySecrets{
		"pol_1": {SecretVersion: 6, Users: map[string]string{}},
	}})
	resp = f.do(http.MethodGet, "/assets/app.js", map[string]string{
		"Authorization": basicHeader("alice", "s3cret!"),
	})
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("stale version status = %d, want 503", resp.Code)
	}
}

func TestPublicBindingNeedsNoAuth(t *testing.T) {
	f := newFixture(t, nil)
	if resp := f.do(http.MethodGet, "/assets/app.js", nil); resp.Code != http.StatusOK {
		t.Fatalf("public binding status = %d", resp.Code)
	}
}
