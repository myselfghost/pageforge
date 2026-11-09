package objectstore

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Credentials describes one S3-compatible endpoint.
type Credentials struct {
	Endpoint      string
	Region        string
	AccessKey     string
	SecretKey     string
	PathStyle     bool
	TLSSkipVerify bool
	// ConnectTimeout bounds establishing a TCP/TLS connection to the endpoint
	// (nginx: proxy_connect_timeout). Zero keeps the default transport value.
	ConnectTimeout time.Duration
	// ReadTimeout bounds waiting for the first response byte (headers) after the
	// request is sent (nginx: proxy_read_timeout for the header phase).
	// It does not bound the body transfer, which is governed by upstream idle
	// timeouts at the handler layer.
	ReadTimeout time.Duration
}

// MinIO implements ObjectStore with the MinIO Go client.
type MinIO struct {
	client *minio.Client
}

var _ ObjectStore = (*MinIO)(nil)

// NewMinIO builds a store for creds. Endpoint may include an http or https scheme.
func NewMinIO(creds Credentials) (*MinIO, error) {
	endpoint, opts, err := minioOptions(creds)
	if err != nil {
		return nil, err
	}
	client, err := minio.New(endpoint, opts)
	if err != nil {
		return nil, fmt.Errorf("create minio client for %q: %w", creds.Endpoint, err)
	}
	return &MinIO{client: client}, nil
}

func minioOptions(creds Credentials) (string, *minio.Options, error) {
	endpoint, secure, err := parseEndpoint(creds.Endpoint)
	if err != nil {
		return "", nil, err
	}
	opts := &minio.Options{
		Creds:        credentials.NewStaticV4(creds.AccessKey, creds.SecretKey, ""),
		Secure:       secure,
		Region:       creds.Region,
		BucketLookup: minio.BucketLookupAuto,
	}
	if creds.PathStyle {
		opts.BucketLookup = minio.BucketLookupPath
	}
	// A custom transport is needed when TLS verification is skipped or when
	// connect/read timeouts are configured.
	if creds.TLSSkipVerify || creds.ConnectTimeout > 0 || creds.ReadTimeout > 0 {
		opts.Transport = httpTransport(creds)
	}
	return endpoint, opts, nil
}

// parseEndpoint strips an optional scheme and reports whether TLS is required.
func parseEndpoint(raw string) (string, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false, errors.New("endpoint must not be empty")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false, fmt.Errorf("parse endpoint %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false, fmt.Errorf("endpoint %q must use http or https", raw)
	}
	if u.Host == "" {
		return "", false, fmt.Errorf("endpoint %q has no host", raw)
	}
	if u.Path != "" && u.Path != "/" {
		return "", false, fmt.Errorf("endpoint %q must not contain a path", raw)
	}
	return u.Host, u.Scheme == "https", nil
}

// httpTransport clones the default transport, keeping its pool settings, and
// applies the configured connect/read timeouts plus optional TLS skip-verify.
func httpTransport(creds Credentials) *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if creds.TLSSkipVerify {
		if tr.TLSClientConfig == nil {
			tr.TLSClientConfig = &tls.Config{}
		}
		tr.TLSClientConfig.InsecureSkipVerify = true
	}
	if creds.ConnectTimeout > 0 {
		timeout := creds.ConnectTimeout
		dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
		tr.DialContext = dialer.DialContext
	}
	if creds.ReadTimeout > 0 {
		tr.ResponseHeaderTimeout = creds.ReadTimeout
	}
	return tr
}

// BucketExists reports whether the bucket is reachable with the configured
// credentials. It doubles as a cheap connectivity probe.
func (m *MinIO) BucketExists(ctx context.Context, bucket string) (bool, error) {
	exists, err := m.client.BucketExists(ctx, bucket)
	if err != nil {
		return false, fmt.Errorf("check bucket %q: %w", bucket, mapNotFound(err))
	}
	return exists, nil
}

// EnsureBucket creates the bucket when it does not exist yet.
func (m *MinIO) EnsureBucket(ctx context.Context, bucket string) error {
	exists, err := m.client.BucketExists(ctx, bucket)
	if err != nil {
		return fmt.Errorf("check bucket %q: %w", bucket, mapNotFound(err))
	}
	if exists {
		return nil
	}
	if err := m.client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		return fmt.Errorf("create bucket %q: %w", bucket, mapNotFound(err))
	}
	return nil
}

func (m *MinIO) Stat(ctx context.Context, bucket, key string) (ObjectInfo, error) {
	info, err := m.client.StatObject(ctx, bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return ObjectInfo{}, wrapError("stat", bucket, key, err)
	}
	return objectInfo(info), nil
}

func (m *MinIO) Get(ctx context.Context, bucket, key string, opt GetOptions) (io.ReadCloser, ObjectInfo, error) {
	var opts minio.GetObjectOptions
	if err := applyGetOptions(&opts, opt); err != nil {
		return nil, ObjectInfo{}, fmt.Errorf("get %s/%s: %w", bucket, key, err)
	}
	info, err := m.Stat(ctx, bucket, key)
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	obj, err := m.client.GetObject(ctx, bucket, key, opts)
	if err != nil {
		return nil, ObjectInfo{}, wrapError("get", bucket, key, err)
	}
	return obj, info, nil
}

