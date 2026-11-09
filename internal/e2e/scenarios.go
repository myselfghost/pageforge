package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pageforge/pageforge/internal/handler"
	"github.com/pageforge/pageforge/internal/manifest"
	"github.com/pageforge/pageforge/internal/objectstore"
	"github.com/pageforge/pageforge/internal/snapshot"
)

// ScenarioBasicHosting exercises the default try_files behaviour, the root
// index fallback, asset etags/cache-control and precompressed negotiation.
func ScenarioBasicHosting(t *testing.T, cp *ControlPlane) {
	aliasID := bootstrapAlias(t, cp)
	projectID := bootstrapProject(t, cp, aliasID, "site-"+newID(""), cp.Bucket)
	bootstrapBinding(t, cp, projectID, "basic.example.com", "/")
	bootstrapDeployment(t, cp, projectID, siteZip(false, map[string]string{
		"assets/app.js":    "console.log('app');",
		"assets/app.js.br": "BR-DATA",
		"docs/guide.html":  "<h1>guide</h1>",
	}))
	if err := waitForSnapshot(cp.InternalURL, "internal-secret", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	dataPlane := newDataPlane(t, cp, "basic.example.com")

	resp := dataPlane.do(t, http.MethodGet, "/", nil)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "<h1>hello</h1>") {
		t.Fatalf("root: status=%d body=%q", resp.Code, resp.Body.String())
	}

	resp = dataPlane.do(t, http.MethodGet, "/assets/app.js", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("asset status = %d", resp.Code)
	}
	if got := resp.Header().Get("ETag"); got == "" {
		t.Fatal("asset missing ETag")
	}
	if got := resp.Header().Get("Cache-Control"); got != "public, max-age=300" {
		t.Fatalf("Cache-Control = %q", got)
	}

	resp = dataPlane.do(t, http.MethodGet, "/assets/app.js", map[string]string{"Accept-Encoding": "br"})
	if resp.Code != http.StatusOK || resp.Header().Get("Content-Encoding") != "br" || resp.Body.String() != "BR-DATA" {
		t.Fatalf("br: status=%d enc=%q body=%q", resp.Code, resp.Header().Get("Content-Encoding"), resp.Body.String())
	}
}

func ScenarioSPAFallback(t *testing.T, cp *ControlPlane) {
	aliasID := bootstrapAlias(t, cp)
	projectID := bootstrapProject(t, cp, aliasID, "spa-"+newID(""), cp.Bucket,
		map[string]any{"spa": map[string]any{"enabled": true}})
	bootstrapBinding(t, cp, projectID, "spa.example.com", "/")
	bootstrapDeployment(t, cp, projectID, siteZip(true, map[string]string{
		"assets/app.js": "/* app */",
		"api/data.json": "{}",
	}))
	if err := waitForSnapshot(cp.InternalURL, "internal-secret", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	dataPlane := newDataPlane(t, cp, "spa.example.com")

	headers := map[string]string{"Accept": "text/html"}
	resp := dataPlane.do(t, http.MethodGet, "/dashboard/route", headers)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "<h1>hello</h1>") {
		t.Fatalf("spa fallback: status=%d body=%q", resp.Code, resp.Body.String())
	}

	// asset extension must not fall back
	resp = dataPlane.do(t, http.MethodGet, "/assets/app.js", headers)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "app") {
		t.Fatalf("asset: status=%d body=%q", resp.Code, resp.Body.String())
	}

	// excluded prefix /api must not fall back either
	resp = dataPlane.do(t, http.MethodGet, "/api/data.json", headers)
	if resp.Code != http.StatusOK {
		t.Fatalf("/api prefix fell back: status=%d", resp.Code)
	}
}

func ScenarioCustomErrorDoc(t *testing.T, cp *ControlPlane) {
	aliasID := bootstrapAlias(t, cp)
	projectID := bootstrapProject(t, cp, aliasID, "err-"+newID(""), cp.Bucket)
	bootstrapBinding(t, cp, projectID, "err.example.com", "/")
	bootstrapDeployment(t, cp, projectID, siteZip(false, nil))
	if err := waitForSnapshot(cp.InternalURL, "internal-secret", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	dataPlane := newDataPlane(t, cp, "err.example.com")

	resp := dataPlane.do(t, http.MethodGet, "/missing", nil)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.Code)
	}
	if !strings.Contains(resp.Body.String(), "not found") {
		t.Fatalf("error body missing custom doc: %q", resp.Body.String())
	}
}

