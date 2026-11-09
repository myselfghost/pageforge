// Package e2e provides reusable integration tests that exercise the PageForge
// control plane end-to-end against any S3-compatible object store.
//
// Run the suite against a real object store by pointing the test at its URL
// via environment variables (see cmd/e2e/main.go) or invoke it with
// objectstore.NewMemoryStore() for a fully self-contained smoke test.
package e2e

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pageforge/pageforge/internal/alias"
	"github.com/pageforge/pageforge/internal/api"
	"github.com/pageforge/pageforge/internal/authpolicy"
	"github.com/pageforge/pageforge/internal/binding"
	"github.com/pageforge/pageforge/internal/config"
	"github.com/pageforge/pageforge/internal/deploy"
	"github.com/pageforge/pageforge/internal/internalapi"
	"github.com/pageforge/pageforge/internal/objectstore"
	"github.com/pageforge/pageforge/internal/project"
	"github.com/pageforge/pageforge/internal/share"
	"github.com/pageforge/pageforge/internal/snapshot"
	"github.com/pageforge/pageforge/internal/store"
	"github.com/pageforge/pageforge/internal/testutil"
	"github.com/pageforge/pageforge/internal/version"
)

// Suite orchestrates a control plane and exercises it through both management
// API and the internal snapshot API. Each scenario is idempotent so the
// suite can be re-run safely.
type Suite struct {
	// ObjectStoreFactory overrides the store selection. When nil, Config (or
	// ConfigFromEnv) is used, falling back to an in-memory store.
	ObjectStoreFactory func() (objectstore.ObjectStore, string)
	DatabaseFactory    func() (*store.Store, error)
	// Config points the suite at a real S3-compatible store.
	Config *Config
	// ControlPlane, when set, reuses an already-started harness.
	ControlPlane *ControlPlane
}

// Config describes a real S3-compatible object store used by the suite.
// Endpoint may be repeated to allow probing several hosts; the first that
// responds with the expected credentials wins.
type Config struct {
	Endpoints []string
	AccessKey string
	SecretKey string
	Bucket    string
	Region    string
	PathStyle bool
}