// applyGetOptions maps the generic options onto the minio client options.
func applyGetOptions(o *minio.GetObjectOptions, opt GetOptions) error {
	if opt.Range != "" {
		start, end, whole, err := parseRange(opt.Range)
		if err != nil {
			return err
		}
		// An open ended "bytes=0-" is the same as no range at all.
		if !whole {
			if err := o.SetRange(start, end); err != nil {
				return fmt.Errorf("range %q: %w", opt.Range, err)
			}
		}
	}
	if opt.IfNoneMatch != "" {
		etag := strings.Trim(strings.TrimSpace(opt.IfNoneMatch), `"`)
		if etag == "*" {
			o.Set("If-None-Match", "*")
		} else if err := o.SetMatchETagExcept(etag); err != nil {
			return fmt.Errorf("if-none-match: %w", err)
		}
	}
	if opt.IfModifiedSince != nil {
		if err := o.SetModified(*opt.IfModifiedSince); err != nil {
			return fmt.Errorf("if-modified-since: %w", err)
		}
	}
	return nil
}

// parseRange splits an HTTP Range header into minio start/end offsets.
// whole reports that the request covers the entire object.
func parseRange(spec string) (start, end int64, whole bool, err error) {
	spec = strings.TrimSpace(spec)
	if !strings.HasPrefix(spec, "bytes=") {
		return 0, 0, false, fmt.Errorf("invalid range %q", spec)
	}
	spec = strings.TrimPrefix(spec, "bytes=")
	if spec == "" || strings.Contains(spec, ",") {
		return 0, 0, false, fmt.Errorf("invalid range %q", spec)
	}
	startStr, endStr, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, false, fmt.Errorf("invalid range %q", spec)
	}
	if startStr == "" {
		n, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false, fmt.Errorf("invalid range %q", spec)
		}
		return 0, -n, false, nil
	}
	start, err = strconv.ParseInt(startStr, 10, 64)
	if err != nil || start < 0 {
		return 0, 0, false, fmt.Errorf("invalid range %q", spec)
	}
	if endStr == "" {
		return start, 0, start == 0, nil
	}
	end, err = strconv.ParseInt(endStr, 10, 64)
	if err != nil || end < start {
		return 0, 0, false, fmt.Errorf("invalid range %q", spec)
	}
	return start, end, false, nil
}

func (m *MinIO) Put(ctx context.Context, bucket, key string, r io.Reader, size int64, opt PutOptions) error {
	_, err := m.client.PutObject(ctx, bucket, key, r, size, minio.PutObjectOptions{
		ContentType:  opt.ContentType,
		CacheControl: opt.CacheControl,
		UserMetadata: putUserMetadata(opt),
	})
	if err != nil {
		return wrapError("put", bucket, key, err)
	}
	return nil
}

// putUserMetadata returns the user metadata for an object, folding the content
// encoding into it. The encoding is recorded under MetadataEncoding rather than
// the S3 Content-Encoding attribute so no backend rewrites or transparently
// decompresses the stored bytes. See MetadataEncoding.
func putUserMetadata(opt PutOptions) map[string]string {
	if opt.Encoding == "" {
		return opt.Metadata
	}
	meta := make(map[string]string, len(opt.Metadata)+1)
	for k, v := range opt.Metadata {
		meta[k] = v
	}
	meta[MetadataEncoding] = opt.Encoding
	return meta
}

func (m *MinIO) List(ctx context.Context, bucket, prefix string, fn func(ObjectInfo) error) error {
	for obj := range m.client.ListObjects(ctx, bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return wrapError("list", bucket, prefix, obj.Err)
		}
		if err := fn(objectInfo(obj)); err != nil {
			return err
		}
	}
	return nil
}

func (m *MinIO) Remove(ctx context.Context, bucket, key string) error {
	if err := m.client.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return wrapError("remove", bucket, key, err)
	}
	return nil
}

func (m *MinIO) PresignPut(ctx context.Context, bucket, key string, ttl time.Duration) (string, error) {
	u, err := m.client.PresignedPutObject(ctx, bucket, key, ttl)
	if err != nil {
		return "", wrapError("presign put", bucket, key, err)
	}
	return u.String(), nil
}

func (m *MinIO) Copy(ctx context.Context, srcBucket, srcKey, dstBucket, dstKey string) error {
	_, err := m.client.CopyObject(ctx,
		minio.CopyDestOptions{Bucket: dstBucket, Object: dstKey},
		minio.CopySrcOptions{Bucket: srcBucket, Object: srcKey},
	)
	if err != nil {
		return fmt.Errorf("copy %s/%s to %s/%s: %w",
			srcBucket, srcKey, dstBucket, dstKey, mapNotFound(err))
	}
	return nil
}

func objectInfo(info minio.ObjectInfo) ObjectInfo {
	return ObjectInfo{
		Key:          info.Key,
		Size:         info.Size,
		ETag:         strings.Trim(info.ETag, `"`),
		ContentType:  info.ContentType,
		LastModified: info.LastModified,
	}
}

// wrapError adds operation context and maps missing objects to ErrNotFound.
func wrapError(op, bucket, key string, err error) error {
	return fmt.Errorf("%s %s/%s: %w", op, bucket, key, mapNotFound(err))
}

// mapNotFound converts S3 not-found responses into ErrNotFound.
func mapNotFound(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrNotFound) {
		return ErrNotFound
	}
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		if resp.StatusCode == http.StatusNotFound ||
			resp.Code == minio.NoSuchKey ||
			resp.Code == minio.NoSuchBucket ||
			resp.Code == minio.NoSuchVersion {
			return ErrNotFound
		}
	}
	return err
}
