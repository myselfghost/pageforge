package handler_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pageforge/pageforge/internal/cache"
	"github.com/pageforge/pageforge/internal/deploy"
	"github.com/pageforge/pageforge/internal/handler"
	"github.com/pageforge/pageforge/internal/manifest"
	"github.com/pageforge/pageforge/internal/model"
	"github.com/pageforge/pageforge/internal/objectstore"
	"github.com/pageforge/pageforge/internal/snapshot"
	"github.com/pageforge/pageforge/internal/testutil"
)

const (
	testBucket = "bucket-a"
	testHost   = "site.example.com"
	testPID    = "prj_1"
	testVID    = "ver_1"
)

type fakeProvider struct {
	store objectstore.ObjectStore
}

func (p *fakeProvider) Store(context.Context, string, snapshot.AliasView) (objectstore.ObjectStore, error) {
	return p.store, nil
}

type countingStore struct {
	objectstore.ObjectStore
	gets int32
}

func (c *countingStore) Get(ctx context.Context, bucket, key string, opt objectstore.GetOptions) (io.ReadCloser, objectstore.ObjectInfo, error) {
	if strings.HasPrefix(key, "projects/") {
		atomic.AddInt32(&c.gets, 1)
	}
	return c.ObjectStore.Get(ctx, bucket, key, opt)
}

type fixture struct {
	t         *testing.T
	store     *testutil.MemoryStore
	wrapped   *countingStore
	snapshots *snapshot.Store
	snap      *snapshot.Snapshot
	handler   *handler.Handler
}

func fixtureOptions(t *testing.T) handler.Options {
	return handler.Options{
		ReservedPrefix:      "/__pageforge",
		ClientIdleTimeout:   5 * time.Second,
		ClientTotalTimeout:  5 * time.Second,
		UpstreamIdleTimeout: 5 * time.Second,
	}
}

func newFixture(t *testing.T, mutate func(*snapshot.Snapshot)) *fixture {
	t.Helper()
	return newFixtureWithOptions(t, mutate, nil)
}

func newFixtureWithOptions(t *testing.T, mutate func(*snapshot.Snapshot), optsFn func(*handler.Options)) *fixture {
	t.Helper()
	memory := testutil.NewMemoryStore()
	files := map[string]string{
		"index.html":       "<h1>index</h1>",
		"404.html":         "<h1>missing</h1>",
		"assets/app.js":    "console.log('app');",
		"assets/app.js.br": "BR-DATA",
		"assets/app.js.gz": "GZ-DATA",
		"docs/index.html":  "<h1>docs</h1>",
		"docs/guide.html":  "<h1>guide</h1>",
		"big.txt":          "0123456789",
	}
	builder, err := manifest.NewBuilder("_system/manifests/"+testPID+"/"+testVID, manifest.Limits{}, t.TempDir())
	if err != nil {
		t.Fatalf("new manifest builder: %v", err)
	}
	defer func() { _ = builder.Close() }()

	ctx := context.Background()
	for name, body := range files {
		entry := manifest.Entry{
			Key:         name,
			Size:        int64(len(body)),
			SHA256:      digest(body),
			ContentType: contentType(name),
			ModTime:     1700000000,
		}
		if strings.HasSuffix(name, ".br") {
			entry.Encoding = "br"
			entry.ContentType = "text/javascript"
		}
		if strings.HasSuffix(name, ".gz") {
			entry.Encoding = "gz"
			entry.ContentType = "text/javascript"
		}
		if err := builder.Add(entry); err != nil {
			t.Fatalf("add manifest entry: %v", err)
		}
		if err := memory.Put(ctx, testBucket, "projects/"+testPID+"/"+testVID+"/"+name,
			strings.NewReader(body), int64(len(body)), objectstore.PutOptions{ContentType: entry.ContentType}); err != nil {
			t.Fatalf("put object: %v", err)
		}
	}
	if _, err := builder.Build(ctx, memory, testBucket, manifest.RootOptions{
		ProjectID: testPID,
		VersionID: testVID,
		IndexDoc:  "index.html",
		ErrorDoc:  "404.html",
	}); err != nil {
		t.Fatalf("build manifest: %v", err)
	}

	snap := &snapshot.Snapshot{
		Generation: 1,
		Aliases: map[string]snapshot.AliasView{
			"ali_1": {Endpoint: "s3.example.com", Region: "us-east-1", PathStyle: true,
				BucketAllow: []string{testBucket}, CredentialVer: 1},
		},
		Projects: map[string]snapshot.ProjectView{
			testPID: {
				AliasID:          "ali_1",
				Bucket:           testBucket,
				CurrentVersionID: testVID,
				IndexDoc:         "index.html",
				ErrorDoc:         "404.html",
				TryFilesEnabled:  true,
			},
		},
		Bindings: []snapshot.BindingView{
			{ID: "bnd_1", Host: testHost, PathPrefix: "/", ProjectID: testPID, Enabled: true},
		},
		AuthPolicies: map[string]snapshot.AuthPolicyView{},
	}
	if mutate != nil {
		mutate(snap)
	}

	snapshots := snapshot.NewStore()
	if err := snapshots.Replace(snap); err != nil {
		t.Fatalf("activate snapshot: %v", err)
	}
	wrapped := &countingStore{ObjectStore: memory}
	opts := fixtureOptions(t)
	opts.Snapshots = snapshots
	opts.Stores = &fakeProvider{store: wrapped}
	opts.Manifests = manifest.NewReader()
	if optsFn != nil {
		optsFn(&opts)
	}

	return &fixture{t: t, store: memory, wrapped: wrapped, snapshots: snapshots,
		snap: snap, handler: handler.New(opts)}
}

