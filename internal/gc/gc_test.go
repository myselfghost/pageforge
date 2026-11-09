package gc_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pageforge/pageforge/internal/alias"
	"github.com/pageforge/pageforge/internal/binding"
	"github.com/pageforge/pageforge/internal/gc"
	"github.com/pageforge/pageforge/internal/objectstore"
	"github.com/pageforge/pageforge/internal/project"
	"github.com/pageforge/pageforge/internal/store"
	"github.com/pageforge/pageforge/internal/testutil"
	"github.com/pageforge/pageforge/internal/version"
)

type harness struct {
	service  *gc.Service
	store    *testutil.MemoryStore
	db       *store.Store
	versions *version.Repo
	project  project.Project
}

func newHarness(t *testing.T) *harness {
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
		Name: "site", AliasID: created.ID, Bucket: "static",
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	mem := testutil.NewMemoryStore()
	service := gc.NewService(db, func(context.Context, project.Project) (objectstore.ObjectStore, error) {
		return mem, nil
	}, gc.Config{
		Interval:     time.Hour,
		OrphanGrace:  24 * time.Hour,
		KeepVersions: 2,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	return &harness{
		service:  service,
		store:    mem,
		db:       db,
		versions: version.NewRepo(db),
		project:  p,
	}
}

func (h *harness) readyVersion(t *testing.T, label string, age time.Duration) version.Version {
	t.Helper()
	ctx := context.Background()
	v, err := h.versions.Create(ctx, h.project.ID, "dep_"+label, label,
		"_system/manifests/"+h.project.ID+"/ver_"+label)
	if err != nil {
		t.Fatalf("create version %s: %v", label, err)
	}
	if _, err := h.versions.MarkReady(ctx, h.project.ID, v.ID, "sha", 1, 10); err != nil {
		t.Fatalf("mark ready %s: %v", label, err)
	}
	if _, err := h.db.ExecContext(ctx, `UPDATE versions SET created_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-age), v.ID); err != nil {
		t.Fatalf("age version %s: %v", label, err)
	}
	return v
}

func (h *harness) putObject(t *testing.T, key string) {
	t.Helper()
	if err := h.store.Put(context.Background(), "static", key, stringReader("data"), 4, objectstore.PutOptions{}); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

func TestRunDeletesExpiredVersions(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	v1 := h.readyVersion(t, "v1", 40*24*time.Hour)
	v2 := h.readyVersion(t, "v2", 30*24*time.Hour)
	v3 := h.readyVersion(t, "v3", 20*24*time.Hour)
	v4 := h.readyVersion(t, "v4", 10*24*time.Hour)

	if _, err := h.versions.SetPointer(ctx, h.project.ID, v4.ID, ""); err != nil {
		t.Fatalf("set pointer: %v", err)
	}
	if _, err := h.versions.SetProtected(ctx, h.project.ID, v3.ID, true); err != nil {
		t.Fatalf("protect v3: %v", err)
	}
	bindings := binding.NewRepo(h.db, "/_pageforge")
	if _, err := bindings.Create(ctx, binding.CreateInput{
		Host: "example.com", PathPrefix: "/", ProjectID: h.project.ID, PinnedVersionID: v2.ID,
	}); err != nil {
		t.Fatalf("create binding: %v", err)
	}

	h.putObject(t, "projects/"+h.project.ID+"/"+v1.ID+"/index.html")
	h.putObject(t, "_system/manifests/"+h.project.ID+"/ver_v1/root.json")
	h.putObject(t, "_system/packages/"+h.project.ID+"/"+v1.ID+".zip")

	report, err := h.service.Run(ctx, true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !report.DryRun || report.Versions != 1 || report.Objects != 3 {
		t.Fatalf("dry run report = %+v", report)
	}
	if _, err := h.versions.Get(ctx, h.project.ID, v1.ID); err != nil {
		t.Fatalf("dry run removed version: %v", err)
	}
	if _, ok := h.store.Read("static", "projects/"+h.project.ID+"/"+v1.ID+"/index.html"); !ok {
		t.Fatal("dry run removed objects")
	}

	report, err = h.service.Run(ctx, false)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if report.Versions != 1 || report.Objects != 3 {
		t.Fatalf("report = %+v", report)
	}
	if _, err := h.versions.Get(ctx, h.project.ID, v1.ID); err == nil {
		t.Fatal("expired version was not soft deleted")
	} else {
		testutil.RequireCode(t, err, "VERSION_NOT_FOUND")
	}
	for _, key := range []string{
		"projects/" + h.project.ID + "/" + v1.ID + "/index.html",
		"_system/manifests/" + h.project.ID + "/ver_v1/root.json",
		"_system/packages/" + h.project.ID + "/" + v1.ID + ".zip",
	} {
		if _, ok := h.store.Read("static", key); ok {
			t.Fatalf("object %q was not removed", key)
		}
	}
	for _, keep := range []version.Version{v2, v3, v4} {
		if _, err := h.versions.Get(ctx, h.project.ID, keep.ID); err != nil {
			t.Fatalf("version %s was removed: %v", keep.ID, err)
		}
	}
}

func TestRunRemovesFailedVersionObjects(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	v, err := h.versions.Create(ctx, h.project.ID, "dep_failed", "failed",
		"_system/manifests/"+h.project.ID+"/ver_failed")
	if err != nil {
		t.Fatalf("create version: %v", err)
	}
	if err := h.versions.MarkFailed(ctx, h.project.ID, v.ID, "boom"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	if _, err := h.db.ExecContext(ctx, `UPDATE versions SET created_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-48*time.Hour), v.ID); err != nil {
		t.Fatalf("age version: %v", err)
	}
	h.putObject(t, "projects/"+h.project.ID+"/"+v.ID+"/index.html")

	report, err := h.service.Run(ctx, false)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if report.Objects != 1 {
		t.Fatalf("objects removed = %d, want 1", report.Objects)
	}
	if report.OrphansPurged != 1 {
		t.Fatalf("orphans purged = %d, want 1", report.OrphansPurged)
	}
	if _, ok := h.store.Read("static", "projects/"+h.project.ID+"/"+v.ID+"/index.html"); ok {
		t.Fatal("failed version objects were not removed")
	}

	// 第二轮不应重复处理：孤儿对象已标记清理，跳过该版本。
	second, err := h.service.Run(ctx, false)
	if err != nil {
		t.Fatalf("gc second run: %v", err)
	}
	if second.Objects != 0 || second.Versions != 0 || second.OrphansPurged != 0 {
		t.Fatalf("second run should be a no-op, report = %+v", second)
	}
}

// 软删除的版本：第二轮 GC 不应重复扫描/重复删除其对象。
func TestRunSoftDeletedVersionPurgedOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	v := h.readyVersion(t, "sd1", 30*24*time.Hour)
	h.putObject(t, "projects/"+h.project.ID+"/"+v.ID+"/index.html")

	// 手动软删除，并把删除时间置于宽限期之前。
	if err := h.versions.Delete(ctx, h.project.ID, v.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if _, err := h.db.ExecContext(ctx, `UPDATE versions SET deleted_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-48*time.Hour), v.ID); err != nil {
		t.Fatalf("age deleted_at: %v", err)
	}

	first, err := h.service.Run(ctx, false)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if first.Objects != 1 {
		t.Fatalf("objects removed = %d, want 1", first.Objects)
	}
	var purged sql.NullString
	if err := h.db.QueryRowContext(ctx,
		`SELECT objects_purged_at FROM versions WHERE id = ?`, v.ID).Scan(&purged); err != nil {
		t.Fatalf("read objects_purged_at: %v", err)
	}
	if !purged.Valid {
		t.Fatal("objects_purged_at should be set after purge")
	}

	second, err := h.service.Run(ctx, false)
	if err != nil {
		t.Fatalf("gc second run: %v", err)
	}
	if second.Objects != 0 {
		t.Fatalf("second run should not re-purge soft-deleted objects, report = %+v", second)
	}
}

func TestRunCleansStagingDirectories(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	dir := filepath.Join(t.TempDir(), "staging")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create staging: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.zip"), []byte("zip"), 0o600); err != nil {
		t.Fatalf("write staging file: %v", err)
	}
	if _, err := h.db.ExecContext(ctx, `INSERT INTO deployments
		(id, project_id, mode, status, idempotency_key, package_key, staging_prefix, error_message, created_at, completed_at)
		VALUES (?, ?, 'zip', 'ready', NULL, '', ?, '', ?, ?)`,
		"dep_staging", h.project.ID, dir, time.Now().UTC().Add(-48*time.Hour), time.Now().UTC()); err != nil {
		t.Fatalf("insert deployment: %v", err)
	}

	report, err := h.service.Run(ctx, false)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if report.StagingDirs != 1 {
		t.Fatalf("staging cleaned = %d", report.StagingDirs)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("staging directory still exists: %v", err)
	}
	var prefix string
	if err := h.db.QueryRowContext(ctx,
		`SELECT staging_prefix FROM deployments WHERE id = ?`, "dep_staging").Scan(&prefix); err != nil {
		t.Fatalf("read deployment: %v", err)
	}
	if prefix != "" {
		t.Fatalf("staging prefix = %q", prefix)
	}
}

func TestRunProtectsShareLinkedVersions(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// 三个版本中，只有 v1 落在 keep_versions=2 窗口之外且无保护。
	v1 := h.readyVersion(t, "s1", 40*24*time.Hour)
	h.readyVersion(t, "s2", 30*24*time.Hour)
	v3 := h.readyVersion(t, "s3", 20*24*time.Hour)
	if _, err := h.versions.SetPointer(ctx, h.project.ID, v3.ID, ""); err != nil {
		t.Fatalf("set pointer: %v", err)
	}

	insertShare := func(id, versionID string, enabled bool, expires *time.Time) {
		t.Helper()
		var expiresArg any
		if expires != nil {
			expiresArg = *expires
		}
		if _, err := h.db.ExecContext(ctx, `INSERT INTO share_links
			(id, token, project_id, version_id, password_hash, expires_at, max_views, views, cache_policy, enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, '', ?, 0, 0, '{}', ?, ?, ?)`,
			id, "tok_"+id, h.project.ID, versionID, expiresArg, enabled,
			time.Now().UTC(), time.Now().UTC()); err != nil {
			t.Fatalf("insert share link %s: %v", id, err)
		}
	}

	// 活跃、未过期的分享链接指向 v1（窗口外）→ 必须被保护。
	insertShare("shr_active", v1.ID, true, nil)

	h.putObject(t, "projects/"+h.project.ID+"/"+v1.ID+"/index.html")

	report, err := h.service.Run(ctx, false)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if report.Versions != 0 {
		t.Fatalf("share-linked version was deleted: report=%+v", report)
	}
	if _, err := h.versions.Get(ctx, h.project.ID, v1.ID); err != nil {
		t.Fatalf("share-linked version %s was removed: %v", v1.ID, err)
	}

	// 过期后不再保护：同一次运行即可回收 v1（以及窗口外无保护的 v2 视情况）。
	past := time.Now().UTC().Add(-time.Hour)
	if _, err := h.db.ExecContext(ctx, `UPDATE share_links SET expires_at = ? WHERE id = ?`, past, "shr_active"); err != nil {
		t.Fatalf("expire share link: %v", err)
	}
	report, err = h.service.Run(ctx, false)
	if err != nil {
		t.Fatalf("gc after expiry: %v", err)
	}
	if report.Versions != 1 {
		t.Fatalf("expired share link should release v1: report=%+v", report)
	}
	if _, err := h.versions.Get(ctx, h.project.ID, v1.ID); err == nil {
		t.Fatal("v1 should be deleted after the share link expired")
	}
}

func stringReader(value string) io.Reader { return strings.NewReader(value) }

// setProjectRetention 直接写入项目的保留策略（nil 表示继承全局）。
func (h *harness) setProjectRetention(t *testing.T, keepVersions, keepDays *int) {
	t.Helper()
	if _, err := h.db.ExecContext(context.Background(),
		`UPDATE projects SET keep_versions = ?, keep_days = ? WHERE id = ?`,
		keepVersions, keepDays, h.project.ID); err != nil {
		t.Fatalf("set project retention: %v", err)
	}
}

// 项目设置 keep_versions 时应覆盖全局值（全局为 2）。
func TestRunUsesProjectKeepVersionsOverride(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	v1 := h.readyVersion(t, "v1", 40*24*time.Hour)
	v2 := h.readyVersion(t, "v2", 30*24*time.Hour)
	v3 := h.readyVersion(t, "v3", 20*24*time.Hour)
	if _, err := h.versions.SetPointer(ctx, h.project.ID, v3.ID, ""); err != nil {
		t.Fatalf("set pointer: %v", err)
	}
	h.putObject(t, "projects/"+h.project.ID+"/"+v1.ID+"/index.html")

	// 项目覆盖为保留最新 3 个：全局只有 2，但这里 v1/v2/v3 全部保留。
	three := 3
	h.setProjectRetention(t, &three, nil)

	report, err := h.service.Run(ctx, false)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if report.Versions != 0 {
		t.Fatalf("project override should keep all versions: report=%+v", report)
	}
	for _, v := range []version.Version{v1, v2, v3} {
		if _, err := h.versions.Get(ctx, h.project.ID, v.ID); err != nil {
			t.Fatalf("version %s should be kept: %v", v.ID, err)
		}
	}
}

// 项目显式设置为 0/0 时覆盖全局的删除策略（0=不按该维度保留，两者都 0 即不删除）。
func TestRunProjectZeroRetentionKeepsEverything(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	v1 := h.readyVersion(t, "z1", 90*24*time.Hour)
	v2 := h.readyVersion(t, "z2", 80*24*time.Hour)
	v3 := h.readyVersion(t, "z3", 70*24*time.Hour)
	if _, err := h.versions.SetPointer(ctx, h.project.ID, v3.ID, ""); err != nil {
		t.Fatalf("set pointer: %v", err)
	}
	h.putObject(t, "projects/"+h.project.ID+"/"+v1.ID+"/index.html")

	zero := 0
	h.setProjectRetention(t, &zero, &zero)

	report, err := h.service.Run(ctx, false)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if report.Versions != 0 {
		t.Fatalf("0/0 should keep everything: report=%+v", report)
	}
	for _, v := range []version.Version{v1, v2, v3} {
		if _, err := h.versions.Get(ctx, h.project.ID, v.ID); err != nil {
			t.Fatalf("version %s should be kept: %v", v.ID, err)
		}
	}
}
