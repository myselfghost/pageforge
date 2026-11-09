// Package objectstore abstracts the S3-compatible storage used for site content.
package objectstore

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotFound reports a missing bucket, key, or object version.
var ErrNotFound = errors.New("object not found")

// MetadataEncoding is the user-metadata key recording the content encoding of a
// stored object (e.g. "gzip", "br").
//
// PageForge deliberately records the encoding in user metadata rather than the
// S3 Content-Encoding attribute. S3-compatible backends treat Content-Encoding
// inconsistently: the minio-go SDK transparently decompresses objects that carry
// Content-Encoding: gzip, while other gateways return the raw bytes. Depending
// on that attribute therefore makes behaviour vendor-specific. Keeping the body
// byte-identical to what was uploaded (raw, still compressed) and decoding on
// read keeps every backend consistent.
const MetadataEncoding = "pageforge-encoding"

// GetOptions controls a single Get call.
type GetOptions struct {
	Range           string // "bytes=0-1023"
	IfNoneMatch     string
	IfModifiedSince *time.Time
}

// PutOptions carries the object metadata written alongside the body.
type PutOptions struct {
	ContentType string
	// Encoding is the content encoding of the body ("gzip"/"br"), stored as
	// user metadata under MetadataEncoding rather than the S3 Content-Encoding
	// attribute. Empty means the body is stored verbatim.
	Encoding     string
	CacheControl string
	Metadata     map[string]string
}

// ObjectInfo is the storage-independent metadata for one object.
type ObjectInfo struct {
	Key          string
	Size         int64
	ETag         string
	ContentType  string
	LastModified time.Time
}

// ObjectStore is the subset of the S3 API PageForge depends on.
type ObjectStore interface {
	Stat(ctx context.Context, bucket, key string) (ObjectInfo, error)
	Get(ctx context.Context, bucket, key string, opt GetOptions) (io.ReadCloser, ObjectInfo, error)
	Put(ctx context.Context, bucket, key string, r io.Reader, size int64, opt PutOptions) error
	List(ctx context.Context, bucket, prefix string, fn func(ObjectInfo) error) error
	Remove(ctx context.Context, bucket, key string) error
	PresignPut(ctx context.Context, bucket, key string, ttl time.Duration) (string, error)
	Copy(ctx context.Context, srcBucket, srcKey, dstBucket, dstKey string) error
}