func (f *fixture) do(method, target string, headers map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, "http://"+testHost+target, nil)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) update(fn func(*snapshot.Snapshot)) {
	f.t.Helper()
	next := &snapshot.Snapshot{
		Generation:   f.snap.Generation + 1,
		PublishedAt:  time.Now().UTC(),
		Aliases:      f.snap.Aliases,
		Projects:     map[string]snapshot.ProjectView{},
		Bindings:     append([]snapshot.BindingView(nil), f.snap.Bindings...),
		AuthPolicies: f.snap.AuthPolicies,
	}
	for id, project := range f.snap.Projects {
		next.Projects[id] = project
	}
	fn(next)
	f.snap = next
	if err := f.snapshots.Replace(next); err != nil {
		f.t.Fatalf("replace snapshot: %v", err)
	}
}

func digest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func contentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "text/javascript"
	case strings.HasSuffix(name, ".css"):
		return "text/css"
	default:
		return "text/plain; charset=utf-8"
	}
}

func TestServeFileHeadersAndMethods(t *testing.T) {
	f := newFixture(t, nil)

	resp := f.do(http.MethodGet, "/assets/app.js", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d", resp.Code)
	}
	if resp.Body.String() != "console.log('app');" {
		t.Fatalf("body = %q", resp.Body.String())
	}
	wantETag := `"` + digest("console.log('app');") + `"`
	if got := resp.Header().Get("ETag"); got != wantETag {
		t.Fatalf("ETag = %q, want %q", got, wantETag)
	}
	if got := resp.Header().Get("Content-Type"); got != "text/javascript" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := resp.Header().Get("Cache-Control"); got != "public, max-age=300" {
		t.Fatalf("Cache-Control = %q", got)
	}

	head := f.do(http.MethodHead, "/assets/app.js", nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD status=%d body=%q", head.Code, head.Body.String())
	}
	if head.Header().Get("Content-Length") != "19" {
		t.Fatalf("HEAD Content-Length = %q", head.Header().Get("Content-Length"))
	}

	post := f.do(http.MethodPost, "/assets/app.js", nil)
	if post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", post.Code)
	}
	if post.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("Allow = %q", post.Header().Get("Allow"))
	}
}

