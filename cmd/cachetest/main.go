// Command cachetest verifies the data-plane disk cache against a live
// PageForge deployment: basic fill/hit, byte-budget LRU eviction, TTL idle
// expiry, background sweeping and refill behaviour.
//
// It is designed for repeated one-click runs against the local environment:
//
//	go run ./cmd/cachetest
//
// Flags allow pointing at another deployment. Do not browse the tested site
// while the TTL phase is idling, or the last-access times will be refreshed
// and the assertions will fail.
package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"gopkg.in/yaml.v3"
)

type manifestRoot struct {
	Schema       string            `json:"schema"`
	ProjectID    string            `json:"project_id"`
	VersionID    string            `json:"version_id"`
	DefaultShard string            `json:"default_shard"`
	Shards       map[string]string `json:"shards"`
}

type manifestEntry struct {
	Key  string `json:"key"`
	Size int64  `json:"size"`
}

type manifestShard struct {
	Schema string                   `json:"schema"`
	Files  map[string]manifestEntry `json:"files"`
}

type cachetest struct {
	dataPlane   string
	host        string
	prefix      string
	adminAPI    string
	adminToken  string
	internalAPI string
	internalTok string
	cacheDir    string
	alias       string
	bucket      string
	project     string
	version     string
	budget      int64
	ratio       float64
	ttl         time.Duration
	sweep       time.Duration

	client *http.Client
	fail   int
}

func main() {
	var c cachetest
	var workDir, phases string
	flag.StringVar(&c.dataPlane, "data-plane", "http://127.0.0.1", "data-plane base URL (Caddy)")
	flag.StringVar(&c.host, "host", "yt.com", "Host header of the binding under test")
	flag.StringVar(&c.prefix, "prefix", "/ai-feng", "binding path prefix")
	flag.StringVar(&workDir, "workdir", `E:\soft\rdesk1.5\bin`, "directory holding control.yaml and internal.token")
	flag.StringVar(&c.cacheDir, "cache-dir", "", "cache_dir of the s3static module (default <workdir>/cache)")
	flag.StringVar(&c.alias, "alias", "als_01M6Q4F7YMKVDYT05K549AYZQZ", "alias id")
	flag.StringVar(&c.bucket, "bucket", "page-bucket-2", "bucket name")
	flag.StringVar(&c.project, "project", "prj_01M6SSHFKWZ9TXHCB3R7FJXBTF", "project id")
	flag.StringVar(&c.version, "version", "", "version id (default: project current pointer)")
	flag.Int64Var(&c.budget, "budget", 10240, "cache_max_bytes of the running config")
	flag.Float64Var(&c.ratio, "ratio", 0.6, "cache_evict_ratio of the running config")
	flag.DurationVar(&c.ttl, "ttl", 3*time.Minute, "cache_ttl of the running config")
	flag.DurationVar(&c.sweep, "sweep", 5*time.Minute, "background sweep interval of the cache build")
	flag.StringVar(&phases, "phases", "all", "comma separated phases: hit,lru,ttl or all")
	flag.Parse()

	if c.cacheDir == "" {
		c.cacheDir = filepath.Join(workDir, "cache")
	}
	if err := c.loadSecrets(workDir); err != nil {
		fatalf("load secrets: %v", err)
	}
	c.client = &http.Client{
		Timeout: 30 * time.Second,
		// The cache is filled once per object; redirects must not be followed
		// automatically, or a 301 would look like a cacheable body.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	fmt.Printf("cachetest: host=%s prefix=%s budget=%d ratio=%.2f ttl=%s sweep=%s\n",
		c.host, c.prefix, c.budget, c.ratio, c.ttl, c.sweep)
	fmt.Println("cachetest: do NOT browse the site while the test is running")

	files, err := c.loadManifest()
	if err != nil {
		fatalf("load manifest: %v", err)
	}
	fmt.Printf("manifest: %d files, version=%s\n", len(files), c.version)

	run := map[string]func([]manifestEntry) error{
		"hit": c.phaseHit,
		"lru": c.phaseLRU,
		"ttl": c.phaseTTL,
	}
	order := []string{"hit", "lru", "ttl"}
	if phases != "all" {
		order = strings.Split(phases, ",")
	}
	for _, name := range order {
		name = strings.TrimSpace(name)
		fn, ok := run[name]
		if !ok {
			fatalf("unknown phase %q", name)
		}
		fmt.Printf("\n=== phase %s ===\n", name)
		if err := fn(files); err != nil {
			c.fail++
			fmt.Printf("FAIL(%s): %v\n", name, err)
		} else {
			fmt.Printf("PASS(%s)\n", name)
		}
	}
	fmt.Printf("\ncachetest done: %d failed phase(s)\n", c.fail)
	if c.fail > 0 {
		os.Exit(1)
	}
}

