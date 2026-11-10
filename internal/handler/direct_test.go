package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/pageforge/pageforge/internal/handler"
	"github.com/pageforge/pageforge/internal/model"
	"github.com/pageforge/pageforge/internal/snapshot"
)

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode error body %q: %v", string(body), err)
	}
	return envelope.Error.Code
}

type fakeShareAuth struct {
	grant     handler.ShareGrant
	err       error
	token     string
	password  string
	countView bool
	calls     int
}

func (f *fakeShareAuth) Authorize(_ context.Context, token, password string, countView bool) (handler.ShareGrant, error) {
	f.calls++
	f.token = token
	f.password = password
	f.countView = countView
	return f.grant, f.err
}

func TestVersionDirectServesVersion(t *testing.T) {
	f := newFixture(t, nil)

	path := "/__pageforge/v/" + testPID + "/" + testVID + "/"
	if rec := f.do(http.MethodGet, path, nil); rec.Code != http.StatusOK || rec.Body.String() != "<h1>index</h1>" {
		t.Fatalf("direct root = %d %q", rec.Code, rec.Body.String())
	}

	path = "/__pageforge/v/" + testPID + "/" + testVID + "/assets/app.js"
	if rec := f.do(http.MethodGet, path, nil); rec.Code != http.StatusOK || rec.Body.String() != "console.log('app');" {
		t.Fatalf("direct asset = %d %q", rec.Code, rec.Body.String())
	}

	rec := f.do(http.MethodGet, "/__pageforge/v/"+testPID+"/ver_missing/index.html", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing version = %d, want 404", rec.Code)
	}
	if code := errorCode(t, rec.Body.Bytes()); code != "VERSION_NOT_AVAILABLE" {
		t.Fatalf("missing version code = %q", code)
	}

	rec = f.do(http.MethodGet, "/__pageforge/v/prj_missing/"+testVID+"/index.html", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing project = %d, want 404", rec.Code)
	}
	if code := errorCode(t, rec.Body.Bytes()); code != "PROJECT_NOT_FOUND" {
		t.Fatalf("missing project code = %q", code)
	}
}

func TestVersionDirectAppliesProjectHeaders(t *testing.T) {
	f := newFixture(t, func(snap *snapshot.Snapshot) {
		project := snap.Projects[testPID]
		project.ResponseHeaders = []model.HeaderRule{{Name: "X-Project", Value: "$project_id"}}
		snap.Projects[testPID] = project
	})

	rec := f.do(http.MethodGet, "/__pageforge/v/"+testPID+"/"+testVID+"/", nil)
	if got := rec.Header().Get("X-Project"); got != testPID {
		t.Fatalf("X-Project = %q, want %q", got, testPID)
	}
}