func TestRootFallsBackToIndex(t *testing.T) {
	f := newFixture(t, nil)
	resp := f.do(http.MethodGet, "/", nil)
	if resp.Code != http.StatusOK || resp.Body.String() != "<h1>index</h1>" {
		t.Fatalf("status=%d body=%q", resp.Code, resp.Body.String())
	}

	f.update(func(s *snapshot.Snapshot) {
		p := s.Projects[testPID]
		p.TryFilesEnabled = false
		s.Projects[testPID] = p
	})
	if resp := f.do(http.MethodGet, "/", nil); resp.Code != http.StatusOK {
		t.Fatalf("exact root status = %d", resp.Code)
	}
	resp = f.do(http.MethodGet, "/no-such-file", nil)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("exact miss status = %d", resp.Code)
	}
}

func TestBindingRootAddsTrailingSlash(t *testing.T) {
	f := newFixture(t, func(s *snapshot.Snapshot) {
		s.Bindings = append(s.Bindings, snapshot.BindingView{
			ID: "bnd_app", Host: testHost, PathPrefix: "/app", ProjectID: testPID, Enabled: true,
		})
	})

	resp := f.do(http.MethodGet, "/app?x=1", nil)
	if resp.Code != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 301", resp.Code)
	}
	if got := resp.Header().Get("Location"); got != "/app/?x=1" {
		t.Fatalf("Location = %q", got)
	}

	resp = f.do(http.MethodGet, "/app/", nil)
	if resp.Code != http.StatusOK || resp.Body.String() != "<h1>index</h1>" {
		t.Fatalf("status=%d body=%q", resp.Code, resp.Body.String())
	}

	if resp := f.do(http.MethodGet, "/app/assets/app.js", nil); resp.Code != http.StatusOK {
		t.Fatalf("subpath asset status = %d", resp.Code)
	}
	if resp := f.do(http.MethodGet, "/", nil); resp.Code != http.StatusOK {
		t.Fatalf("root binding status = %d", resp.Code)
	}

	// try_files 关闭时同样补尾斜杠。
	f.update(func(s *snapshot.Snapshot) {
		p := s.Projects[testPID]
		p.TryFilesEnabled = false
		s.Projects[testPID] = p
	})
	if resp := f.do(http.MethodGet, "/app", nil); resp.Code != http.StatusMovedPermanently {
		t.Fatalf("exact mode status = %d, want 301", resp.Code)
	}
}

func TestTryFilesDirectoriesAndRedirect(t *testing.T) {
	f := newFixture(t, nil)

	resp := f.do(http.MethodGet, "/docs", nil)
	if resp.Code != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 301", resp.Code)
	}
	if got := resp.Header().Get("Location"); got != "/docs/" {
		t.Fatalf("Location = %q", got)
	}

	resp = f.do(http.MethodGet, "/docs/", nil)
	if resp.Code != http.StatusOK || resp.Body.String() != "<h1>docs</h1>" {
		t.Fatalf("status=%d body=%q", resp.Code, resp.Body.String())
	}

	resp = f.do(http.MethodGet, "/docs/guide.html", nil)
	if resp.Code != http.StatusOK || resp.Body.String() != "<h1>guide</h1>" {
		t.Fatalf("status=%d body=%q", resp.Code, resp.Body.String())
	}
}

func TestSPAFallbackAndExclusions(t *testing.T) {
	f := newFixture(t, func(s *snapshot.Snapshot) {
		p := s.Projects[testPID]
		p.SPA = model.SPAConfig{Enabled: true}
		s.Projects[testPID] = p
	})

	nav := map[string]string{"Accept": "text/html"}
	resp := f.do(http.MethodGet, "/dashboard/route", nav)
	if resp.Code != http.StatusOK || resp.Body.String() != "<h1>index</h1>" {
		t.Fatalf("SPA status=%d body=%q", resp.Code, resp.Body.String())
	}

	if resp := f.do(http.MethodGet, "/dashboard/app.js", nav); resp.Code != http.StatusNotFound {
		t.Fatalf("asset extension must not fall back, status = %d", resp.Code)
	}
	if resp := f.do(http.MethodGet, "/api/data", nav); resp.Code != http.StatusNotFound {
		t.Fatalf("excluded prefix must not fall back, status = %d", resp.Code)
	}
	if resp := f.do(http.MethodGet, "/dashboard/route", nil); resp.Code != http.StatusNotFound {
		t.Fatalf("non-navigation request must not fall back, status = %d", resp.Code)
	}
}