func fatalf(format string, args ...any) {
	fmt.Printf("FATAL: "+format+"\n", args...)
	os.Exit(2)
}

// loadSecrets reads the admin and internal tokens from the live work dir.
func (c *cachetest) loadSecrets(workDir string) error {
	body, err := os.ReadFile(filepath.Join(workDir, "control.yaml"))
	if err != nil {
		return err
	}
	var cfg struct {
		Server struct {
			Listen        string `yaml:"listen"`
			InternalListe string `yaml:"internal_listen"`
			AdminToken    string `yaml:"admin_token"`
		} `yaml:"server"`
	}
	if err := yaml.Unmarshal(body, &cfg); err != nil {
		return err
	}
	c.adminToken = cfg.Server.AdminToken
	c.adminAPI = "http://" + cfg.Server.Listen
	if strings.HasPrefix(cfg.Server.Listen, "0.0.0.0") {
		c.adminAPI = "http://127.0.0.1" + strings.TrimPrefix(cfg.Server.Listen, "0.0.0.0")
	}
	tok, err := os.ReadFile(filepath.Join(workDir, "internal.token"))
	if err != nil {
		return err
	}
	c.internalTok = strings.TrimSpace(string(tok))
	c.internalAPI = "http://" + cfg.Server.InternalListe
	return nil
}

// getJSON performs an authenticated GET and decodes the JSON body.
func (c *cachetest) getJSON(rawURL, token string, out any) error {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GET %s: %s: %s", rawURL, resp.Status, body)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// loadManifest resolves the current version and downloads the manifest files.
func (c *cachetest) loadManifest() ([]manifestEntry, error) {
	if c.version == "" {
		var project struct {
			CurrentVersionID string `json:"current_version_id"`
		}
		if err := c.getJSON(c.adminAPI+"/api/v1/projects/"+c.project, c.adminToken, &project); err != nil {
			return nil, err
		}
		c.version = project.CurrentVersionID
	}
	var creds struct {
		AccessKey string `json:"access_key"`
		SecretKey string `json:"secret_key"`
	}
	if err := c.getJSON(c.internalAPI+"/internal/v1/aliases/"+c.alias+"/credentials", c.internalTok, &creds); err != nil {
		return nil, err
	}
	endpoint := "127.0.0.1:9000"
	mc, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(creds.AccessKey, creds.SecretKey, ""),
		Secure: false,
		Region: "us-east-1",
	})
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	prefix := "_system/manifests/" + c.project + "/" + c.version
	get := func(key string, out any) error {
		obj, err := mc.GetObject(ctx, c.bucket, key, minio.GetObjectOptions{})
		if err != nil {
			return err
		}
		defer obj.Close()
		// Manifest objects are stored gzip-compressed.
		gz, err := gzip.NewReader(obj)
		if err != nil {
			return err
		}
		defer gz.Close()
		return json.NewDecoder(gz).Decode(out)
	}
	var root manifestRoot
	if err := get(prefix+"/root.json", &root); err != nil {
		return nil, fmt.Errorf("read root.json: %w", err)
	}
	var all []manifestEntry
	seen := map[string]bool{}
	for _, rel := range root.Shards {
		if seen[rel] {
			continue
		}
		seen[rel] = true
		var shard manifestShard
		if err := get(prefix+"/"+rel, &shard); err != nil {
			return nil, fmt.Errorf("read shard %s: %w", rel, err)
		}
		for _, e := range shard.Files {
			if e.Size > 0 {
				all = append(all, e)
			}
		}
	}
	if len(all) == 0 {
		return nil, errors.New("manifest has no files")
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Size < all[j].Size })
	return all, nil
}

// fetch requests one manifest key through the data plane and reports the
// X-Cache status and body size. The body is always read to EOF so fills
// commit.
func (c *cachetest) fetch(key string) (status string, n int64, code int, err error) {
	u := (&url.URL{Scheme: "http", Host: mustHost(c.dataPlane), Path: path.Join("/", c.prefix, key)}).String()
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", 0, 0, err
	}
	req.Host = c.host
	resp, err := c.client.Do(req)
	if err != nil {
		return "", 0, 0, err
	}
	defer resp.Body.Close()
	n, _ = io.Copy(io.Discard, resp.Body)
	return resp.Header.Get("X-Cache"), n, resp.StatusCode, nil
}

func mustHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		fatalf("invalid data-plane URL %q", raw)
	}
	return u.Host
}

// cachePath maps a manifest key to the expected on-disk cache file.
func (c *cachetest) cachePath(key string) string {
	return filepath.Join(c.cacheDir, c.alias, c.bucket, c.project, c.version, filepath.FromSlash(key))
}

