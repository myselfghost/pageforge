package crypto

import (
	"bytes"
	"encoding/base64"
	"errors"
	"testing"
)

func testKey(b byte) []byte {
	key := make([]byte, KeySize)
	for i := range key {
		key[i] = b
	}
	return key
}

func TestNewRejectsWrongKeySize(t *testing.T) {
	for _, size := range []int{0, 16, 31, 33} {
		if _, err := New(make([]byte, size)); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("New(%d bytes) error = %v, want ErrInvalidKey", size, err)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	c, err := New(testKey(1))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := c.EncryptString("super-secret")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("super-secret")) {
		t.Fatal("ciphertext contains plaintext")
	}
	got, err := c.DecryptString(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if got != "super-secret" {
		t.Fatalf("DecryptString() = %q, want super-secret", got)
	}

	sealed2, err := c.EncryptString("super-secret")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(sealed, sealed2) {
		t.Fatal("encrypting twice produced identical ciphertext; nonce is not random")
	}
}

func TestDecryptFailures(t *testing.T) {
	c, _ := New(testKey(2))
	other, _ := New(testKey(3))
	sealed, err := c.EncryptString("value")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.Decrypt(sealed[:5]); err == nil {
		t.Fatal("Decrypt(short input) = nil error, want error")
	}

	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := c.Decrypt(tampered); err == nil {
		t.Fatal("Decrypt(tampered) = nil error, want error")
	}

	if _, err := other.Decrypt(sealed); err == nil {
		t.Fatal("Decrypt(wrong key) = nil error, want error")
	}
}

func TestNewFromBase64(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(testKey(4))
	if _, err := NewFromBase64(encoded); err != nil {
		t.Fatalf("NewFromBase64() error: %v", err)
	}
	if _, err := NewFromBase64("not-base64!"); err == nil {
		t.Fatal("NewFromBase64(invalid) = nil error, want error")
	}
	if _, err := NewFromBase64(base64.StdEncoding.EncodeToString([]byte("short"))); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("NewFromBase64(short) error = %v, want ErrInvalidKey", err)
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("PAGEFORGE_TEST_MASTER_KEY", base64.StdEncoding.EncodeToString(testKey(5)))
	if _, err := FromEnv("PAGEFORGE_TEST_MASTER_KEY"); err != nil {
		t.Fatalf("FromEnv() error: %v", err)
	}
	if _, err := FromEnv("PAGEFORGE_TEST_MISSING_KEY"); err == nil {
		t.Fatal("FromEnv(missing) = nil error, want error")
	}
}