func TestShareLinkAuthorizedServing(t *testing.T) {
	auth := &fakeShareAuth{grant: handler.ShareGrant{
		ProjectID: testPID,
		Cache:     model.CachePolicy{HTML: "public, max-age=60"},
	}}
	f := newFixtureWithOptions(t, nil, func(opts *handler.Options) {
		opts.ShareAuth = auth
	})

	rec := f.do(http.MethodGet, "/__pageforge/s/tok_1/", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "<h1>index</h1>" {
		t.Fatalf("share root = %d %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Fatalf("Cache-Control = %q, want share policy", got)
	}
	if auth.token != "tok_1" || !auth.countView {
		t.Fatalf("authorize call = token %q countView %v", auth.token, auth.countView)
	}

	auth.calls = 0
	rec = f.do(http.MethodGet, "/__pageforge/s/tok_1/assets/app.js", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("share asset = %d", rec.Code)
	}
	if auth.countView {
		t.Fatal("asset request must not count as a view")
	}
}

func TestShareLinkPasswordAndErrors(t *testing.T) {
	auth := &fakeShareAuth{grant: handler.ShareGrant{ProjectID: testPID}}
	f := newFixtureWithOptions(t, nil, func(opts *handler.Options) {
		opts.ShareAuth = auth
	})

	auth.err = &handler.ShareAuthError{Status: http.StatusUnauthorized,
		Code: "SHARE_PASSWORD_REQUIRED", Message: "share link requires a password"}
	rec := f.do(http.MethodGet, "/__pageforge/s/tok_1/", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got == "" {
		t.Fatal("WWW-Authenticate header is missing")
	}

	auth.err = &handler.ShareAuthError{Status: http.StatusGone, Code: "SHARE_EXPIRED", Message: "expired"}
	rec = f.do(http.MethodGet, "/__pageforge/s/tok_1/", nil)
	if rec.Code != http.StatusGone || errorCode(t, rec.Body.Bytes()) != "SHARE_EXPIRED" {
		t.Fatalf("expired = %d %q", rec.Code, rec.Body.String())
	}

	auth.err = nil
	auth.grant = handler.ShareGrant{ProjectID: "prj_missing"}
	rec = f.do(http.MethodGet, "/__pageforge/s/tok_1/", nil)
	if rec.Code != http.StatusNotFound || errorCode(t, rec.Body.Bytes()) != "PROJECT_NOT_FOUND" {
		t.Fatalf("missing project = %d %q", rec.Code, rec.Body.String())
	}
}

func TestSharePasswordFormAndCookie(t *testing.T) {
	auth := &fakeShareAuth{grant: handler.ShareGrant{ProjectID: testPID}}
	f := newFixtureWithOptions(t, nil, func(opts *handler.Options) {
		opts.ShareAuth = auth
	})

	auth.err = &handler.ShareAuthError{Status: http.StatusUnauthorized,
		Code: "SHARE_PASSWORD_REQUIRED", Message: "share link requires a password"}
	rec := f.do(http.MethodGet, "/__pageforge/s/tok_1/", map[string]string{"Accept": "text/html"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `name="password"`) || strings.Contains(body, `name="username"`) {
		t.Fatalf("expected password-only form, got %q", body)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != "" {
		t.Fatalf("WWW-Authenticate = %q, want empty for HTML navigation", got)
	}

	auth.err = nil
	form := url.Values{"password": {"secret"}}
	req := httptest.NewRequest(http.MethodPost, "http://"+testHost+"/__pageforge/s/tok_1/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html")
	rec2 := httptest.NewRecorder()
	f.handler.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusSeeOther {
		t.Fatalf("POST status = %d, want 303", rec2.Code)
	}
	if loc := rec2.Header().Get("Location"); loc != "/__pageforge/s/tok_1/" {
		t.Fatalf("Location = %q", loc)
	}
	if auth.password != "secret" {
		t.Fatalf("authorize password = %q", auth.password)
	}
	cookies := rec2.Result().Cookies()
	if len(cookies) == 0 || cookies[0].Name != "pf_share_pw" {
		t.Fatalf("cookies = %v", cookies)
	}
	if want := "/__pageforge/s/tok_1/"; cookies[0].Path != want {
		t.Fatalf("cookie path = %q, want %q", cookies[0].Path, want)
	}

	rec3 := f.do(http.MethodGet, "/__pageforge/s/tok_1/assets/app.js", map[string]string{
		"Cookie": cookies[0].Name + "=" + cookies[0].Value,
	})
	if rec3.Code != http.StatusOK {
		t.Fatalf("cookie request status = %d", rec3.Code)
	}
	if auth.password != "secret" {
		t.Fatalf("authorize password from cookie = %q", auth.password)
	}
	if len(rec3.Result().Cookies()) != 0 {
		t.Fatal("cookie should not be set again when reusing the session")
	}
}

func TestShareLinkPassesPasswordHeader(t *testing.T) {
	auth := &fakeShareAuth{grant: handler.ShareGrant{ProjectID: testPID}}
	f := newFixtureWithOptions(t, nil, func(opts *handler.Options) {
		opts.ShareAuth = auth
	})
	rec := f.do(http.MethodGet, "/__pageforge/s/tok_1/", map[string]string{"X-Share-Password": "s3cret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if auth.password != "s3cret" {
		t.Fatalf("password = %q", auth.password)
	}
}

func TestShareLinkWithoutAuthorizer(t *testing.T) {
	f := newFixture(t, nil)
	rec := f.do(http.MethodGet, "/__pageforge/s/tok_1/", nil)
	// The local authorizer is configured but the share directory has not been
	// polled yet, so the link cannot be validated.
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// 分享链接在本地目录可用时必须离线可用：control 停机也要能继续服务。
func TestShareLinkServesOfflineFromDirectory(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("s3cret"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	expired := time.Now().Add(-time.Hour).UTC()
	f := newFixtureWithOptions(t, nil, func(opts *handler.Options) {
		// No ControlURL: the authorizer must work from the mirrored directory.
		opts.ControlURL = ""
		opts.Snapshots.ReplaceShares(&snapshot.ShareDirectory{Links: map[string]snapshot.ShareLinkSecrets{
			"tok_open":   {ID: "shr_open", ProjectID: testPID, Enabled: true},
			"tok_locked": {ID: "shr_locked", ProjectID: testPID, Enabled: true, PasswordHash: string(hash)},
			"tok_off":    {ID: "shr_off", ProjectID: testPID, Enabled: false},
			"tok_exp":    {ID: "shr_exp", ProjectID: testPID, Enabled: true, ExpiresAt: &expired},
			"tok_full":   {ID: "shr_full", ProjectID: testPID, Enabled: true, MaxViews: 3, Views: 3},
		}})
	})

	// An open link serves the project with no control-plane call at all.
	rec := f.do(http.MethodGet, "/__pageforge/s/tok_open/", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "<h1>index</h1>" {
		t.Fatalf("open share = %d %q", rec.Code, rec.Body.String())
	}

	// A password-protected link challenges, then serves with the right password.
	if rec := f.do(http.MethodGet, "/__pageforge/s/tok_locked/", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("locked share without password = %d", rec.Code)
	}
	if rec := f.do(http.MethodGet, "/__pageforge/s/tok_locked/",
		map[string]string{"X-Share-Password": "s3cret"}); rec.Code != http.StatusOK {
		t.Fatalf("locked share with password = %d", rec.Code)
	}

	// Disabled, expired and over-budget links keep their control-plane codes.
	if rec := f.do(http.MethodGet, "/__pageforge/s/tok_off/", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("disabled share = %d, want 403", rec.Code)
	}
	if rec := f.do(http.MethodGet, "/__pageforge/s/tok_exp/", nil); rec.Code != http.StatusGone {
		t.Fatalf("expired share = %d, want 410", rec.Code)
	}
	if rec := f.do(http.MethodGet, "/__pageforge/s/tok_full/", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("exhausted share = %d, want 403", rec.Code)
	}
	if rec := f.do(http.MethodGet, "/__pageforge/s/tok_missing/", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown share = %d, want 404", rec.Code)
	}
}
