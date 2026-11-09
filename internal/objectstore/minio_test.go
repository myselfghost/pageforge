package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
)

func TestParseEndpoint(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		wantHost   string
		wantSecure bool
		wantErr    bool
	}{
		{name: "http scheme", raw: "http://127.0.0.1:9000", wantHost: "127.0.0.1:9000"},
		{name: "https scheme", raw: "https://s3.example.com", wantHost: "s3.example.com", wantSecure: true},
		{name: "no scheme defaults to https", raw: " minio.local:9000 ", wantHost: "minio.local:9000", wantSecure: true},
		{name: "trailing slash", raw: "http://minio.local:9000/", wantHost: "minio.local:9000"},
		{name: "empty", raw: "  ", wantErr: true},
		{name: "missing host", raw: "http://", wantErr: true},
		{name: "unsupported scheme", raw: "ftp://minio.local:9000", wantErr: true},
		{name: "path not allowed", raw: "http://minio.local:9000/data", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, secure, err := parseEndpoint(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseEndpoint(%q) = %q, %v, want error", tc.raw, host, secure)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseEndpoint(%q): %v", tc.raw, err)
			}
			if host != tc.wantHost {
				t.Errorf("host = %q, want %q", host, tc.wantHost)
			}
			if secure != tc.wantSecure {
				t.Errorf("secure = %v, want %v", secure, tc.wantSecure)
			}
		})
	}
}

func TestMinIOOptions(t *testing.T) {
	creds := Credentials{
		Endpoint:  "http://127.0.0.1:9000",
		Region:    "eu-west-1",
		AccessKey: "key",
		SecretKey: "secret",
		PathStyle: true,
	}
	endpoint, opts, err := minioOptions(creds)
	if err != nil {
		t.Fatalf("minioOptions: %v", err)
	}
	if endpoint != "127.0.0.1:9000" {
		t.Errorf("endpoint = %q, want 127.0.0.1:9000", endpoint)
	}
	if opts.Secure {
		t.Error("secure = true, want false for http endpoint")
	}
	if opts.Region != "eu-west-1" {
		t.Errorf("region = %q, want eu-west-1", opts.Region)
	}
	if opts.BucketLookup != minio.BucketLookupPath {
		t.Errorf("bucket lookup = %v, want path style", opts.BucketLookup)
	}
	if opts.Transport != nil {
		t.Error("transport should be nil when TLS verification is on")
	}

	_, defaultOpts, err := minioOptions(Credentials{Endpoint: "s3.example.com"})
	if err != nil {
		t.Fatalf("minioOptions: %v", err)
	}
	if !defaultOpts.Secure {
		t.Error("secure = false, want true for scheme-less endpoint")
	}
	if defaultOpts.BucketLookup != minio.BucketLookupAuto {
		t.Errorf("bucket lookup = %v, want auto", defaultOpts.BucketLookup)
	}
}

func TestMinIOOptionsTLSSkipVerify(t *testing.T) {
	_, opts, err := minioOptions(Credentials{Endpoint: "https://s3.example.com", TLSSkipVerify: true})
	if err != nil {
		t.Fatalf("minioOptions: %v", err)
	}
	tr, ok := opts.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", opts.Transport)
	}
	if tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("TLSClientConfig.InsecureSkipVerify = false, want true")
	}
	base := http.DefaultTransport.(*http.Transport)
	if tr.MaxIdleConns != base.MaxIdleConns || tr.IdleConnTimeout != base.IdleConnTimeout {
		t.Errorf("connection pool settings not preserved: max idle %d/%d, idle timeout %v/%v",
			tr.MaxIdleConns, base.MaxIdleConns, tr.IdleConnTimeout, base.IdleConnTimeout)
	}
}

func TestMinIOOptionsTimeouts(t *testing.T) {
	_, opts, err := minioOptions(Credentials{
		Endpoint:       "https://s3.example.com",
		ConnectTimeout: 3 * time.Second,
		ReadTimeout:    7 * time.Second,
	})
	if err != nil {
		t.Fatalf("minioOptions: %v", err)
	}
	tr, ok := opts.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", opts.Transport)
	}
	if tr.ResponseHeaderTimeout != 7*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v, want 7s", tr.ResponseHeaderTimeout)
	}
	if tr.DialContext == nil {
		t.Fatal("DialContext is nil, want a dialer with the connect timeout")
	}
	// A connect to a black-hole address must fail within roughly the configured
	// connect timeout (plus small scheduling slack).
	start := time.Now()
	conn, err := tr.DialContext(context.Background(), "tcp", "10.255.255.1:80")
	if err == nil {
		_ = conn.Close()
		t.Fatal("expected dial to fail")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second+2*time.Second {
		t.Errorf("dial took %v, expected ~3s connect timeout", elapsed)
	}
	// Pool settings must be preserved.
	base := http.DefaultTransport.(*http.Transport)
	if tr.MaxIdleConns != base.MaxIdleConns {
		t.Errorf("pool settings not preserved: max idle %d/%d", tr.MaxIdleConns, base.MaxIdleConns)
	}
}

