package manifest

import (
	"bytes"
	"compress/gzip"
	"io"
	"testing"
)

// TestMaybeGunzip covers the two shapes a manifest object can take across
// S3-compatible backends: transparently-decompressed plain JSON (MinIO via the
// minio-go SDK) and raw gzip bytes (gateways that do not honour
// Content-Encoding). Both must decode to the same payload.
func TestMaybeGunzip(t *testing.T) {
	payload := []byte(`{"index":"index.html","entries":[{"key":"index.html"}]}`)

	gzipped := func() []byte {
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		if _, err := w.Write(payload); err != nil {
			t.Fatalf("gzip write: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("gzip close: %v", err)
		}
		return buf.Bytes()
	}()

	cases := []struct {
		name  string
		input []byte
	}{
		{name: "raw gzip bytes (unhonoured Content-Encoding)", input: gzipped},
		{name: "already decompressed plain json", input: payload},
		{name: "empty body", input: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader, closer, err := maybeGunzip(bytes.NewReader(tc.input), "root.json")
			if err != nil {
				t.Fatalf("maybeGunzip: %v", err)
			}
			defer closer()
			got, err := io.ReadAll(reader)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			switch tc.name {
			case "empty body":
				if len(got) != 0 {
					t.Fatalf("empty body decoded to %q", got)
				}
			default:
				if !bytes.Equal(got, payload) {
					t.Fatalf("decoded %q, want %q", got, payload)
				}
			}
		})
	}
}

// TestMaybeGunzipRejectsInvalidHeader documents that a body whose magic matches
// gzip but is otherwise corrupt still surfaces a decompress error rather than
// silently succeeding.
func TestMaybeGunzipRejectsInvalidHeader(t *testing.T) {
	corrupt := []byte{0x1f, 0x8b, 0x00, 0x01, 0x02}
	_, closer, err := maybeGunzip(bytes.NewReader(corrupt), "root.json")
	if err == nil {
		closer()
		t.Fatal("expected error for corrupt gzip body")
	}
}