func TestErrorDocument(t *testing.T) {
	f := newFixture(t, nil)
	resp := f.do(http.MethodGet, "/missing", map[string]string{"Accept": "text/html"})
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d", resp.Code)
	}
	if resp.Body.String() != "<h1>missing</h1>" {
		t.Fatalf("body = %q, want error document", resp.Body.String())
	}
	if !strings.HasPrefix(resp.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("Content-Type = %q", resp.Header().Get("Content-Type"))
	}
}

func TestProjectErrorDocOverridesManifest(t *testing.T) {
	// project 在发布后把 error_doc 改成一个不存在的文件：不能再回退到包里的 404.html。
	f := newFixture(t, func(s *snapshot.Snapshot) {
		p := s.Projects[testPID]
		p.ErrorDoc = "5"
		s.Projects[testPID] = p
	})
	resp := f.do(http.MethodGet, "/missing", map[string]string{"Accept": "text/html"})
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d", resp.Code)
	}
	if resp.Body.String() == "<h1>missing</h1>" {
		t.Fatal("served stale manifest error document")
	}
	if !strings.HasPrefix(resp.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("Content-Type = %q", resp.Header().Get("Content-Type"))
	}

	// 改成包内存在的文件时，按实时配置返回该文件。
	f2 := newFixture(t, func(s *snapshot.Snapshot) {
		p := s.Projects[testPID]
		p.ErrorDoc = "docs/guide.html"
		s.Projects[testPID] = p
	})
	resp2 := f2.do(http.MethodGet, "/missing", map[string]string{"Accept": "text/html"})
	if resp2.Code != http.StatusNotFound || resp2.Body.String() != "<h1>guide</h1>" {
		t.Fatalf("status=%d body=%q", resp2.Code, resp2.Body.String())
	}
}

func TestDisabledProjectAndMissingSnapshot(t *testing.T) {
	f := newFixture(t, func(s *snapshot.Snapshot) {
		p := s.Projects[testPID]
		p.Status = model.ProjectDisabled
		s.Projects[testPID] = p
	})
	if resp := f.do(http.MethodGet, "/", nil); resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.Code)
	}

	empty := snapshot.NewStore()
	h := handler.New(handler.Options{
		Snapshots:      empty,
		Stores:         &fakeProvider{store: testutil.NewMemoryStore()},
		Manifests:      manifest.NewReader(),
		ReservedPrefix: "/__pageforge",
	})
	req := httptest.NewRequest(http.MethodGet, "http://"+testHost+"/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestEncodedSlashRejected(t *testing.T) {
	f := newFixture(t, nil)
	// %2F 在 URL.Path 中已被 net/http 解码为 /，只有 EscapedPath 保留编码；
	// 编码斜杠必须被拦截，不能当作普通路径段穿过路由。
	if resp := f.do(http.MethodGet, "/assets%2Fapp.js", nil); resp.Code != http.StatusBadRequest {
		t.Fatalf("encoded slash status = %d, want 400", resp.Code)
	}
	if resp := f.do(http.MethodGet, "/a%5Cb", nil); resp.Code != http.StatusBadRequest {
		t.Fatalf("encoded backslash status = %d, want 400", resp.Code)
	}
	// 合法路径不受影响。
	if resp := f.do(http.MethodGet, "/assets/app.js", nil); resp.Code != http.StatusOK {
		t.Fatalf("plain asset status = %d, want 200", resp.Code)
	}
}