func ScenarioTryFilesAndDirectories(t *testing.T, cp *ControlPlane) {
	aliasID := bootstrapAlias(t, cp)
	projectID := bootstrapProject(t, cp, aliasID, "docs-"+newID(""), cp.Bucket)
	bootstrapBinding(t, cp, projectID, "docs.example.com", "/")
	bootstrapDeployment(t, cp, projectID, siteZip(false, map[string]string{
		"docs/index.html":     "<h1>docs index</h1>",
		"docs/api/index.html": "<h1>api docs</h1>",
		"docs/api/ref.html":   "<h1>api ref</h1>",
	}))
	if err := waitForSnapshot(cp.InternalURL, "internal-secret", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	dataPlane := newDataPlane(t, cp, "docs.example.com")

	// directory without trailing slash → 301
	resp := dataPlane.do(t, http.MethodGet, "/docs", nil)
	if resp.Code != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 301", resp.Code)
	}
	if got := resp.Header().Get("Location"); got != "/docs/" {
		t.Fatalf("Location = %q", got)
	}

	resp = dataPlane.do(t, http.MethodGet, "/docs/", nil)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "docs index") {
		t.Fatalf("docs index: status=%d body=%q", resp.Code, resp.Body.String())
	}

	resp = dataPlane.do(t, http.MethodGet, "/docs/api/", nil)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "api docs") {
		t.Fatalf("docs/api/: status=%d body=%q", resp.Code, resp.Body.String())
	}

	resp = dataPlane.do(t, http.MethodGet, "/docs/api/ref.html", nil)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "api ref") {
		t.Fatalf("exact file: status=%d body=%q", resp.Code, resp.Body.String())
	}
}

func ScenarioSubpathBinding(t *testing.T, cp *ControlPlane) {
	aliasID := bootstrapAlias(t, cp)
	admin := newClient(cp.AdminURL, cp.AdminToken)
	proj1 := bootstrapProject(t, cp, aliasID, "app-"+newID(""), cp.Bucket)
	proj2 := bootstrapProject(t, cp, aliasID, "blog-"+newID(""), cp.Bucket)
	bootstrapBinding(t, cp, proj1, "multi.example.com", "/app")
	bootstrapBinding(t, cp, proj2, "multi.example.com", "/blog")
	bootstrapDeployment(t, cp, proj1, siteZip(false, map[string]string{
		"index.html":    "<h1>app root</h1>",
		"assets/app.js": "app()",
	}))
	bootstrapDeployment(t, cp, proj2, siteZip(false, map[string]string{
		"index.html":     "<h1>blog root</h1>",
		"posts/one.html": "<h1>one</h1>",
	}))
	if err := waitForSnapshot(cp.InternalURL, "internal-secret", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	dataPlane := newDataPlane(t, cp, "multi.example.com")

	resp := dataPlane.do(t, http.MethodGet, "/app/", nil)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "app root") {
		t.Fatalf("app root: status=%d body=%q", resp.Code, resp.Body.String())
	}
	resp = dataPlane.do(t, http.MethodGet, "/blog/posts/one.html", nil)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "one") {
		t.Fatalf("blog post: status=%d body=%q", resp.Code, resp.Body.String())
	}
	resp = dataPlane.do(t, http.MethodGet, "/app/assets/app.js", nil)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "app()") {
		t.Fatalf("subpath asset: status=%d body=%q", resp.Code, resp.Body.String())
	}

	status, body := admin.doJSON(t, http.MethodGet, "/healthz", nil)
	requireStatus(t, status, http.StatusOK, body)
}

