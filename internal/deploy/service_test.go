package deploy_test

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/pageforge/pageforge/internal/alias"
	"github.com/pageforge/pageforge/internal/deploy"
	"github.com/pageforge/pageforge/internal/manifest"
	"github.com/pageforge/pageforge/internal/objectstore"
	"github.com/pageforge/pageforge/internal/project"
	"github.com/pageforge/pageforge/internal/store"
	"github.com/pageforge/pageforge/internal/testutil"
	"github.com/pageforge/pageforge/internal/version"
)

type harness struct {
	service  *deploy.Service
	store    *testutil.MemoryStore
	db       *store.Store
	projects *project.Repo
	versions *version.Repo
	project  project.Project
}

func newHarness(t *testing.T, retention bool) *harness {
	t.Helper()
	ctx := context.Background()
	db := testutil.NewDB(t)

	aliases := alias.NewRepo(db, testutil.NewCipher(t))
	created, err := aliases.Create(ctx, alias.CreateInput{
		Name:            "test",
		Endpoint:        "http://127.0.0.1:9000",
		AccessKey:       "key",
		SecretKey:       "secret",
		BucketAllowlist: []string{"static"},
	})
	if err != nil {
		t.Fatalf("create alias: %v", err)
	}

	projects := project.NewRepo(db)
	p, err := projects.Create(ctx, project.CreateInput{
		Name:             "site",
		AliasID:          created.ID,
		Bucket:           "static",
		PackageRetention: &retention,
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	mem := testutil.NewMemoryStore()
	service := deploy.NewService(db, deploy.Options{
		Config: deploy.Config{TempDir: t.TempDir(), Workers: 4},
		Stores: func(context.Context, project.Project) (objectstore.ObjectStore, error) {
			return mem, nil
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return &harness{
		service:  service,
		store:    mem,
		db:       db,
		projects: projects,
		versions: version.NewRepo(db),
		project:  p,
	}
}

func buildZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for name, body := range files {
		part, err := writer.Create(name)
		if err != nil {
			t.Fatalf("create zip entry %q: %v", name, err)
		}
		if _, err := part.Write([]byte(body)); err != nil {
			t.Fatalf("write zip entry %q: %v", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func (h *harness) publish(t *testing.T, files map[string]string) deploy.View {
	t.Helper()
	ctx := context.Background()
	view, _, err := h.service.Create(ctx, deploy.CreateInput{ProjectID: h.project.ID})
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	pkg := buildZip(t, files)
	if _, err := h.service.PutPackage(ctx, view.DeploymentID, bytes.NewReader(pkg), int64(len(pkg))); err != nil {
		t.Fatalf("put package: %v", err)
	}
	view, err = h.service.Complete(ctx, view.DeploymentID)
	if err != nil {
		t.Fatalf("complete deployment: %v", err)
	}
	return view
}

func readRoot(t *testing.T, store *testutil.MemoryStore, key string) manifest.Root {
	t.Helper()
	data, ok := store.Read("static", key)
	if !ok {
		t.Fatalf("root manifest %q missing", key)
	}
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip root: %v", err)
	}
	defer reader.Close()
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	var root manifest.Root
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("decode root: %v", err)
	}
	return root
}

func TestPublishHappyPath(t *testing.T) {
	h := newHarness(t, true)
	view := h.publish(t, map[string]string{
		"index.html":     "<html>index</html>",
		"assets/app.js":  "console.log(1)",
		"pageforge.json": `{"error_doc":"404.html","content_types":{"js":"text/x-custom"}}`,
	})

	if view.Status != deploy.StatusReady {
		t.Fatalf("status = %s (%s)", view.Status, view.ErrorMessage)
	}
	if view.FileCount != 2 || view.TotalBytes != int64(len("<html>index</html>")+len("console.log(1)")) {
		t.Fatalf("count/bytes = %d/%d", view.FileCount, view.TotalBytes)
	}
	if view.ManifestSHA256 == "" {
		t.Fatal("manifest sha is empty")
	}

	p, err := h.projects.Get(context.Background(), h.project.ID)
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if p.CurrentVersionID != view.VersionID {
		t.Fatalf("current version = %q, want %q", p.CurrentVersionID, view.VersionID)
	}
	ver, err := h.versions.Get(context.Background(), h.project.ID, view.VersionID)
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	if ver.Status != version.StatusReady || ver.FileCount != 2 || ver.ManifestSHA256 != view.ManifestSHA256 {
		t.Fatalf("version = %+v", ver)
	}

	objectPrefix := "projects/" + h.project.ID + "/" + view.VersionID
	keys := h.store.Keys("static")
	for _, key := range []string{objectPrefix + "/index.html", objectPrefix + "/assets/app.js"} {
		if !containsKey(keys, key) {
			t.Fatalf("missing object %q in %v", key, keys)
		}
	}
	if containsKey(keys, objectPrefix+"/pageforge.json") {
		t.Fatal("pageforge.json must not be published")
	}
	archiveKey := "_system/packages/" + h.project.ID + "/" + view.VersionID + ".zip"
	if !containsKey(keys, archiveKey) {
		t.Fatalf("package archive %q missing", archiveKey)
	}

	root := readRoot(t, h.store, ver.ManifestPrefix+"/root.json")
	if root.IndexDoc != "index.html" {
		t.Fatalf("index doc = %q", root.IndexDoc)
	}
	if root.ErrorDoc != "404.html" {
		t.Fatalf("error doc = %q", root.ErrorDoc)
	}
	if root.FileCount != 2 || root.ProjectID != h.project.ID || root.VersionID != view.VersionID {
		t.Fatalf("root = %+v", root)
	}

	shardKey := ver.ManifestPrefix + "/" + root.Shards["assets"]
	shard := readShard(t, h.store, shardKey)
	entry, ok := shard.Files["assets/app.js"]
	if !ok {
		t.Fatalf("assets/app.js missing from shard %v", shard.Files)
	}
	if entry.ContentType != "text/x-custom" {
		t.Fatalf("content type = %q, want override", entry.ContentType)
	}
	if entry.SHA256 == "" || entry.Size != int64(len("console.log(1)")) {
		t.Fatalf("entry = %+v", entry)
	}
}

func TestPublishRejectsMaliciousPackageAndKeepsPointer(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()
	view, _, err := h.service.Create(ctx, deploy.CreateInput{ProjectID: h.project.ID})
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	pkg := buildZip(t, map[string]string{"../evil.txt": "x"})
	if _, err := h.service.PutPackage(ctx, view.DeploymentID, bytes.NewReader(pkg), int64(len(pkg))); err != nil {
		t.Fatalf("put package: %v", err)
	}
	_, err = h.service.Complete(ctx, view.DeploymentID)
	if err == nil {
		t.Fatal("malicious package published")
	}
	testutil.RequireCode(t, err, "INVALID_PACKAGE")

	updated, err := h.service.Get(ctx, view.DeploymentID)
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if updated.Status != deploy.StatusFailed {
		t.Fatalf("deployment status = %s", updated.Status)
	}
	ver, err := h.versions.Get(ctx, h.project.ID, view.VersionID)
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	if ver.Status != version.StatusFailed {
		t.Fatalf("version status = %s", ver.Status)
	}
	p, err := h.projects.Get(ctx, h.project.ID)
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if p.CurrentVersionID != "" {
		t.Fatalf("pointer moved to %q", p.CurrentVersionID)
	}
}

func TestCreateIsIdempotentAndExpires(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()

	first, reused, err := h.service.Create(ctx, deploy.CreateInput{ProjectID: h.project.ID, IdempotencyKey: "key-1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if reused {
		t.Fatal("first create reported reuse")
	}
	second, reused, err := h.service.Create(ctx, deploy.CreateInput{ProjectID: h.project.ID, IdempotencyKey: "key-1"})
	if err != nil {
		t.Fatalf("create again: %v", err)
	}
	if !reused || second.DeploymentID != first.DeploymentID {
		t.Fatalf("idempotent replay = %+v reused=%v", second, reused)
	}

	if _, err := h.db.ExecContext(ctx,
		`UPDATE deployments SET created_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-48*time.Hour), first.DeploymentID); err != nil {
		t.Fatalf("expire deployment: %v", err)
	}
	third, reused, err := h.service.Create(ctx, deploy.CreateInput{ProjectID: h.project.ID, IdempotencyKey: "key-1"})
	if err != nil {
		t.Fatalf("create after expiry: %v", err)
	}
	if reused || third.DeploymentID == first.DeploymentID {
		t.Fatalf("expired key was reused: %+v", third)
	}
}

func TestCancelDeployment(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()

	view, _, err := h.service.Create(ctx, deploy.CreateInput{ProjectID: h.project.ID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	pkg := buildZip(t, map[string]string{"index.html": "hi"})
	if _, err := h.service.PutPackage(ctx, view.DeploymentID, bytes.NewReader(pkg), int64(len(pkg))); err != nil {
		t.Fatalf("put package: %v", err)
	}
	canceled, err := h.service.Cancel(ctx, view.DeploymentID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if canceled.Status != deploy.StatusCanceled {
		t.Fatalf("status = %s", canceled.Status)
	}
	if _, err := h.service.Complete(ctx, view.DeploymentID); err == nil {
		t.Fatal("canceled deployment completed")
	} else {
		testutil.RequireCode(t, err, "DEPLOYMENT_NOT_COMPLETABLE")
	}
	ver, err := h.versions.Get(ctx, h.project.ID, view.VersionID)
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	if ver.Status != version.StatusFailed {
		t.Fatalf("version status = %s", ver.Status)
	}

	ready := h.publish(t, map[string]string{"index.html": "ready"})
	if _, err := h.service.Cancel(ctx, ready.DeploymentID); err == nil {
		t.Fatal("ready deployment was canceled")
	} else {
		testutil.RequireCode(t, err, "DEPLOYMENT_NOT_CANCELABLE")
	}
}

func TestCompleteWithoutPackage(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()
	view, _, err := h.service.Create(ctx, deploy.CreateInput{ProjectID: h.project.ID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := h.service.Complete(ctx, view.DeploymentID); err == nil {
		t.Fatal("complete without package succeeded")
	} else {
		testutil.RequireCode(t, err, "PACKAGE_NOT_UPLOADED")
	}
}

func TestPackageRetentionDisabled(t *testing.T) {
	h := newHarness(t, false)
	view := h.publish(t, map[string]string{"index.html": "hi"})
	archiveKey := "_system/packages/" + h.project.ID + "/" + view.VersionID + ".zip"
	if containsKey(h.store.Keys("static"), archiveKey) {
		t.Fatal("package was archived despite retention being disabled")
	}
}

func TestPublishUsesProjectDocumentOverrides(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()
	indexDoc := "home.html"
	if _, err := h.projects.Update(ctx, h.project.ID, project.UpdateInput{IndexDoc: &indexDoc}); err != nil {
		t.Fatalf("update project: %v", err)
	}
	view := h.publish(t, map[string]string{
		"home.html":      "home",
		"index.html":     "index",
		"pageforge.json": `{"index_doc":"other.html","error_doc":"404.html"}`,
	})
	ver, err := h.versions.Get(ctx, h.project.ID, view.VersionID)
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	root := readRoot(t, h.store, ver.ManifestPrefix+"/root.json")
	if root.IndexDoc != "home.html" {
		t.Fatalf("index doc = %q, want project value", root.IndexDoc)
	}
	if root.ErrorDoc != "404.html" {
		t.Fatalf("error doc = %q", root.ErrorDoc)
	}
}

func TestPublishRejectsInvalidPageConfig(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()
	view, _, err := h.service.Create(ctx, deploy.CreateInput{ProjectID: h.project.ID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	pkg := buildZip(t, map[string]string{
		"index.html":     "hi",
		"pageforge.json": `{"content_types": {"js": "not a mime"}}`,
	})
	if _, err := h.service.PutPackage(ctx, view.DeploymentID, bytes.NewReader(pkg), int64(len(pkg))); err != nil {
		t.Fatalf("put package: %v", err)
	}
	if _, err := h.service.Complete(ctx, view.DeploymentID); err == nil {
		t.Fatal("invalid pageforge.json was accepted")
	} else {
		testutil.RequireCode(t, err, "INVALID_PAGE_CONFIG")
	}
	if containsKey(h.store.Keys("static"), "projects/"+h.project.ID+"/"+view.VersionID+"/index.html") {
		t.Fatal("objects were uploaded despite invalid config")
	}
}

func readShard(t *testing.T, store *testutil.MemoryStore, key string) manifest.Shard {
	t.Helper()
	data, ok := store.Read("static", key)
	if !ok {
		t.Fatalf("shard %q missing", key)
	}
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip shard: %v", err)
	}
	defer reader.Close()
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read shard: %v", err)
	}
	var shard manifest.Shard
	if err := json.Unmarshal(raw, &shard); err != nil {
		t.Fatalf("decode shard: %v", err)
	}
	return shard
}

func TestCreateDeploymentByProjectName(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()

	// project_name 可定位目标，解析结果与 project_id 一致
	view, reused, err := h.service.Create(ctx, deploy.CreateInput{ProjectName: "site"})
	if err != nil {
		t.Fatalf("create by project_name: %v", err)
	}
	if reused {
		t.Fatal("fresh deployment marked as reused")
	}
	if view.ProjectID != h.project.ID {
		t.Fatalf("project_id = %q, want %q", view.ProjectID, h.project.ID)
	}

	// project_id 与 project_name 互斥
	if _, _, err := h.service.Create(ctx, deploy.CreateInput{ProjectID: h.project.ID, ProjectName: "site"}); err != nil {
		testutil.RequireCode(t, err, "INVALID_DEPLOYMENT")
	} else {
		t.Fatal("both project_id and project_name accepted")
	}

	// 两者都缺省
	if _, _, err := h.service.Create(ctx, deploy.CreateInput{}); err != nil {
		testutil.RequireCode(t, err, "INVALID_DEPLOYMENT")
	} else {
		t.Fatal("missing target accepted")
	}

	// 未知 name
	if _, _, err := h.service.Create(ctx, deploy.CreateInput{ProjectName: "missing"}); err != nil {
		testutil.RequireCode(t, err, "PROJECT_NOT_FOUND")
	} else {
		t.Fatal("unknown project_name accepted")
	}
}

func TestPublishWithoutAutoPublish(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()

	// 先正常发布 v1 作为当前版本
	first := h.publish(t, map[string]string{"index.html": "v1"})

	// 以 auto_publish=false 上传 v2：版本 ready，但指针保持 v1
	noAuto := false
	view, _, err := h.service.Create(ctx, deploy.CreateInput{ProjectID: h.project.ID, AutoPublish: &noAuto})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if view.AutoPublish {
		t.Fatal("view auto_publish = true, want false")
	}
	pkg := buildZip(t, map[string]string{"index.html": "v2"})
	if _, err := h.service.PutPackage(ctx, view.DeploymentID, bytes.NewReader(pkg), int64(len(pkg))); err != nil {
		t.Fatalf("put package: %v", err)
	}
	done, err := h.service.Complete(ctx, view.DeploymentID)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if done.Status != deploy.StatusReady {
		t.Fatalf("status = %s", done.Status)
	}

	p, err := h.projects.Get(ctx, h.project.ID)
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if p.CurrentVersionID != first.VersionID {
		t.Fatalf("pointer = %q, want %q", p.CurrentVersionID, first.VersionID)
	}
	ver, err := h.versions.Get(ctx, h.project.ID, view.VersionID)
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	if ver.Status != version.StatusReady {
		t.Fatalf("version status = %s", ver.Status)
	}
	if ver.PublishedAt != nil {
		t.Fatalf("published_at = %v, want nil", ver.PublishedAt)
	}

	// 通过指针接口手动上线 v2，published_at 被补写
	if _, err := h.versions.SetPointer(ctx, h.project.ID, view.VersionID, ""); err != nil {
		t.Fatalf("set pointer: %v", err)
	}
	ver, err = h.versions.Get(ctx, h.project.ID, view.VersionID)
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	if ver.PublishedAt == nil {
		t.Fatal("published_at was not stamped on manual publish")
	}
	p, err = h.projects.Get(ctx, h.project.ID)
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if p.CurrentVersionID != view.VersionID {
		t.Fatalf("pointer = %q, want %q", p.CurrentVersionID, view.VersionID)
	}

	// 缺省 auto_publish 行为不变：上传即切换指针
	def := h.publish(t, map[string]string{"index.html": "v3"})
	if !def.AutoPublish {
		t.Fatal("default auto_publish = false, want true")
	}
	p, err = h.projects.Get(ctx, h.project.ID)
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if p.CurrentVersionID != def.VersionID {
		t.Fatalf("pointer = %q, want %q", p.CurrentVersionID, def.VersionID)
	}
}

func containsKey(keys []string, key string) bool {
	for _, candidate := range keys {
		if candidate == key {
			return true
		}
	}
	return false
}

func TestListDeployments(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()

	// 已正常发布、带有版本与文件计数的部署
	published := h.publish(t, map[string]string{"index.html": "v1"})

	// 另一条部署的记录，但其版本被软删除，LEFT JOIN 结果为空
	// （回归：file_count/total_bytes 为 NULL 时不能直接 Scan 到 int64）
	orphan, _, err := h.service.Create(ctx, deploy.CreateInput{ProjectID: h.project.ID})
	if err != nil {
		t.Fatalf("create orphan: %v", err)
	}
	if _, err := h.db.ExecContext(ctx,
		`UPDATE versions SET deleted_at = ? WHERE deployment_id = ?`,
		time.Now().UTC(), orphan.DeploymentID); err != nil {
		t.Fatalf("soft-delete version: %v", err)
	}

	items, err := h.service.List(ctx, h.project.ID, 0)
	if err != nil {
		t.Fatalf("list deployments: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("len = %d, want 2", len(items))
	}
	// 最新在前：orphan 是后建的，应排在第一
	if items[0].DeploymentID != orphan.DeploymentID {
		t.Fatalf("first = %q, want orphan %q", items[0].DeploymentID, orphan.DeploymentID)
	}
	if items[0].VersionID != "" || items[0].FileCount != 0 {
		t.Fatalf("orphan item = %+v, want empty version and zero counts", items[0])
	}
	if items[0].ProjectName != "site" {
		t.Fatalf("project_name = %q, want site", items[0].ProjectName)
	}

	if items[1].DeploymentID != published.DeploymentID {
		t.Fatalf("second = %q, want %q", items[1].DeploymentID, published.DeploymentID)
	}
	if items[1].VersionID != published.VersionID || items[1].FileCount != published.FileCount {
		t.Fatalf("published item = %+v, want version %q count %d", items[1], published.VersionID, published.FileCount)
	}

	// 过滤：不存在的 project 应返回空
	empty, err := h.service.List(ctx, "no-such-project", 0)
	if err != nil {
		t.Fatalf("list by unknown project: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("len = %d, want 0", len(empty))
	}
}