func TestReservedPaths(t *testing.T) {
	f := newFixture(t, nil)
	if resp := f.do(http.MethodGet, "/__pageforge/healthz", nil); resp.Code != http.StatusOK {
		t.Fatalf("healthz status = %d", resp.Code)
	}
	if resp := f.do(http.MethodGet, "/__pageforge/readyz", nil); resp.Code != http.StatusOK {
		t.Fatalf("readyz status = %d", resp.Code)
	}

	f.update(func(s *snapshot.Snapshot) {
		s.Bindings = append(s.Bindings, snapshot.BindingView{
			ID: "bnd_x", Host: testHost, PathPrefix: "/x", ProjectID: testPID, Enabled: true,
		})
	})
	resp := f.do(http.MethodGet, "/x/__pageforge/secret", nil)
	if resp.Code != http.StatusNotFound || !strings.Contains(resp.Body.String(), "RESERVED_PATH") {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func TestReservedProxyForwarding(t *testing.T) {
	var gotPath, gotToken string
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotToken = r.Header.Get("X-Internal-Token")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_key":"ak"}`))
	}))
	defer control.Close()

	f := newFixture(t, nil)
	opts := fixtureOptions(t)
	opts.Snapshots = f.snapshots
	opts.Stores = &fakeProvider{store: f.store}
	opts.Manifests = manifest.NewReader()
	opts.ControlURL = control.URL
	opts.InternalToken = "secret-token"
	h := handler.New(opts)

	req := httptest.NewRequest(http.MethodGet, "http://"+testHost+"/__pageforge/internal/v1/aliases/ali_1/credentials", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/internal/v1/aliases/ali_1/credentials" {
		t.Fatalf("forwarded path = %q", gotPath)
	}
	if gotToken != "secret-token" {
		t.Fatalf("forwarded token = %q", gotToken)
	}
}

func TestRangeAndConditionalRequests(t *testing.T) {
	f := newFixture(t, nil)

	resp := f.do(http.MethodGet, "/big.txt", map[string]string{"Range": "bytes=2-5"})
	if resp.Code != http.StatusPartialContent || resp.Body.String() != "2345" {
		t.Fatalf("status=%d body=%q", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Content-Range"); got != "bytes 2-5/10" {
		t.Fatalf("Content-Range = %q", got)
	}

	resp = f.do(http.MethodGet, "/big.txt", map[string]string{"Range": "bytes=99-"})
	if resp.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want 416", resp.Code)
	}
	if got := resp.Header().Get("Content-Range"); got != "bytes */10" {
		t.Fatalf("Content-Range = %q", got)
	}

	etag := `"` + digest("0123456789") + `"`
	resp = f.do(http.MethodGet, "/big.txt", map[string]string{"If-None-Match": etag})
	if resp.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", resp.Code)
	}

	resp = f.do(http.MethodGet, "/big.txt", map[string]string{"If-Modified-Since": time.Unix(1700000000, 0).UTC().Format(http.TimeFormat)})
	if resp.Code != http.StatusNotModified {
		t.Fatalf("IMS status = %d, want 304", resp.Code)
	}
}

func TestPrecompressedNegotiation(t *testing.T) {
	f := newFixture(t, nil)
	resp := f.do(http.MethodGet, "/assets/app.js", map[string]string{"Accept-Encoding": "br"})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d", resp.Code)
	}
	if got := resp.Header().Get("Content-Encoding"); got != "br" {
		t.Fatalf("Content-Encoding = %q", got)
	}
	if resp.Body.String() != "BR-DATA" {
		t.Fatalf("body = %q", resp.Body.String())
	}
	if got := resp.Header().Get("ETag"); got != `"`+digest("console.log('app');")+`"` {
		t.Fatalf("ETag = %q, want original object hash", got)
	}

	plain := f.do(http.MethodGet, "/assets/app.js", nil)
	if plain.Header().Get("Content-Encoding") != "" || plain.Body.String() != "console.log('app');" {
		t.Fatalf("plain response encoding=%q body=%q", plain.Header().Get("Content-Encoding"), plain.Body.String())
	}
}