func ScenarioPublishAndRollback(t *testing.T, cp *ControlPlane) {
	aliasID := bootstrapAlias(t, cp)
	projectID := bootstrapProject(t, cp, aliasID, "rollback-"+newID(""), cp.Bucket)
	bootstrapBinding(t, cp, projectID, "roll.example.com", "/")
	_, version1 := bootstrapDeployment(t, cp, projectID, siteZip(false, map[string]string{
		"index.html": "<h1>v1</h1>",
	}))
	if err := waitForSnapshot(cp.InternalURL, "internal-secret", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	dataPlane := newDataPlane(t, cp, "roll.example.com")

	resp := dataPlane.do(t, http.MethodGet, "/", nil)
	if !strings.Contains(resp.Body.String(), "v1") {
		t.Fatalf("before v2: %q", resp.Body.String())
	}

	_, version2 := bootstrapDeployment(t, cp, projectID, siteZip(false, map[string]string{
		"index.html": "<h1>v2</h1>",
	}))
	if err := waitForSnapshot(cp.InternalURL, "internal-secret", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	// Rebuild data plane so it picks up the new snapshot
	dataPlane = newDataPlane(t, cp, "roll.example.com")
	resp = dataPlane.do(t, http.MethodGet, "/", nil)
	if !strings.Contains(resp.Body.String(), "v2") {
		t.Fatalf("after v2: %q", resp.Body.String())
	}

	admin := newClient(cp.AdminURL, cp.AdminToken)
	status, body := admin.doJSON(t, http.MethodPost, "/api/v1/projects/"+projectID+"/pointer", map[string]any{
		"version_id": version1,
	})
	requireStatus(t, status, http.StatusOK, body)

	if err := waitForSnapshot(cp.InternalURL, "internal-secret", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	dataPlane = newDataPlane(t, cp, "roll.example.com")
	resp = dataPlane.do(t, http.MethodGet, "/", nil)
	if !strings.Contains(resp.Body.String(), "v1") {
		t.Fatalf("after rollback: %q", resp.Body.String())
	}
	_ = version2
}

func ScenarioRejectMaliciousPackages(t *testing.T, cp *ControlPlane) {
	aliasID := bootstrapAlias(t, cp)
	projectID := bootstrapProject(t, cp, aliasID, "security-"+newID(""), cp.Bucket)
	admin := newClient(cp.AdminURL, cp.AdminToken)

	status, body := admin.doJSON(t, http.MethodPost, "/api/v1/deployments", map[string]any{
		"project_id": projectID,
	})
	requireStatus(t, status, http.StatusCreated, body)
	var dep struct {
		DeploymentID string `json:"deployment_id"`
	}
	if err := json.Unmarshal(body, &dep); err != nil {
		t.Fatalf("decode: %v", err)
	}

	malicious := zipBuild(func(z *zipBuilder) {
		z.file("index.html", "<h1>ok</h1>")
		z.file("../../evil.html", "<h1>evil</h1>")
	})
	status, body = admin.upload(t, "/api/v1/deployments/"+dep.DeploymentID+"/package", malicious)
	requireStatus(t, status, http.StatusOK, body)

	status, body = admin.doJSON(t, http.MethodPost, "/api/v1/deployments/"+dep.DeploymentID+"/complete", nil)
	requireStatus(t, status, http.StatusBadRequest, body)
	requireErrorCode(t, body, "INVALID_PACKAGE")
}

func ScenarioRangeAndConditional(t *testing.T, cp *ControlPlane) {
	aliasID := bootstrapAlias(t, cp)
	projectID := bootstrapProject(t, cp, aliasID, "range-"+newID(""), cp.Bucket)
	bootstrapBinding(t, cp, projectID, "range.example.com", "/")
	const body = "0123456789"
	bootstrapDeployment(t, cp, projectID, siteZip(false, map[string]string{
		"big.txt": body,
	}))
	if err := waitForSnapshot(cp.InternalURL, "internal-secret", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	dataPlane := newDataPlane(t, cp, "range.example.com")

	resp := dataPlane.do(t, http.MethodGet, "/big.txt", map[string]string{"Range": "bytes=2-5"})
	if resp.Code != http.StatusPartialContent {
		t.Fatalf("Range: status = %d, want 206", resp.Code)
	}
	if resp.Body.String() != "2345" {
		t.Fatalf("Range body = %q", resp.Body.String())
	}
	if got := resp.Header().Get("Content-Range"); got != "bytes 2-5/10" {
		t.Fatalf("Content-Range = %q", got)
	}

	resp = dataPlane.do(t, http.MethodGet, "/big.txt", map[string]string{"Range": "bytes=99-"})
	if resp.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("bad Range status = %d, want 416", resp.Code)
	}

	etag := dataPlane.do(t, http.MethodHead, "/big.txt", nil).Header().Get("ETag")
	resp = dataPlane.do(t, http.MethodGet, "/big.txt", map[string]string{"If-None-Match": etag})
	if resp.Code != http.StatusNotModified {
		t.Fatalf("304: status = %d, want 304", resp.Code)
	}

	head := dataPlane.do(t, http.MethodHead, "/big.txt", nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD: status=%d len=%d", head.Code, head.Body.Len())
	}
}

func ScenarioReservedPrefix(t *testing.T, cp *ControlPlane) {
	aliasID := bootstrapAlias(t, cp)
	projectID := bootstrapProject(t, cp, aliasID, "pf-"+newID(""), cp.Bucket)
	bootstrapBinding(t, cp, projectID, "pf.example.com", "/")
	bootstrapDeployment(t, cp, projectID, siteZip(false, nil))
	if err := waitForSnapshot(cp.InternalURL, "internal-secret", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	dataPlane := newDataPlane(t, cp, "pf.example.com")

	for _, path := range []string{"/__pageforge/healthz", "/__pageforge/readyz"} {
		resp := dataPlane.do(t, http.MethodGet, path, nil)
		if resp.Code != http.StatusOK {
			t.Fatalf("%s status = %d", path, resp.Code)
		}
	}
}

// newDataPlane spins up a handler pointed at this control plane.
func newDataPlane(t *testing.T, cp *ControlPlane, host string) *dataPlane {
	t.Helper()
	snap := snapshotLoad(t, cp.InternalURL, "internal-secret")
	store := newStaticStore(snap)
	store.ReplaceAuth(authDirectoryLoad(t, cp.InternalURL, "internal-secret"))
	if host == "" {
		host = "basic.example.com"
	}
	opts := handler.Options{
		Snapshots:      store,
		Stores:         &staticProvider{store: cp.Store},
		Manifests:      manifest.NewReader(),
		ReservedPrefix: "/__pageforge",
		ControlURL:     cp.InternalURL,
		InternalToken:  "internal-secret",
	}
	h := handler.New(opts)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &dataPlane{URL: srv.URL, DefaultHost: host}
}

func authDirectoryLoad(t *testing.T, baseURL, token string) *snapshot.AuthDirectory {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, strings.TrimSuffix(baseURL, "/")+"/internal/v1/auth-policies", nil)
	if err != nil {
		t.Fatalf("build auth directory request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fetch auth directory: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("auth directory status = %d", resp.StatusCode)
	}
	var dir snapshot.AuthDirectory
	if err := json.NewDecoder(resp.Body).Decode(&dir); err != nil {
		t.Fatalf("decode auth directory: %v", err)
	}
	return &dir
}

type staticProvider struct {
	store objectstore.ObjectStore
}

func (s *staticProvider) Store(context.Context, string, snapshot.AliasView) (objectstore.ObjectStore, error) {
	return s.store, nil
}

func newStaticStore(snap *snapshot.Snapshot) *snapshot.Store {
	store := snapshot.NewStore()
	_ = store.Replace(snap)
	return store
}

type dataPlane struct {
	URL         string
	DefaultHost string
}

func (d *dataPlane) do(t *testing.T, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	host := d.DefaultHost
	req, err := http.NewRequest(method, d.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for k, v := range headers {
		if strings.EqualFold(k, "X-Override-Host") {
			host = v
			continue
		}
		req.Header.Set(k, v)
	}
	req.Host = host
	// Use a client that does NOT follow redirects so we can observe 301s
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("data plane %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	rec := httptest.NewRecorder()
	rec.Code = resp.StatusCode
	rec.Body.Write(body)
	for k, values := range resp.Header {
		for _, v := range values {
			rec.Header().Add(k, v)
		}
	}
	return rec
}

func ScenarioShareLinkAndVersionDirect(t *testing.T, cp *ControlPlane) {
	aliasID := bootstrapAlias(t, cp)
	projectID := bootstrapProject(t, cp, aliasID, "share-"+newID(""), cp.Bucket)
	bootstrapBinding(t, cp, projectID, "share.example.com", "/")
	_, version1 := bootstrapDeployment(t, cp, projectID, siteZip(false, map[string]string{
		"index.html": "<h1>v1</h1>",
	}))
	_, version2 := bootstrapDeployment(t, cp, projectID, siteZip(false, map[string]string{
		"index.html":    "<h1>v2</h1>",
		"assets/app.js": "app()",
	}))
	if err := waitForSnapshot(cp.InternalURL, "internal-secret", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	dataPlane := newDataPlane(t, cp, "share.example.com")

	// FR-19: version direct links are immutable and independent of the pointer.
	resp := dataPlane.do(t, http.MethodGet, "/__pageforge/v/"+projectID+"/"+version1+"/", nil)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "v1") {
		t.Fatalf("version direct v1: status=%d body=%q", resp.Code, resp.Body.String())
	}
	resp = dataPlane.do(t, http.MethodGet, "/__pageforge/v/"+projectID+"/"+version2+"/assets/app.js", nil)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "app()") {
		t.Fatalf("version direct asset: status=%d body=%q", resp.Code, resp.Body.String())
	}
	resp = dataPlane.do(t, http.MethodGet, "/__pageforge/v/"+projectID+"/ver_missing/index.html", nil)
	requireErrorCode(t, resp.Body.Bytes(), "VERSION_NOT_AVAILABLE")

	// FR-18: password-protected share link with a two-view budget.
	admin := newClient(cp.AdminURL, cp.AdminToken)
	status, body := admin.doJSON(t, http.MethodPost, "/api/v1/share-links", map[string]any{
		"project_id": projectID,
		"password":   "s3cret",
		"max_views":  2,
		"cache":      map[string]any{"html": "public, max-age=30"},
	})
	requireStatus(t, status, http.StatusCreated, body)
	var created struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode share link: %v", err)
	}

	resp = dataPlane.do(t, http.MethodGet, "/__pageforge/s/"+created.Token+"/", nil)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("share without password: status=%d", resp.Code)
	}
	if got := resp.Header().Get("WWW-Authenticate"); got == "" {
		t.Fatal("share challenge header is missing")
	}

	resp = dataPlane.do(t, http.MethodGet, "/__pageforge/s/"+created.Token+"/",
		map[string]string{"X-Share-Password": "s3cret"})
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "v2") {
		t.Fatalf("share root: status=%d body=%q", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Cache-Control"); got != "public, max-age=30" {
		t.Fatalf("share Cache-Control = %q", got)
	}

	resp = dataPlane.do(t, http.MethodGet, "/__pageforge/s/"+created.Token+"/assets/app.js",
		map[string]string{"X-Share-Password": "s3cret"})
	if resp.Code != http.StatusOK {
		t.Fatalf("share asset: status=%d", resp.Code)
	}

	resp = dataPlane.do(t, http.MethodGet, "/__pageforge/s/"+created.Token+"/",
		map[string]string{"X-Share-Password": "s3cret"})
	if resp.Code != http.StatusOK {
		t.Fatalf("share second view: status=%d", resp.Code)
	}
	resp = dataPlane.do(t, http.MethodGet, "/__pageforge/s/"+created.Token+"/",
		map[string]string{"X-Share-Password": "s3cret"})
	requireStatus(t, resp.Code, http.StatusForbidden, resp.Body.Bytes())
	requireErrorCode(t, resp.Body.Bytes(), "SHARE_VIEWS_EXCEEDED")

	resp = dataPlane.do(t, http.MethodGet, "/__pageforge/s/tok_missing/", nil)
	requireStatus(t, resp.Code, http.StatusNotFound, resp.Body.Bytes())
	requireErrorCode(t, resp.Body.Bytes(), "SHARE_LINK_NOT_FOUND")

	status, body = admin.doJSON(t, http.MethodDelete, "/api/v1/share-links/"+created.ID, nil)
	requireStatus(t, status, http.StatusNoContent, body)
	resp = dataPlane.do(t, http.MethodGet, "/__pageforge/s/"+created.Token+"/",
		map[string]string{"X-Share-Password": "s3cret"})
	requireStatus(t, resp.Code, http.StatusNotFound, resp.Body.Bytes())
}

func ScenarioOriginalPackageDownload(t *testing.T, cp *ControlPlane) {
	aliasID := bootstrapAlias(t, cp)
	projectID := bootstrapProject(t, cp, aliasID, "pkg-"+newID(""), cp.Bucket,
		map[string]any{"package_retention": true})
	packageBytes := siteZip(false, map[string]string{"index.html": "<h1>pkg</h1>"})
	_, versionID := bootstrapDeployment(t, cp, projectID, packageBytes)

	admin := newClient(cp.AdminURL, cp.AdminToken)
	resp, body := admin.doRaw(t, http.MethodGet,
		"/api/v1/projects/"+projectID+"/versions/"+versionID+"/package")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download package: status=%d body=%s", resp.StatusCode, string(body))
	}
	if len(body) == 0 {
		t.Fatal("download package returned an empty body")
	}
	if got := resp.Header.Get("Content-Disposition"); !strings.Contains(got, "attachment") {
		t.Fatalf("Content-Disposition = %q", got)
	}

	resp, body = admin.doRaw(t, http.MethodHead,
		"/api/v1/projects/"+projectID+"/versions/"+versionID+"/package")
	if resp.StatusCode != http.StatusOK || len(body) != 0 {
		t.Fatalf("HEAD package: status=%d len=%d", resp.StatusCode, len(body))
	}

	noRetention := bootstrapProject(t, cp, aliasID, "nopkg-"+newID(""), cp.Bucket,
		map[string]any{"package_retention": false})
	_, versionID = bootstrapDeployment(t, cp, noRetention, siteZip(false, nil))
	resp, body = admin.doRaw(t, http.MethodGet,
		"/api/v1/projects/"+noRetention+"/versions/"+versionID+"/package")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("package without retention: status=%d body=%s", resp.StatusCode, string(body))
	}
	requireErrorCode(t, body, "PACKAGE_NOT_AVAILABLE")
}

func ScenarioBindingBasicAuth(t *testing.T, cp *ControlPlane) {
	aliasID := bootstrapAlias(t, cp)
	projectID := bootstrapProject(t, cp, aliasID, "auth-"+newID(""), cp.Bucket)
	bootstrapDeployment(t, cp, projectID, siteZip(false, nil))
	bootstrapBinding(t, cp, projectID, "public.example.com", "/")

	admin := newClient(cp.AdminURL, cp.AdminToken)
	status, body := admin.doJSON(t, http.MethodPost, "/api/v1/auth-policies", map[string]any{
		"name": "internal-docs", "mode": "basic",
	})
	requireStatus(t, status, http.StatusCreated, body)
	var policy struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &policy); err != nil {
		t.Fatalf("decode policy: %v", err)
	}
	status, body = admin.doJSON(t, http.MethodPut,
		"/api/v1/auth-policies/"+policy.ID+"/users/alice", map[string]any{"password": "s3cret!"})
	requireStatus(t, status, http.StatusOK, body)

	status, body = admin.doJSON(t, http.MethodPost, "/api/v1/bindings", map[string]any{
		"host": "secure.example.com", "path_prefix": "/", "project_id": projectID,
		"auth_policy_id": policy.ID, "enabled": true,
	})
	requireStatus(t, status, http.StatusCreated, body)

	if err := waitForSnapshot(cp.InternalURL, "internal-secret", 5*time.Second); err != nil {
		t.Fatal(err)
	}

	// 快照只包含策略元数据，不得出现用户名或哈希。
	snapBody := internalGet(t, cp.InternalURL, "/internal/v1/snapshot", "internal-secret")
	if strings.Contains(snapBody, "alice") || strings.Contains(snapBody, "$2") {
		t.Fatalf("snapshot leaked auth material: %s", snapBody)
	}

	public := newDataPlane(t, cp, "public.example.com")
	if resp := public.do(t, http.MethodGet, "/", nil); resp.Code != http.StatusOK {
		t.Fatalf("public binding status = %d", resp.Code)
	}

	secure := newDataPlane(t, cp, "secure.example.com")
	resp := secure.do(t, http.MethodGet, "/", nil)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("missing credentials status = %d", resp.Code)
	}
	if got := resp.Header().Get("WWW-Authenticate"); got != `Basic realm="internal-docs"` {
		t.Fatalf("WWW-Authenticate = %q", got)
	}

	if got := secure.do(t, http.MethodGet, "/", map[string]string{
		"Authorization": basicAuthHeader("alice", "wrong"),
	}).Code; got != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d", got)
	}

	ok := secure.do(t, http.MethodGet, "/", map[string]string{
		"Authorization": basicAuthHeader("alice", "s3cret!"),
	})
	if ok.Code != http.StatusOK || !strings.Contains(ok.Body.String(), "<h1>hello</h1>") {
		t.Fatalf("authorized status=%d body=%q", ok.Code, ok.Body.String())
	}

	// 仍有绑定引用时不允许删除策略。
	status, body = admin.doJSON(t, http.MethodDelete, "/api/v1/auth-policies/"+policy.ID, nil)
	requireStatus(t, status, http.StatusConflict, body)
	requireErrorCode(t, body, "AUTH_POLICY_IN_USE")

	// 删除用户后，重建数据面（等价于下一次轮询）应拒绝旧凭证。
	status, body = admin.doJSON(t, http.MethodDelete,
		"/api/v1/auth-policies/"+policy.ID+"/users/alice", nil)
	requireStatus(t, status, http.StatusNoContent, body)
	secureAfter := newDataPlane(t, cp, "secure.example.com")
	if got := secureAfter.do(t, http.MethodGet, "/", map[string]string{
		"Authorization": basicAuthHeader("alice", "s3cret!"),
	}).Code; got != http.StatusUnauthorized {
		t.Fatalf("deleted user status = %d", got)
	}
}