// dirState returns the number and total size of committed cache files.
func (c *cachetest) dirState() (files map[string]int64, total int64, err error) {
	files = map[string]int64{}
	root := c.cacheDir
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".pf-tmp-") {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return nil
		}
		files[filepath.ToSlash(rel)] = info.Size()
		total += info.Size()
		return nil
	})
	return files, total, err
}

func (c *cachetest) check(cond bool, format string, args ...any) error {
	if cond {
		fmt.Printf("  ok: "+format+"\n", args...)
		return nil
	}
	return fmt.Errorf(format, args...)
}

// phaseHit verifies the basic fill and hit behaviour plus on-disk placement.
func (c *cachetest) phaseHit(files []manifestEntry) error {
	f1 := files[0]
	if f1.Size > c.budget {
		return fmt.Errorf("smallest manifest file %s is %d bytes, over budget %d", f1.Key, f1.Size, c.budget)
	}
	fmt.Printf("target: %s (%d bytes)\n", f1.Key, f1.Size)

	status, n, code, err := c.fetch(f1.Key)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("first GET %s: HTTP %d", f1.Key, code)
	}
	fmt.Printf("first GET: X-Cache=%s (%s)\n", status, map[string]string{"MISS": "fresh fill", "HIT": "already cached from earlier traffic"}[status])
	if status != "MISS" && status != "HIT" {
		return fmt.Errorf("first GET: unexpected X-Cache %q", status)
	}
	if err := c.check(n == f1.Size, "body size %d matches manifest", n); err != nil {
		return err
	}

	status, _, code, err = c.fetch(f1.Key)
	if err != nil {
		return err
	}
	if err := c.check(code == http.StatusOK && status == "HIT", "second GET served from cache (X-Cache=%s)", status); err != nil {
		return err
	}
	if _, err := os.Stat(c.cachePath(f1.Key)); err != nil {
		return fmt.Errorf("cache file missing on disk: %v", err)
	}
	return c.check(true, "cache file exists at %s", c.cachePath(f1.Key))
}