// ConfigFromEnv reads the PAGEFORGE_E2E_* variables. It returns nil when no
// endpoint is configured, which selects the in-memory default.
//
//	PAGEFORGE_E2E_ENDPOINT    comma-separated http(s) base URLs
//	PAGEFORGE_E2E_ACCESS_KEY  access key id
//	PAGEFORGE_E2E_SECRET_KEY  secret access key
//	PAGEFORGE_E2E_BUCKET      bucket name (created when missing)
//	PAGEFORGE_E2E_REGION      region (default us-east-1)
//	PAGEFORGE_E2E_PATH_STYLE  "false" to disable path-style addressing
func ConfigFromEnv() *Config {
	raw := strings.TrimSpace(os.Getenv("PAGEFORGE_E2E_ENDPOINT"))
	if raw == "" {
		return nil
	}
	cfg := &Config{
		AccessKey: strings.TrimSpace(os.Getenv("PAGEFORGE_E2E_ACCESS_KEY")),
		SecretKey: strings.TrimSpace(os.Getenv("PAGEFORGE_E2E_SECRET_KEY")),
		Bucket:    strings.TrimSpace(os.Getenv("PAGEFORGE_E2E_BUCKET")),
		Region:    strings.TrimSpace(os.Getenv("PAGEFORGE_E2E_REGION")),
		PathStyle: true,
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if v := strings.TrimSpace(os.Getenv("PAGEFORGE_E2E_PATH_STYLE")); v != "" {
		cfg.PathStyle = v != "false" && v != "0"
	}
	for _, part := range strings.Split(raw, ",") {
		if ep := strings.TrimSpace(part); ep != "" {
			cfg.Endpoints = append(cfg.Endpoints, ep)
		}
	}
	return cfg
}

// AliasSeed holds the credentials embedded in created aliases. Scenarios do
// not fetch objects through the alias (the data plane reuses the suite's
// store), but the values are kept realistic for real-store runs.
type AliasSeed struct {
	Endpoint  string
	Region    string
	AccessKey string
	SecretKey string
	PathStyle bool
}

func defaultAliasSeed() AliasSeed {
	return AliasSeed{
		Endpoint:  "https://minio.example.com",
		Region:    "us-east-1",
		AccessKey: "AKIA",
		SecretKey: "secret",
		PathStyle: true,
	}
}

// ControlPlane wraps the live HTTP server started by Run.
type ControlPlane struct {
	AdminURL    string
	InternalURL string
	AdminToken  string
	Store       objectstore.ObjectStore
	Bucket      string
	Alias       AliasSeed
	DB          *store.Store
}

// Run boots the harness and invokes every registered scenario in order.
func (s *Suite) Run(t *testing.T, scenarios ...Scenario) {
	t.Helper()
	cp := s.ControlPlane
	if cp == nil {
		var err error
		cp, err = s.startControl(t)
		if err != nil {
			t.Fatalf("start control plane: %v", err)
		}
		defer cp.Close()
	}
	for i, scn := range scenarios {
		name := fmt.Sprintf("%d-%s", i+1, scn.Name)
		t.Run(name, func(t *testing.T) {
			scn.Run(t, cp)
		})
	}
}

func (s *Suite) startControl(t *testing.T) (*ControlPlane, error) {
	t.Helper()
	aliasSeed := defaultAliasSeed()
	var objects objectstore.ObjectStore
	var bucket string

	switch {
	case s.ObjectStoreFactory != nil:
		objects, bucket = s.ObjectStoreFactory()
	default:
		cfg := s.Config
		if cfg == nil {
			cfg = ConfigFromEnv()
		}
		if cfg != nil {
			st, b, endpoint, err := openRealStore(cfg)
			if err != nil {
				return nil, err
			}
			objects, bucket = st, b
			aliasSeed = AliasSeed{
				Endpoint:  endpoint,
				Region:    cfg.Region,
				AccessKey: cfg.AccessKey,
				SecretKey: cfg.SecretKey,
				PathStyle: cfg.PathStyle,
			}
			t.Logf("e2e: using real object store %s bucket=%s", endpoint, b)
		} else {
			objects, bucket = newInMemoryStore(), "test-bucket"
			t.Logf("e2e: using in-memory object store bucket=%s", bucket)
		}
	}

	if s.DatabaseFactory == nil {
		s.DatabaseFactory = func() (*store.Store, error) {
			return store.Open(context.Background(), store.SQLite, ":memory:")
		}
	}
	db, err := s.DatabaseFactory()
	if err != nil {
		return nil, err
	}
	cfg := config.Default()
	cfg.Server.AdminToken = "admin-secret"
	cfg.Server.InternalToken = "internal-secret"
	cfg.Server.SnapshotWarnBytes = 32 << 20

	cipher := testutil.NewCipher(t)
	if _, err := db.Up(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	aliases := alias.NewRepo(db, cipher)
	versions := version.NewRepo(db)
	projects := project.NewRepo(db)
	bindings := binding.NewRepo(db, cfg.Server.ReservedPrefix)
	shares := share.NewRepo(db)

	deployments := deploy.NewService(db, deploy.Options{
		Config: deploy.Config{
			TempDir:         t.TempDir(),
			MaxPackageBytes: 256 << 20,
			Workers:         2,
			MaxFiles:        10_000,
		},
		Stores: func(context.Context, project.Project) (objectstore.ObjectStore, error) {
			return objects, nil
		},
	})

	snapGen := snapshot.NewGenerator(db, cfg.Server.SnapshotWarnBytes, nil)
	authPolicies := authpolicy.NewRepo(db)
	adminHTTP := httptest.NewServer((&api.Server{
		Aliases:      aliases,
		Projects:     projects,
		Bindings:     bindings,
		AuthPolicies: authPolicies,
		Versions:     versions,
		Shares:       shares,
		Deployments:  deployments,
		AdminToken:   cfg.Server.AdminToken,
		Stores: func(context.Context, project.Project) (objectstore.ObjectStore, error) {
			return objects, nil
		},
		PackagePrefix: cfg.Storage.PackagePrefix,
	}).Handler())
	internalHTTP := httptest.NewServer((&internalapi.Server{
		Token:        cfg.Server.InternalToken,
		Snapshots:    snapGen,
		Aliases:      aliases,
		Shares:       shares,
		AuthPolicies: authPolicies,
	}).Handler())
	t.Cleanup(adminHTTP.Close)
	t.Cleanup(internalHTTP.Close)

	return &ControlPlane{
		AdminURL:    adminHTTP.URL,
		InternalURL: internalHTTP.URL,
		AdminToken:  cfg.Server.AdminToken,
		Store:       objects,
		Bucket:      bucket,
		Alias:       aliasSeed,
		DB:          db,
	}, nil
}

// openRealStore connects to the first endpoint that answers with the supplied
// credentials and makes sure the target bucket exists.
func openRealStore(cfg *Config) (objectstore.ObjectStore, string, string, error) {
	if len(cfg.Endpoints) == 0 {
		return nil, "", "", fmt.Errorf("real store: no endpoints configured")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, "", "", fmt.Errorf("real store: PAGEFORGE_E2E_ACCESS_KEY and PAGEFORGE_E2E_SECRET_KEY are required")
	}
	if cfg.Bucket == "" {
		return nil, "", "", fmt.Errorf("real store: PAGEFORGE_E2E_BUCKET is required")
	}

	var lastErr error
	for _, endpoint := range cfg.Endpoints {
		st, err := objectstore.NewMinIO(objectstore.Credentials{
			Endpoint:  endpoint,
			Region:    cfg.Region,
			AccessKey: cfg.AccessKey,
			SecretKey: cfg.SecretKey,
			PathStyle: cfg.PathStyle,
		})
		if err != nil {
			lastErr = err
			continue
		}

		probeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		exists, err := st.BucketExists(probeCtx, cfg.Bucket)
		cancel()
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", endpoint, err)
			continue
		}
		if !exists {
			makeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			err = st.EnsureBucket(makeCtx, cfg.Bucket)
			cancel()
			if err != nil {
				lastErr = fmt.Errorf("%s: %w", endpoint, err)
				continue
			}
		}
		return st, cfg.Bucket, endpoint, nil
	}
	return nil, "", "", fmt.Errorf("real store: no endpoint reachable (%s): %w", strings.Join(cfg.Endpoints, ", "), lastErr)
}

// Close releases control plane resources.
func (cp *ControlPlane) Close() {
	if cp.DB != nil {
		_ = cp.DB.Close()
	}
}

// Scenario is one end-to-end case.
type Scenario struct {
	Name string
	Run  func(t *testing.T, cp *ControlPlane)
}

// ----- helpers -----

func newID(prefix string) string {
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	return prefix + hex.EncodeToString(buf[:])
}

// zip builds an in-memory archive.
func zipBuild(opts func(*zipBuilder)) []byte {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	z := &zipBuilder{w: w}
	opts(z)
	if err := w.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

type zipBuilder struct {
	buf bytes.Buffer
	w   *zip.Writer
}

func (z *zipBuilder) file(name, body string) {
	f, err := z.w.Create(name)
	if err != nil {
		panic(err)
	}
	if _, err := f.Write([]byte(body)); err != nil {
		panic(err)
	}
}

// siteZip packs a minimal static site used across multiple scenarios.
// Assets may override the default index.html and 404.html.
func siteZip(spa bool, assets map[string]string) []byte {
	return zipBuild(func(z *zipBuilder) {
		if _, ok := assets["index.html"]; !ok {
			z.file("index.html", "<h1>hello</h1>")
		}
		if _, ok := assets["404.html"]; !ok {
			z.file("404.html", "<h1>not found</h1>")
		}
		if spa {
			z.file("pageforge.json", `{"spa":{"enabled":true}}`)
		}
		for name, body := range assets {
			z.file(name, body)
		}
	})
}

// client is a thin HTTP helper with a base URL + bearer token.
type client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func newClient(baseURL, token string) *client {
	return &client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

func (c *client) doJSON(t *testing.T, method, path string, payload any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, c.BaseURL+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, body
}

// doRaw performs a request and returns the response headers plus body.
func (c *client) doRaw(t *testing.T, method, path string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, c.BaseURL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, body
}

func (c *client) upload(t *testing.T, path string, body []byte) (int, []byte) {
	t.Helper()
	url := c.BaseURL + path
	t.Logf("upload PUT %s (%d bytes)", url, len(body))
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Content-Type", "application/zip")
	req.ContentLength = int64(len(body))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, respBody
}

func decode[T any](t *testing.T, data []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("decode %s: %v", string(data), err)
	}
	return v
}

func requireStatus(t *testing.T, got, want int, body []byte) {
	t.Helper()
	if got != want {
		t.Fatalf("status = %d, want %d (body: %s)", got, want, string(body))
	}
}

func requireErrorCode(t *testing.T, body []byte, code string) {
	t.Helper()
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode error envelope: %v (body: %s)", err, string(body))
	}
	if envelope.Error.Code != code {
		t.Fatalf("error code = %q, want %q (body: %s)", envelope.Error.Code, code, string(body))
	}
}

// waitForSnapshot polls /internal/v1/snapshot until generation > 0 or timeout.
func waitForSnapshot(internalURL, internalToken string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, strings.TrimRight(internalURL, "/")+"/internal/v1/snapshot", nil)
		if err != nil {
			return err
		}
		if internalToken != "" {
			req.Header.Set("Authorization", "Bearer "+internalToken)
		}
		resp, err := client.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var snap snapshot.Snapshot
			if json.Unmarshal(body, &snap) == nil && snap.Generation > 0 {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("snapshot generation did not advance within %s", timeout)
}

// forceReload triggers an immediate snapshot refresh.
func forceReload(internalURL, internalToken string) error {
	status, _ := newClient(internalURL, internalToken).doJSON(nil, http.MethodPost, "/internal/v1/reload", nil)
	if status != http.StatusOK {
		return fmt.Errorf("reload returned %d", status)
	}
	return nil
}

// ----- scenario primitives: create alias/project/binding/deploy -----

// bootstrapAliases ensures a usable alias exists, returning its id.
func bootstrapAlias(t *testing.T, cp *ControlPlane) string {
	t.Helper()
	admin := newClient(cp.AdminURL, cp.AdminToken)
	name := "alias-" + newID("")
	seed := cp.Alias
	if seed.Endpoint == "" {
		seed = defaultAliasSeed()
	}
	input := map[string]any{
		"name":             name,
		"endpoint":         seed.Endpoint,
		"region":           seed.Region,
		"access_key":       seed.AccessKey,
		"secret_key":       seed.SecretKey,
		"path_style":       seed.PathStyle,
		"bucket_allowlist": []string{cp.Bucket},
	}
	status, body := admin.doJSON(t, http.MethodPost, "/api/v1/aliases", input)
	if status != http.StatusCreated {
		t.Fatalf("create alias: status=%d body=%s", status, string(body))
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode alias: %v", err)
	}
	return created.ID
}

func bootstrapProject(t *testing.T, cp *ControlPlane, aliasID, name, bucket string, opts ...map[string]any) string {
	t.Helper()
	admin := newClient(cp.AdminURL, cp.AdminToken)
	input := map[string]any{
		"name":              name,
		"alias_id":          aliasID,
		"bucket":            bucket,
		"index_doc":         "index.html",
		"error_doc":         "404.html",
		"try_files_enabled": true,
	}
	for _, patch := range opts {
		for k, v := range patch {
			input[k] = v
		}
	}
	status, body := admin.doJSON(t, http.MethodPost, "/api/v1/projects", input)
	if status != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", status, string(body))
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode project: %v", err)
	}
	return created.ID
}