func TestParseRange(t *testing.T) {
	cases := []struct {
		spec      string
		start     int64
		end       int64
		whole     bool
		wantError bool
	}{
		{spec: "bytes=0-1023", start: 0, end: 1023},
		{spec: "bytes=10-", start: 10},
		{spec: "bytes=0-", whole: true},
		{spec: "bytes=-500", start: 0, end: -500},
		{spec: "bytes=0-0", start: 0, end: 0},
		{spec: "0-10", wantError: true},
		{spec: "bytes=", wantError: true},
		{spec: "bytes=abc-def", wantError: true},
		{spec: "bytes=5-2", wantError: true},
		{spec: "bytes=0-5,10-20", wantError: true},
	}
	for _, tc := range cases {
		t.Run(tc.spec, func(t *testing.T) {
			start, end, whole, err := parseRange(tc.spec)
			if tc.wantError {
				if err == nil {
					t.Fatalf("parseRange(%q) = %d, %d, %v, want error", tc.spec, start, end, whole)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRange(%q): %v", tc.spec, err)
			}
			if start != tc.start || end != tc.end || whole != tc.whole {
				t.Errorf("parseRange(%q) = %d, %d, %v, want %d, %d, %v",
					tc.spec, start, end, whole, tc.start, tc.end, tc.whole)
			}
		})
	}
}

func TestMapNotFound(t *testing.T) {
	plain := errors.New("boom")
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil},
		{name: "no such key", err: minio.ErrorResponse{Code: minio.NoSuchKey, StatusCode: http.StatusNotFound}, want: true},
		{name: "no such bucket", err: minio.ErrorResponse{Code: minio.NoSuchBucket, StatusCode: http.StatusNotFound}, want: true},
		{name: "no such version", err: minio.ErrorResponse{Code: minio.NoSuchVersion, StatusCode: http.StatusNotFound}, want: true},
		{name: "http 404", err: minio.ErrorResponse{StatusCode: http.StatusNotFound}, want: true},
		{name: "already wrapped", err: fmt.Errorf("wrapped: %w", ErrNotFound), want: true},
		{name: "access denied", err: minio.ErrorResponse{Code: minio.AccessDenied, StatusCode: http.StatusForbidden}},
		{name: "plain error", err: plain},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapNotFound(tc.err)
			if tc.want {
				if !errors.Is(got, ErrNotFound) {
					t.Fatalf("mapNotFound(%v) = %v, want ErrNotFound", tc.err, got)
				}
				return
			}
			if tc.err == nil {
				if got != nil {
					t.Fatalf("mapNotFound(nil) = %v, want nil", got)
				}
				return
			}
			if got != tc.err {
				t.Fatalf("mapNotFound(%v) = %v, want the original error", tc.err, got)
			}
		})
	}
}

func TestPoolReuseAndVersionSwitch(t *testing.T) {
	pool := NewPool()
	creds := Credentials{Endpoint: "127.0.0.1:9000", AccessKey: "key", SecretKey: "secret"}

	first, err := pool.Get("alias-1", 1, creds)
	if err != nil {
		t.Fatalf("pool.Get: %v", err)
	}
	again, err := pool.Get("alias-1", 1, creds)
	if err != nil {
		t.Fatalf("pool.Get: %v", err)
	}
	if again != first {
		t.Error("same alias and version must reuse the cached store")
	}

	upgraded, err := pool.Get("alias-1", 2, creds)
	if err != nil {
		t.Fatalf("pool.Get: %v", err)
	}
	if upgraded == first {
		t.Error("new credential version must build a new store")
	}

	other, err := pool.Get("alias-2", 1, creds)
	if err != nil {
		t.Fatalf("pool.Get: %v", err)
	}
	if other == first {
		t.Error("different aliases must not share a store")
	}

	pool.Invalidate("alias-1")
	rebuilt, err := pool.Get("alias-1", 2, creds)
	if err != nil {
		t.Fatalf("pool.Get: %v", err)
	}
	if rebuilt == upgraded {
		t.Error("invalidated alias must build a new store")
	}

	pool.Invalidate("unknown-alias")
}

func TestPoolGetError(t *testing.T) {
	pool := NewPool()
	if _, err := pool.Get("alias", 1, Credentials{}); err == nil {
		t.Fatal("pool.Get with empty endpoint should fail")
	}
	store, err := pool.Get("alias", 1, Credentials{Endpoint: "127.0.0.1:9000"})
	if err != nil {
		t.Fatalf("pool.Get after failure: %v", err)
	}
	if store == nil {
		t.Fatal("pool.Get returned a nil store")
	}
}

