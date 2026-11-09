package authpolicy_test

import (
	"context"
	"testing"

	"github.com/pageforge/pageforge/internal/authpolicy"
	"github.com/pageforge/pageforge/internal/testutil"
)

func TestPolicyUsersAndDirectory(t *testing.T) {
	db := testutil.NewDB(t)
	repo := authpolicy.NewRepo(db)
	ctx := context.Background()

	created, err := repo.Create(ctx, authpolicy.CreateInput{Name: "internal-docs", Mode: "basic"})
	if err != nil {
		t.Fatalf("create policy: %v", err)
	}
	if created.Mode != authpolicy.ModeBasic || created.SecretVersion != 1 {
		t.Fatalf("created = %+v", created)
	}

	// 默认模式为 basic。
	if _, err := repo.Create(ctx, authpolicy.CreateInput{Name: "default-mode"}); err != nil {
		t.Fatalf("create default mode: %v", err)
	}
	if _, err := repo.Create(ctx, authpolicy.CreateInput{Name: "bad", Mode: "none"}); err == nil {
		t.Fatal("unsupported mode was accepted")
	}

	user, err := repo.UpsertUser(ctx, created.ID, "alice", authpolicy.UserInput{Password: "s3cret!"})
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	if !user.PasswordHashSet || user.Username != "alice" {
		t.Fatalf("user = %+v", user)
	}

	detail, err := repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get policy: %v", err)
	}
	if len(detail.Users) != 1 || detail.Users[0].Username != "alice" {
		t.Fatalf("users = %+v", detail.Users)
	}
	if detail.SecretVersion != 2 {
		t.Fatalf("secret_version = %d, want 2", detail.SecretVersion)
	}

	// 更新密码同样递增 secret_version。
	if _, err := repo.UpsertUser(ctx, created.ID, "alice", authpolicy.UserInput{Password: "n3w-secret"}); err != nil {
		t.Fatalf("update user: %v", err)
	}
	detail, _ = repo.Get(ctx, created.ID)
	if detail.SecretVersion != 3 {
		t.Fatalf("secret_version = %d, want 3", detail.SecretVersion)
	}

	dir, err := repo.Directory(ctx)
	if err != nil {
		t.Fatalf("directory: %v", err)
	}
	secrets, ok := dir.Policies[created.ID]
	if !ok {
		t.Fatalf("policy missing from directory: %+v", dir)
	}
	if secrets.Realm != "internal-docs" || secrets.SecretVersion != 3 {
		t.Fatalf("secrets = %+v", secrets)
	}
	if secrets.Users["alice"] == "" {
		t.Fatalf("password hash missing: %+v", secrets.Users)
	}

	if err := repo.DeleteUser(ctx, created.ID, "alice"); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if err := repo.DeleteUser(ctx, created.ID, "alice"); err == nil {
		t.Fatal("deleting a missing user succeeded")
	}
	detail, _ = repo.Get(ctx, created.ID)
	if detail.SecretVersion != 4 {
		t.Fatalf("secret_version = %d, want 4", detail.SecretVersion)
	}

	if err := repo.Delete(ctx, created.ID); err != nil {
		t.Fatalf("delete policy: %v", err)
	}
	if _, err := repo.Get(ctx, created.ID); err == nil {
		t.Fatal("deleted policy is still readable")
	}
}

func TestUserValidation(t *testing.T) {
	db := testutil.NewDB(t)
	repo := authpolicy.NewRepo(db)
	ctx := context.Background()
	policy, err := repo.Create(ctx, authpolicy.CreateInput{Name: "validation"})
	if err != nil {
		t.Fatalf("create policy: %v", err)
	}

	cases := []struct {
		username string
		password string
	}{
		{"", "password1"},
		{"a:b", "password1"},
		{"alice", ""},
		{"alice", string(make([]byte, 80))},
	}
	for _, tc := range cases {
		if _, err := repo.UpsertUser(ctx, policy.ID, tc.username, authpolicy.UserInput{Password: tc.password}); err == nil {
			t.Fatalf("invalid user input accepted: %+v", tc)
		}
	}
}