func bootstrapBinding(t *testing.T, cp *ControlPlane, projectID, host, prefix string) string {
	t.Helper()
	admin := newClient(cp.AdminURL, cp.AdminToken)
	input := map[string]any{
		"host":        host,
		"path_prefix": prefix,
		"project_id":  projectID,
		"enabled":     true,
	}
	status, body := admin.doJSON(t, http.MethodPost, "/api/v1/bindings", input)
	if status != http.StatusCreated {
		t.Fatalf("create binding: status=%d body=%s", status, string(body))
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode binding: %v", err)
	}
	return created.ID
}

func bootstrapDeployment(t *testing.T, cp *ControlPlane, projectID string, zipBytes []byte) (deploymentID, versionID string) {
	t.Helper()
	admin := newClient(cp.AdminURL, cp.AdminToken)

	status, body := admin.doJSON(t, http.MethodPost, "/api/v1/deployments", map[string]any{
		"project_id": projectID,
	})
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("create deployment: status=%d body=%s", status, string(body))
	}
	var dep struct {
		DeploymentID string `json:"deployment_id"`
	}
	if err := json.Unmarshal(body, &dep); err != nil {
		t.Fatalf("decode deployment: %v (body=%s)", err, string(body))
	}
	deploymentID = dep.DeploymentID

	status, body = admin.upload(t, "/api/v1/deployments/"+deploymentID+"/package", zipBytes)
	if status != http.StatusOK {
		t.Fatalf("upload package: status=%d body=%s", status, string(body))
	}

	status, body = admin.doJSON(t, http.MethodPost, "/api/v1/deployments/"+deploymentID+"/complete", nil)
	if status != http.StatusOK {
		t.Fatalf("complete deployment: status=%d body=%s", status, string(body))
	}
	var result struct {
		VersionID string `json:"version_id"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("decode complete: %v", err)
	}
	if result.Status != "ready" {
		t.Fatalf("deployment status = %q, want ready", result.Status)
	}
	return deploymentID, result.VersionID
}

// snapshotLoad returns the current snapshot or blocks a short while.
func snapshotLoad(t *testing.T, internalURL, internalToken string) *snapshot.Snapshot {
	t.Helper()
	client := newClient(internalURL, internalToken)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, body := client.doJSON(t, http.MethodGet, "/internal/v1/snapshot", nil)
		if status == http.StatusOK {
			var snap snapshot.Snapshot
			if err := json.Unmarshal(body, &snap); err == nil && len(snap.Projects) > 0 {
				return &snap
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("snapshot never became ready")
	return nil
}

// inMemoryStore is a minimal objectstore used by the in-process harness.
type inMemoryStore struct {
	objects map[string]stored
}

type stored struct {
	data []byte
	info objectstore.ObjectInfo
}

func newInMemoryStore() *inMemoryStore {
	return &inMemoryStore{objects: map[string]stored{}}
}

func idOf(bucket, key string) string { return bucket + "\x00" + key }

func (s *inMemoryStore) Stat(_ context.Context, bucket, key string) (objectstore.ObjectInfo, error) {
	st, ok := s.objects[idOf(bucket, key)]
	if !ok {
		return objectstore.ObjectInfo{}, objectstore.ErrNotFound
	}
	return st.info, nil
}

func (s *inMemoryStore) Get(_ context.Context, bucket, key string, opt objectstore.GetOptions) (io.ReadCloser, objectstore.ObjectInfo, error) {
	st, ok := s.objects[idOf(bucket, key)]
	if !ok {
		return nil, objectstore.ObjectInfo{}, objectstore.ErrNotFound
	}
	data := st.data
	if opt.Range != "" {
		// parse "bytes=start-end"
		r := strings.TrimPrefix(opt.Range, "bytes=")
		parts := strings.SplitN(r, "-", 2)
		if len(parts) == 2 {
			start, err1 := strconv.ParseInt(parts[0], 10, 64)
			end, err2 := strconv.ParseInt(parts[1], 10, 64)
			if err1 == nil && err2 == nil && start >= 0 && end >= start && start < int64(len(data)) {
				if end >= int64(len(data)) {
					end = int64(len(data)) - 1
				}
				data = data[start : end+1]
			}
		}
	}
	return io.NopCloser(bytes.NewReader(data)), st.info, nil
}

func (s *inMemoryStore) Put(_ context.Context, bucket, key string, r io.Reader, _ int64, opt objectstore.PutOptions) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	info := objectstore.ObjectInfo{Key: key, Size: int64(len(data)), ContentType: opt.ContentType}
	s.objects[idOf(bucket, key)] = stored{data: data, info: info}
	return nil
}

func (s *inMemoryStore) List(_ context.Context, bucket, prefix string, fn func(objectstore.ObjectInfo) error) error {
	for id, st := range s.objects {
		if strings.HasPrefix(id, bucket+"\x00"+prefix) {
			if err := fn(st.info); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *inMemoryStore) Remove(_ context.Context, bucket, key string) error {
	delete(s.objects, idOf(bucket, key))
	return nil
}

func (s *inMemoryStore) PresignPut(context.Context, string, string, time.Duration) (string, error) {
	return "", nil
}

func (s *inMemoryStore) Copy(context.Context, string, string, string, string) error { return nil }
