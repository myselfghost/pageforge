package credential_test

import (
	"context"
	"testing"

	"github.com/pageforge/pageforge/internal/credential"
	"github.com/pageforge/pageforge/internal/crypto"
	"github.com/pageforge/pageforge/internal/snapshot"
	"github.com/pageforge/pageforge/internal/testutil"
)

// sealed 返回与数据库一致的密文，模拟控制面下发的 secret_cipher。
func sealed(t *testing.T, cipher *crypto.Cipher, secret string) []byte {
	t.Helper()
	body, err := cipher.EncryptString(secret)
	if err != nil {
		t.Fatalf("seal secret: %v", err)
	}
	return body
}

func TestClientDecryptsMirroredCiphertext(t *testing.T) {
	cipher := testutil.NewCipher(t)
	store := snapshot.NewStore()
	store.ReplaceCredentials(&snapshot.CredentialDirectory{Aliases: map[string]snapshot.AliasCredentials{
		"ali_1": {AccessKey: "AKIA", SecretCipher: sealed(t, cipher, "secret"), CredentialVer: 1},
	}})

	client := credential.NewClient(store, cipher)
	view := snapshot.AliasView{Endpoint: "https://s3.example.com", Region: "us-east-1", CredentialVer: 1}
	ctx := context.Background()

	first, err := client.Store(ctx, "ali_1", view)
	if err != nil {
		t.Fatalf("first store: %v", err)
	}
	second, err := client.Store(ctx, "ali_1", view)
	if err != nil {
		t.Fatalf("second store: %v", err)
	}
	if first != second {
		t.Fatal("store should be reused for the same credential version")
	}

	// A newer directory version replaces the object store, since the key pair
	// changed.
	store.ReplaceCredentials(&snapshot.CredentialDirectory{Aliases: map[string]snapshot.AliasCredentials{
		"ali_1": {AccessKey: "AKIA", SecretCipher: sealed(t, cipher, "rotated"), CredentialVer: 2},
	}})
	view.CredentialVer = 2
	third, err := client.Store(ctx, "ali_1", view)
	if err != nil {
		t.Fatalf("third store: %v", err)
	}
	if third == first {
		t.Fatal("a rotated credential version must build a new store")
	}
}

func TestClientRejectsStaleAndMissingCredentials(t *testing.T) {
	cipher := testutil.NewCipher(t)
	store := snapshot.NewStore()
	store.ReplaceCredentials(&snapshot.CredentialDirectory{Aliases: map[string]snapshot.AliasCredentials{
		"ali_1": {AccessKey: "AKIA", SecretCipher: sealed(t, cipher, "secret"), CredentialVer: 1},
	}})
	client := credential.NewClient(store, cipher)
	ctx := context.Background()

	// The snapshot demands a version the directory does not carry yet.
	if _, err := client.Store(ctx, "ali_1", snapshot.AliasView{
		Endpoint: "https://s3.example.com", CredentialVer: 2,
	}); err == nil {
		t.Fatal("credentials older than the snapshot version must be rejected")
	}
	if _, err := client.Store(ctx, "ali_missing", snapshot.AliasView{
		Endpoint: "https://s3.example.com", CredentialVer: 1,
	}); err == nil {
		t.Fatal("an unknown alias must fail")
	}
	if client.Healthy("ali_1", 1) != true || client.Healthy("ali_1", 2) != false {
		t.Fatal("Healthy must mirror the version check")
	}
}

func TestClientWithoutDirectoryOrKeyFailsAndNeverCallsOut(t *testing.T) {
	if _, err := credential.NewClient(nil, testutil.NewCipher(t)).Store(context.Background(), "ali_1",
		snapshot.AliasView{Endpoint: "https://s3.example.com", CredentialVer: 1}); err == nil {
		t.Fatal("a client without a directory must fail rather than reach the network")
	}

	// Without the master key the sealed secret cannot be opened, so the request
	// must fail instead of signing with garbage.
	store := snapshot.NewStore()
	store.ReplaceCredentials(&snapshot.CredentialDirectory{Aliases: map[string]snapshot.AliasCredentials{
		"ali_1": {AccessKey: "AKIA", SecretCipher: []byte("sealed"), CredentialVer: 1},
	}})
	if _, err := credential.NewClient(store, nil).Store(context.Background(), "ali_1",
		snapshot.AliasView{Endpoint: "https://s3.example.com", CredentialVer: 1}); err == nil {
		t.Fatal("a client without a master key must fail")
	}
}

func TestClientRejectsSecretSealedWithAnotherKey(t *testing.T) {
	controlKey, err := crypto.NewFromBase64("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	if err != nil {
		t.Fatalf("build control cipher: %v", err)
	}
	otherKey, err := crypto.NewFromBase64("YWJjZGVmZ2hpamtsbW5vcGFiY2RlZmdoaWprbG1ub3A=")
	if err != nil {
		t.Fatalf("build other cipher: %v", err)
	}

	store := snapshot.NewStore()
	store.ReplaceCredentials(&snapshot.CredentialDirectory{Aliases: map[string]snapshot.AliasCredentials{
		"ali_1": {AccessKey: "AKIA", SecretCipher: sealed(t, controlKey, "secret"), CredentialVer: 1},
	}})
	if _, err := credential.NewClient(store, otherKey).Store(context.Background(), "ali_1",
		snapshot.AliasView{Endpoint: "https://s3.example.com", CredentialVer: 1}); err == nil {
		t.Fatal("a mismatched master key must surface as a decrypt error")
	}
}