func TestMinIOIntegration(t *testing.T) {
	store, bucket := integrationStore(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	prefix := fmt.Sprintf("pageforge-test/%d/", time.Now().UnixNano())
	helloKey := prefix + "hello.txt"
	streamKey := prefix + "streamed.txt"
	copyKey := prefix + "copy.txt"
	t.Cleanup(func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer rmCancel()
		for _, key := range []string{helloKey, streamKey, copyKey} {
			if err := store.Remove(rmCtx, bucket, key); err != nil {
				t.Logf("cleanup %s: %v", key, err)
			}
		}
	})

	body := "hello pageforge"
	if err := store.Put(ctx, bucket, helloKey, strings.NewReader(body), int64(len(body)), PutOptions{
		ContentType:  "text/plain",
		CacheControl: "public, max-age=60",
		Metadata:     map[string]string{"origin": "m1-t4"},
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	info, err := store.Stat(ctx, bucket, helloKey)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size != int64(len(body)) {
		t.Errorf("size = %d, want %d", info.Size, len(body))
	}
	if info.ETag == "" || strings.Contains(info.ETag, `"`) {
		t.Errorf("etag = %q, want a non-empty unquoted value", info.ETag)
	}
	if !strings.HasPrefix(info.ContentType, "text/plain") {
		t.Errorf("content type = %q, want text/plain", info.ContentType)
	}
	if info.LastModified.IsZero() {
		t.Error("last modified is zero")
	}

	rc, got, err := store.Get(ctx, bucket, helloKey, GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Size != int64(len(body)) {
		t.Errorf("get size = %d, want %d", got.Size, len(body))
	}
	if text := readBody(t, rc); text != body {
		t.Errorf("get body = %q, want %q", text, body)
	}

	rc, _, err = store.Get(ctx, bucket, helloKey, GetOptions{Range: "bytes=0-4"})
	if err != nil {
		t.Fatalf("get range: %v", err)
	}
	if text := readBody(t, rc); text != "hello" {
		t.Errorf("range body = %q, want %q", text, "hello")
	}

	if err := store.Put(ctx, bucket, streamKey, strings.NewReader("streamed"), -1, PutOptions{
		ContentType: "text/plain",
	}); err != nil {
		t.Fatalf("put with unknown size: %v", err)
	}
	rc, _, err = store.Get(ctx, bucket, streamKey, GetOptions{})
	if err != nil {
		t.Fatalf("get streamed: %v", err)
	}
	if text := readBody(t, rc); text != "streamed" {
		t.Errorf("streamed body = %q, want %q", text, "streamed")
	}

	if err := store.Copy(ctx, bucket, helloKey, bucket, copyKey); err != nil {
		t.Fatalf("copy: %v", err)
	}
	copied, err := store.Stat(ctx, bucket, copyKey)
	if err != nil {
		t.Fatalf("stat copy: %v", err)
	}
	if copied.Size != int64(len(body)) {
		t.Errorf("copied size = %d, want %d", copied.Size, len(body))
	}

	var keys []string
	if err := store.List(ctx, bucket, prefix, func(oi ObjectInfo) error {
		keys = append(keys, oi.Key)
		return nil
	}); err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{helloKey, streamKey, copyKey} {
		if !contains(keys, want) {
			t.Errorf("list keys %v missing %q", keys, want)
		}
	}

	stop := errors.New("stop listing")
	if err := store.List(ctx, bucket, prefix, func(ObjectInfo) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("list callback error = %v, want %v", err, stop)
	}

	if _, err := store.Stat(ctx, bucket, prefix+"missing.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("stat missing = %v, want ErrNotFound", err)
	}
	if _, _, err := store.Get(ctx, bucket, prefix+"missing.txt", GetOptions{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("get missing = %v, want ErrNotFound", err)
	}

	if err := store.Remove(ctx, bucket, helloKey); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := store.Stat(ctx, bucket, helloKey); !errors.Is(err, ErrNotFound) {
		t.Errorf("stat removed = %v, want ErrNotFound", err)
	}
}

func integrationStore(t *testing.T) (*MinIO, string) {
	t.Helper()
	endpoint := os.Getenv("PAGEFORGE_TEST_S3_ENDPOINT")
	accessKey := os.Getenv("PAGEFORGE_TEST_S3_ACCESS_KEY")
	secretKey := os.Getenv("PAGEFORGE_TEST_S3_SECRET_KEY")
	bucket := os.Getenv("PAGEFORGE_TEST_S3_BUCKET")
	if endpoint == "" || accessKey == "" || secretKey == "" || bucket == "" {
		t.Skip("PAGEFORGE_TEST_S3_ENDPOINT, PAGEFORGE_TEST_S3_ACCESS_KEY, PAGEFORGE_TEST_S3_SECRET_KEY and PAGEFORGE_TEST_S3_BUCKET must all be set")
	}
	region := os.Getenv("PAGEFORGE_TEST_S3_REGION")
	if region == "" {
		region = "us-east-1"
	}
	store, err := NewMinIO(Credentials{
		Endpoint:  endpoint,
		Region:    region,
		AccessKey: accessKey,
		SecretKey: secretKey,
		PathStyle: true,
	})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return store, bucket
}

func readBody(t *testing.T, rc io.ReadCloser) string {
	t.Helper()
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(data)
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