func newDiskCache(t *testing.T) *cache.Cache {
	t.Helper()
	c, err := cache.Open(cache.Options{Dir: t.TempDir(), MaxBytes: 1 << 20})
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestDiskCacheServesSecondRequestLocally(t *testing.T) {
	f := newFixture(t, nil)
	diskCache := newDiskCache(t)
	opts := fixtureOptions(t)
	opts.Snapshots = f.snapshots
	opts.Stores = &fakeProvider{store: f.wrapped}
	opts.Manifests = manifest.NewReader()
	opts.Cache = diskCache
	h := handler.New(opts)
	f.handler = h

	req := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "http://"+testHost+"/big.txt", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, request)
		return rec
	}
	if rec := req(); rec.Code != http.StatusOK || rec.Body.String() != "0123456789" {
		t.Fatalf("first status=%d body=%q", rec.Code, rec.Body.String())
	} else if got := rec.Header().Get("X-Cache"); got != "MISS" {
		t.Fatalf("first X-Cache = %q, want MISS", got)
	}
	firstGets := atomic.LoadInt32(&f.wrapped.gets)
	if firstGets != 1 {
		t.Fatalf("upstream gets = %d, want 1", firstGets)
	}
	if rec := req(); rec.Code != http.StatusOK || rec.Body.String() != "0123456789" {
		t.Fatalf("second status=%d body=%q", rec.Code, rec.Body.String())
	} else if got := rec.Header().Get("X-Cache"); got != "HIT" {
		t.Fatalf("second X-Cache = %q, want HIT", got)
	}
	if gets := atomic.LoadInt32(&f.wrapped.gets); gets != 1 {
		t.Fatalf("second request hit upstream: gets = %d", gets)
	}
	if diskCache.UsedBytes() != 10 {
		t.Fatalf("cache used = %d", diskCache.UsedBytes())
	}
}

// ctxBody 模拟 minio 的懒读取 body：只有真正 Read 时才受请求上下文影响。
type ctxBody struct {
	io.ReadCloser
	ctx context.Context
}

func (b *ctxBody) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	return b.ReadCloser.Read(p)
}

type lazyStore struct {
	objectstore.ObjectStore
}

func (s *lazyStore) Get(ctx context.Context, bucket, key string, opt objectstore.GetOptions) (io.ReadCloser, objectstore.ObjectInfo, error) {
	body, info, err := s.ObjectStore.Get(ctx, bucket, key, opt)
	if err != nil || !strings.HasPrefix(key, "projects/") {
		return body, info, err
	}
	return &ctxBody{ReadCloser: body, ctx: ctx}, info, nil
}

func TestDiskCacheDownloadSurvivesLazyBodyContext(t *testing.T) {
	f := newFixture(t, nil)
	diskCache := newDiskCache(t)
	opts := fixtureOptions(t)
	opts.Snapshots = f.snapshots
	opts.Stores = &fakeProvider{store: &lazyStore{ObjectStore: f.wrapped}}
	opts.Manifests = manifest.NewReader()
	opts.Cache = diskCache
	opts.UpstreamIdleTimeout = 5 * time.Second
	h := handler.New(opts)

	request := httptest.NewRequest(http.MethodGet, "http://"+testHost+"/big.txt", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request)
	if rec.Code != http.StatusOK || rec.Body.String() != "0123456789" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestApplyResponseHeadersHook(t *testing.T) {
	f := newFixture(t, nil)
	opts := fixtureOptions(t)
	opts.Snapshots = f.snapshots
	opts.Stores = &fakeProvider{store: f.store}
	opts.Manifests = manifest.NewReader()
	called := false
	opts.ApplyResponseHeaders = func(w http.ResponseWriter, project snapshot.ProjectView, binding snapshot.BindingView) {
		called = true
		w.Header().Set("X-Hook", project.Bucket+"|"+binding.ID)
	}
	h := handler.New(opts)

	req := httptest.NewRequest(http.MethodGet, "http://"+testHost+"/big.txt", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !called {
		t.Fatal("ApplyResponseHeaders hook was not called")
	}
	if got := rec.Header().Get("X-Hook"); got != testBucket+"|bnd_1" {
		t.Fatalf("X-Hook = %q", got)
	}
}

func TestPinnedVersionOverridesCurrent(t *testing.T) {
	f := newFixture(t, func(s *snapshot.Snapshot) {
		s.Bindings[0].PinnedVersionID = "ver_old"
	})
	resp := f.do(http.MethodGet, "/", nil)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a missing pinned manifest", resp.Code)
	}
}

func TestUnknownHostIs404(t *testing.T) {
	f := newFixture(t, nil)
	req := httptest.NewRequest(http.MethodGet, "http://other.example.com/", nil)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestSnapshotWithoutVersion(t *testing.T) {
	f := newFixture(t, func(s *snapshot.Snapshot) {
		p := s.Projects[testPID]
		p.CurrentVersionID = ""
		s.Projects[testPID] = p
	})
	resp := f.do(http.MethodGet, "/", nil)
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.Code)
	}
	if !strings.Contains(resp.Body.String(), "VERSION_NOT_AVAILABLE") {
		t.Fatalf("body = %s", resp.Body.String())
	}
}

