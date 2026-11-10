package objectstore

import (
	"testing"
)

// TestPutUserMetadata is a cross-backend regression guard.
//
// The encoding of a pre-compressed object must be recorded in user metadata
// (MetadataEncoding), never in the S3 Content-Encoding attribute. Backends
// disagree on Content-Encoding: the minio-go SDK transparently decompresses
// objects that carry "gzip", while others return the raw bytes. Recording it as
// Content-Encoding therefore produces vendor-specific behaviour; user metadata
// keeps the stored bytes identical everywhere.
func TestPutUserMetadata(t *testing.T) {
	t.Run("encoding folded into user metadata", func(t *testing.T) {
		meta := putUserMetadata(PutOptions{ContentType: "text/javascript", Encoding: "gzip"})
		if got := meta[MetadataEncoding]; got != "gzip" {
			t.Fatalf("%s = %q, want gzip", MetadataEncoding, got)
		}
	})

	t.Run("encoding merges with existing metadata", func(t *testing.T) {
		meta := putUserMetadata(PutOptions{Encoding: "br", Metadata: map[string]string{"x": "1"}})
		if meta["x"] != "1" || meta[MetadataEncoding] != "br" {
			t.Fatalf("metadata = %v", meta)
		}
	})

	t.Run("no encoding leaves metadata untouched", func(t *testing.T) {
		original := map[string]string{"x": "1"}
		meta := putUserMetadata(PutOptions{Metadata: original})
		if _, ok := meta[MetadataEncoding]; ok {
			t.Fatalf("unexpected encoding metadata for plain object")
		}
		if len(meta) != 1 || meta["x"] != "1" {
			t.Fatalf("metadata = %v", meta)
		}
	})
}