func basicAuthHeader(username, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
}

func internalGet(t *testing.T, baseURL, path, token string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, strings.TrimSuffix(baseURL, "/")+path, nil)
	if err != nil {
		t.Fatalf("build internal request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("internal request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("internal request status = %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read internal body: %v", err)
	}
	return string(raw)
}

func ScenarioResponseHeaders(t *testing.T, cp *ControlPlane) {
	aliasID := bootstrapAlias(t, cp)
	projectID := bootstrapProject(t, cp, aliasID, "hdr-"+newID(""), cp.Bucket, map[string]any{
		"cache": map[string]any{"html": "no-cache", "assets": "public, max-age=60"},
		"response_headers": []map[string]any{
			{"name": "X-Project", "value": "$project_id"},
			{"name": "Cache-Control", "value": "public, max-age=42"},
		},
	})
	bootstrapBinding(t, cp, projectID, "hdr.example.com", "/")
	_, versionID := bootstrapDeployment(t, cp, projectID, siteZip(false, map[string]string{
		"index.html":    "<h1>hdr</h1>",
		"assets/app.js": "app()",
	}))
	if err := waitForSnapshot(cp.InternalURL, "internal-secret", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	dataPlane := newDataPlane(t, cp, "hdr.example.com")

	resp := dataPlane.do(t, http.MethodGet, "/", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("root: status=%d", resp.Code)
	}
	if got := resp.Header().Get("X-Project"); got != projectID {
		t.Fatalf("X-Project = %q, want %q", got, projectID)
	}
	if got := resp.Header().Get("Cache-Control"); got != "public, max-age=42" {
		t.Fatalf("Cache-Control = %q, want project rule override", got)
	}

	resp = dataPlane.do(t, http.MethodGet, "/missing", nil)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("404: status=%d", resp.Code)
	}
	if got := resp.Header().Get("X-Project"); got != projectID {
		t.Fatalf("404 X-Project = %q", got)
	}
	_ = versionID
}

// Scenarios returns the full ordered list of core scenarios.
func Scenarios() []Scenario {
	return []Scenario{
		{Name: "basic-hosting", Run: ScenarioBasicHosting},
		{Name: "spa-fallback", Run: ScenarioSPAFallback},
		{Name: "custom-error-doc", Run: ScenarioCustomErrorDoc},
		{Name: "try-files-and-directories", Run: ScenarioTryFilesAndDirectories},
		{Name: "subpath-binding", Run: ScenarioSubpathBinding},
		{Name: "publish-and-rollback", Run: ScenarioPublishAndRollback},
		{Name: "reject-malicious-packages", Run: ScenarioRejectMaliciousPackages},
		{Name: "range-and-conditional", Run: ScenarioRangeAndConditional},
		{Name: "reserved-prefix", Run: ScenarioReservedPrefix},
		{Name: "share-link-and-version-direct", Run: ScenarioShareLinkAndVersionDirect},
		{Name: "binding-basic-auth", Run: ScenarioBindingBasicAuth},
		{Name: "original-package-download", Run: ScenarioOriginalPackageDownload},
		{Name: "response-headers", Run: ScenarioResponseHeaders},
	}
}