func TestInvalidPathRejected(t *testing.T) {
	f := newFixture(t, nil)
	resp := f.do(http.MethodGet, "/..%2Fescape", nil)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", resp.Code, resp.Body.String())
	}
}

func TestErrorCodesAreJSON(t *testing.T) {
	f := newFixture(t, nil)
	resp := f.do(http.MethodPost, "/missing", nil)
	body := resp.Body.String()
	if !strings.Contains(body, `"code"`) || !strings.Contains(body, "METHOD_NOT_ALLOWED") {
		t.Fatalf("body = %s", body)
	}
	if !strings.HasPrefix(resp.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("Content-Type = %q", resp.Header().Get("Content-Type"))
	}
}

func TestNegativeCachePreventsRepeatedLookups(t *testing.T) {
	f := newFixture(t, nil)
	if err := f.store.Remove(context.Background(), testBucket, "projects/"+testPID+"/"+testVID+"/big.txt"); err != nil {
		t.Fatalf("remove object: %v", err)
	}
	diskCache := newDiskCache(t)
	opts := fixtureOptions(t)
	opts.Snapshots = f.snapshots
	opts.Stores = &fakeProvider{store: f.wrapped}
	opts.Manifests = manifest.NewReader()
	opts.Cache = diskCache
	h := handler.New(opts)
	f.handler = h

	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://"+testHost+"/big.txt", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := request(); rec.Code != http.StatusNotFound {
		t.Fatalf("first status = %d", rec.Code)
	}
	firstGets := atomic.LoadInt32(&f.wrapped.gets)
	if firstGets != 1 {
		t.Fatalf("upstream gets = %d, want 1", firstGets)
	}
	obj := cache.Object{AliasID: "ali_1", Bucket: testBucket, ProjectID: testPID, VersionID: testVID, Key: "big.txt"}
	if !diskCache.IsNotFound(obj) {
		t.Fatal("negative cache entry missing")
	}
	if rec := request(); rec.Code != http.StatusNotFound {
		t.Fatalf("second status = %d", rec.Code)
	}
	if gets := atomic.LoadInt32(&f.wrapped.gets); gets != firstGets {
		t.Fatalf("negative cache did not prevent a second upstream get: %d", gets)
	}
}

func TestManifestRootErrorIsNotServedAs404WhenObjectMissing(t *testing.T) {
	f := newFixture(t, nil)
	if err := f.store.Remove(context.Background(), testBucket, "projects/"+testPID+"/"+testVID+"/big.txt"); err != nil {
		t.Fatalf("remove object: %v", err)
	}
	resp := f.do(http.MethodGet, "/big.txt", nil)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a vanished object", resp.Code)
	}
	if !strings.Contains(resp.Body.String(), "NOT_FOUND") {
		t.Fatalf("body = %s", resp.Body.String())
	}
}

