// Package crypto encrypts secrets at rest with AES-256-GCM.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
)

// KeySize is the required AES-256 key length in bytes.
const KeySize = 32

// ErrInvalidKey reports a master key that is not 32 bytes.
var ErrInvalidKey = errors.New("master key must be 32 bytes")

// Cipher encrypts and decrypts small secrets such as S3 credentials.
type Cipher struct {
	aead cipher.AEAD
}

// New builds a Cipher from a raw 32-byte master key.
func New(masterKey []byte) (*Cipher, error) {
	if len(masterKey) != KeySize {
		return nil, fmt.Errorf("%w: got %d bytes", ErrInvalidKey, len(masterKey))
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("new aes cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// NewFromBase64 builds a Cipher from a base64-encoded 32-byte master key.
func NewFromBase64(encoded string) (*Cipher, error) {
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode master key: %w", err)
	}
	return New(key)
}

// FromEnv reads a base64 master key from the named environment variable.
func FromEnv(name string) (*Cipher, error) {
	value := os.Getenv(name)
	if value == "" {
		return nil, fmt.Errorf("environment variable %s is not set", name)
	}
	return NewFromBase64(value)
}

// Encrypt seals plaintext and returns nonce||ciphertext.
func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("read nonce: %w", err)
	}
	return c.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt opens nonce||ciphertext produced by Encrypt.
func (c *Cipher) Decrypt(sealed []byte) ([]byte, error) {
	nonceSize := c.aead.NonceSize()
	if len(sealed) < nonceSize+c.aead.Overhead() {
		return nil, errors.New("ciphertext is too short")
	}
	nonce, payload := sealed[:nonceSize], sealed[nonceSize:]
	plaintext, err := c.aead.Open(nil, nonce, payload, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return plaintext, nil
}

// EncryptString encrypts a string and returns raw bytes.
func (c *Cipher) EncryptString(plaintext string) ([]byte, error) {
	return c.Encrypt([]byte(plaintext))
}

// DecryptString decrypts bytes produced by EncryptString.
func (c *Cipher) DecryptString(sealed []byte) (string, error) {
	plaintext, err := c.Decrypt(sealed)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}