// phaseLRU verifies the byte budget: oversized objects bypass, over-budget
// fills evict least recently used entries down to the low watermark, and an
// evicted object is refilled on the next request.
func (c *cachetest) phaseLRU(files []manifestEntry) error {
	// 1. Oversized objects must never be cached.
	var oversized *manifestEntry
	for i := range files {
		if files[i].Size > c.budget {
			oversized = &files[i]
			break
		}
	}
	if oversized != nil {
		fmt.Printf("oversized target: %s (%d bytes > budget %d)\n", oversized.Key, oversized.Size, c.budget)
		for attempt := 1; attempt <= 2; attempt++ {
			status, _, code, err := c.fetch(oversized.Key)
			if err != nil {
				return err
			}
			if err := c.check(code == http.StatusOK && status == "BYPASS",
				"oversized GET #%d: X-Cache=%s", attempt, status); err != nil {
				return err
			}
		}
		if _, err := os.Stat(c.cachePath(oversized.Key)); !os.IsNotExist(err) {
			return fmt.Errorf("oversized object was written to the cache dir")
		}
		fmt.Println("  ok: oversized object never landed in the cache dir")
	} else {
		fmt.Println("no oversized file in manifest; skipping BYPASS check")
	}

	// 2. Fill well over the budget with cacheable files. Files up to the
	// budget are cacheable; with this manifest the 8192-byte SVGs alternate
	// MISS/BYPASS and force evictions.
	var seq []manifestEntry
	var cumulative int64
	for _, f := range files {
		if f.Size > c.budget {
			continue
		}
		seq = append(seq, f)
		cumulative += f.Size
		if cumulative >= c.budget*3/2 {
			break
		}
	}
	if cumulative <= c.budget {
		return fmt.Errorf("not enough cacheable files to exceed the budget (have %d, need > %d)", cumulative, c.budget)
	}
	fmt.Printf("fill sequence: %d files, %d bytes cumulative (budget %d)\n", len(seq), cumulative, c.budget)
	var bypasses int
	for _, f := range seq {
		status, _, code, err := c.fetch(f.Key)
		if err != nil {
			return err
		}
		if code != http.StatusOK {
			return fmt.Errorf("GET %s: HTTP %d", f.Key, code)
		}
		if status == "BYPASS" {
			bypasses++
		}
		time.Sleep(150 * time.Millisecond) // let the async evictor run
	}
	fmt.Printf("fills done (%d bypassed while the budget was full)\n", bypasses)

	// Eviction runs asynchronously; wait for the dir to settle within budget.
	var total int64
	settle := time.Now().Add(10 * time.Second)
	for {
		var err error
		_, total, err = c.dirState()
		if err != nil {
			return err
		}
		if total <= c.budget {
			break
		}
		if time.Now().After(settle) {
			return fmt.Errorf("cache dir holds %d bytes, still over budget %d after 10s", total, c.budget)
		}
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Printf("  ok: cache dir settled at %d bytes, within budget %d\n", total, c.budget)

	// 3. The least recently used entries must be gone now.
	var evicted []string
	for _, f := range seq {
		if _, err := os.Stat(c.cachePath(f.Key)); os.IsNotExist(err) {
			evicted = append(evicted, f.Key)
		}
	}
	if err := c.check(len(evicted) > 0, "%d of %d filled files were evicted (LRU)", len(evicted), len(seq)); err != nil {
		return err
	}

	// 4. Rebuild: an evicted file must be refilled and hit again.
	victim := evicted[0]
	status, _, _, err := c.fetch(victim)
	if err != nil {
		return err
	}
	if err := c.check(status == "MISS", "evicted %s refetched (X-Cache=%s)", victim, status); err != nil {
		return err
	}
	status, _, _, err = c.fetch(victim)
	if err != nil {
		return err
	}
	return c.check(status == "HIT", "refilled %s served from cache (X-Cache=%s)", victim, status)
}

// phaseTTL verifies idle expiry: after ttl without access a cached object is
// dropped lazily on the next request and refilled; entries that are never
// touched again are removed from disk by the background sweep.
func (c *cachetest) phaseTTL(files []manifestEntry) error {
	// Cache three small files fresh so the sweep has victims.
	var picked []manifestEntry
	for _, f := range files {
		if f.Size > c.budget/3 {
			continue
		}
		picked = append(picked, f)
		if len(picked) == 3 {
			break
		}
	}
	if len(picked) < 2 {
		return errors.New("not enough small files for the TTL phase")
	}
	for _, f := range picked {
		// A prime GET may BYPASS while the dir is full of leftovers; the
		// bypass triggers an eviction, so retry until the file sticks.
		cached := false
		for attempt := 0; attempt < 4 && !cached; attempt++ {
			status, _, code, err := c.fetch(f.Key)
			if err != nil {
				return err
			}
			if code != http.StatusOK {
				return fmt.Errorf("prime GET %s: HTTP %d", f.Key, code)
			}
			cached = status == "HIT"
			time.Sleep(200 * time.Millisecond)
		}
		if !cached {
			return fmt.Errorf("prime GET %s never landed in the cache", f.Key)
		}
	}
	time.Sleep(300 * time.Millisecond)
	s0, total, err := c.dirState()
	if err != nil {
		return err
	}
	fmt.Printf("primed %d cache files (%d bytes): %s\n", len(s0), total, keysOf(s0))
	if err := c.check(len(s0) >= 2, "at least two files cached before idling"); err != nil {
		return err
	}

	victim := picked[0]
	idle := c.ttl + 20*time.Second
	fmt.Printf("idling %s (> ttl %s, no requests)...\n", idle, c.ttl)
	time.Sleep(idle)

	// Lazy expiry: the next access must be a MISS and refill the entry.
	status, _, _, err := c.fetch(victim.Key)
	if err != nil {
		return err
	}
	if err := c.check(status == "MISS", "after %s idle, GET %s: X-Cache=%s (lazy expiry)", idle, victim.Key, status); err != nil {
		return err
	}
	status, _, _, err = c.fetch(victim.Key)
	if err != nil {
		return err
	}
	if err := c.check(status == "HIT", "refilled after expiry, GET %s: X-Cache=%s", victim.Key, status); err != nil {
		return err
	}

	// Background sweep: untouched entries must vanish from disk within one
	// sweep interval, without any further requests.
	fmt.Println("waiting for the background sweep to remove expired files (no requests)...")
	deadline := time.Now().Add(c.sweep + 6*time.Minute)
	for {
		gone := 0
		left := []string{}
		for name := range s0 {
			if name == filepath.ToSlash(mustRel(c, victim.Key)) {
				continue // refilled above; its clock restarted
			}
			if _, err := os.Stat(filepath.Join(c.cacheDir, filepath.FromSlash(name))); os.IsNotExist(err) {
				gone++
			} else {
				left = append(left, name)
			}
		}
		if len(left) == 0 {
			fmt.Printf("  ok: sweep removed all %d expired files without any request\n", gone)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("sweep did not remove expired files within %s: still present: %s", c.sweep, strings.Join(left, ", "))
		}
		time.Sleep(15 * time.Second)
	}
}

func mustRel(c *cachetest, key string) string {
	rel, err := filepath.Rel(c.cacheDir, c.cachePath(key))
	if err != nil {
		fatalf("rel path: %v", err)
	}
	return rel
}

func keysOf(m map[string]int64) string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	if len(names) > 6 {
		return strings.Join(names[:6], ", ") + fmt.Sprintf(", ... (+%d)", len(names)-6)
	}
	return strings.Join(names, ", ")
}
