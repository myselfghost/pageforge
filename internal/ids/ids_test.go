package ids

import (
	"strings"
	"testing"
)

func TestULIDFormat(t *testing.T) {
	id := ULID()
	if len(id) != 26 {
		t.Fatalf("ULID() = %q, want 26 characters", id)
	}
	for _, c := range id {
		if !strings.ContainsRune(alphabet, c) {
			t.Fatalf("ULID() = %q contains %q outside the Crockford alphabet", id, c)
		}
	}
}

func TestNewPrefix(t *testing.T) {
	id := New(Project)
	if !strings.HasPrefix(id, "prj_") {
		t.Fatalf("New(Project) = %q, want prj_ prefix", id)
	}
	if len(id) != len("prj_")+26 {
		t.Fatalf("New(Project) = %q, want 30 characters", id)
	}
}

func TestULIDUnique(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id := ULID()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate ULID %q after %d iterations", id, i)
		}
		seen[id] = struct{}{}
	}
}