func TestHostNormalization(t *testing.T) {
	f := newFixture(t, nil)
	req := httptest.NewRequest(http.MethodGet, "http://site.example.com/", nil)
	req.Host = "SITE.example.com:80"
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestReservedPrefixWithQuery(t *testing.T) {
	f := newFixture(t, nil)
	resp := f.do(http.MethodGet, "/__pageforge/healthz?probe=1", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d", resp.Code)
	}
}

func TestServeFromCacheHead(t *testing.T) {
	f := newFixture(t, nil)
	diskCache := newDiskCache(t)
	opts := fixtureOptions(t)
	opts.Snapshots = f.snapshots
	opts.Stores = &fakeProvider{store: f.store}
	opts.Manifests = manifest.NewReader()
	opts.Cache = diskCache
	h := handler.New(opts)

	req := httptest.NewRequest(http.MethodHead, "http://"+testHost+"/big.txt", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Length") != "10" {
		t.Fatalf("Content-Length = %q", rec.Header().Get("Content-Length"))
	}
}

func TestRangeOnCachedObject(t *testing.T) {
	f := newFixture(t, nil)
	diskCache := newDiskCache(t)
	opts := fixtureOptions(t)
	opts.Snapshots = f.snapshots
	opts.Stores = &fakeProvider{store: f.wrapped}
	opts.Manifests = manifest.NewReader()
	opts.Cache = diskCache
	h := handler.New(opts)

	full := httptest.NewRequest(http.MethodGet, "http://"+testHost+"/big.txt", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, full)
	if rec.Code != http.StatusOK {
		t.Fatalf("warm status = %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "http://"+testHost+"/big.txt", nil)
	req.Header.Set("Range", "bytes=0-3")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "0123" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&f.wrapped.gets); got != 1 {
		t.Fatalf("range on cached object hit upstream: %d", got)
	}
}

func TestNoCacheStoresNothing(t *testing.T) {
	f := newFixture(t, nil)
	f.handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://"+testHost+"/big.txt", nil))
	if got := atomic.LoadInt32(&f.wrapped.gets); got != 1 {
		t.Fatalf("upstream gets = %d", got)
	}
	// The second call must go upstream again because no cache is configured.
	f.handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://"+testHost+"/big.txt", nil))
	if got := atomic.LoadInt32(&f.wrapped.gets); got != 2 {
		t.Fatalf("upstream gets = %d, want 2", got)
	}
}

func TestReservedHealthzIgnoresBrokenUpstream(t *testing.T) {
	f := newFixture(t, nil)
	f.store = testutil.NewMemoryStore() // no objects at all
	opts := fixtureOptions(t)
	opts.Snapshots = f.snapshots
	opts.Stores = &fakeProvider{store: f.store}
	opts.Manifests = manifest.NewReader()
	h := handler.New(opts)
	f.handler = h

	req := httptest.NewRequest(http.MethodGet, "http://"+testHost+"/__pageforge/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestServeChineseFilenameViaBinding(t *testing.T) {
	f := newFixture(t, nil)

	name := "更新记录.html"
	body := "<h1>chinese</h1>"
	builder, err := manifest.NewBuilder("_system/manifests/"+testPID+"/"+testVID, manifest.Limits{}, t.TempDir())
	if err != nil {
		t.Fatalf("new manifest builder: %v", err)
	}
	defer func() { _ = builder.Close() }()
	ctx := context.Background()
	for _, entry := range []manifest.Entry{
		{Key: "index.html", Size: 13, SHA256: digest("<h1>index</h1>"), ContentType: "text/html; charset=utf-8", ModTime: 1700000000},
		{Key: "404.html", Size: 13, SHA256: digest("<h1>missing</h1>"), ContentType: "text/html; charset=utf-8", ModTime: 1700000000},
		{Key: name, Size: int64(len(body)), SHA256: digest(body), ContentType: "text/html; charset=utf-8", ModTime: 1700000000},
	} {
		if err := builder.Add(entry); err != nil {
			t.Fatalf("add manifest entry: %v", err)
		}
	}
	if _, err := builder.Build(ctx, f.store, testBucket, manifest.RootOptions{
		ProjectID: testPID, VersionID: testVID, IndexDoc: "index.html", ErrorDoc: "404.html",
	}); err != nil {
		t.Fatalf("build manifest: %v", err)
	}

	s3Key := "projects/" + testPID + "/" + testVID + "/" + deploy.StorageKey(name)
	if err := f.store.Put(ctx, testBucket, s3Key,
		strings.NewReader(body), int64(len(body)), objectstore.PutOptions{ContentType: "text/html"}); err != nil {
		t.Fatalf("put object: %v", err)
	}

	path := "/%E6%9B%B4%E6%96%B0%E8%AE%B0%E5%BD%95.html"
	rec := f.do(http.MethodGet, path, nil)
	t.Logf("status=%d body=%q", rec.Code, rec.Body.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}
