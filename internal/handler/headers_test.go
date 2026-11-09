package handler_test

import (
	"net/http"
	"testing"

	"github.com/pageforge/pageforge/internal/model"
	"github.com/pageforge/pageforge/internal/snapshot"
)

func boolPtr(v bool) *bool { return &v }

func TestResponseHeadersProjectDefaultsAndVariables(t *testing.T) {
	f := newFixture(t, func(s *snapshot.Snapshot) {
		project := s.Projects[testPID]
		project.ResponseHeaders = []model.HeaderRule{
			{Name: "X-Site", Value: "$project_id/$version_id"},
			{Name: "X-Binding", Value: "$binding_id"},
		}
		s.Projects[testPID] = project
	})

	rec := f.do(http.MethodGet, "/big.txt", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("X-Site"); got != testPID+"/"+testVID {
		t.Fatalf("X-Site = %q", got)
	}
	if got := rec.Header().Get("X-Binding"); got != "bnd_1" {
		t.Fatalf("X-Binding = %q", got)
	}
}

func TestBindingHeadersOverrideProject(t *testing.T) {
	f := newFixture(t, func(s *snapshot.Snapshot) {
		project := s.Projects[testPID]
		project.ResponseHeaders = []model.HeaderRule{
			{Name: "X-Site", Value: "project"},
			{Name: "X-Only-Project", Value: "yes"},
		}
		s.Projects[testPID] = project
		s.Bindings[0].ResponseHeaders = []model.HeaderRule{
			{Name: "X-Site", Value: "binding"},
		}
	})

	rec := f.do(http.MethodGet, "/big.txt", nil)
	if got := rec.Header().Get("X-Site"); got != "binding" {
		t.Fatalf("X-Site = %q, want binding", got)
	}
	if got := rec.Header().Get("X-Only-Project"); got != "yes" {
		t.Fatalf("X-Only-Project = %q", got)
	}
}

func TestBindingHeadersAppend(t *testing.T) {
	f := newFixture(t, func(s *snapshot.Snapshot) {
		project := s.Projects[testPID]
		project.ResponseHeaders = []model.HeaderRule{
			{Name: "X-Site", Value: "project"},
		}
		s.Projects[testPID] = project
		s.Bindings[0].ResponseHeaders = []model.HeaderRule{
			{Name: "X-Site", Value: "binding", Replace: boolPtr(false)},
		}
	})

	rec := f.do(http.MethodGet, "/big.txt", nil)
	if got := rec.Header().Get("X-Site"); got != "project, binding" {
		t.Fatalf("X-Site = %q, want project, binding", got)
	}
}

func TestResponseHeadersOnRedirectAndNotFound(t *testing.T) {
	f := newFixture(t, func(s *snapshot.Snapshot) {
		project := s.Projects[testPID]
		project.ResponseHeaders = []model.HeaderRule{{Name: "X-Env", Value: "prod"}}
		s.Projects[testPID] = project
	})

	rec := f.do(http.MethodGet, "/docs", nil)
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 301", rec.Code)
	}
	if got := rec.Header().Get("X-Env"); got != "prod" {
		t.Fatalf("301 X-Env = %q", got)
	}

	rec = f.do(http.MethodGet, "/missing-page.html", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("X-Env"); got != "prod" {
		t.Fatalf("404 X-Env = %q", got)
	}
}

func TestResponseHeadersSkipForbiddenAndOverrideCacheControl(t *testing.T) {
	f := newFixture(t, func(s *snapshot.Snapshot) {
		project := s.Projects[testPID]
		project.ResponseHeaders = []model.HeaderRule{
			{Name: "Content-Length", Value: "999"},
			{Name: "Cache-Control", Value: "public, max-age=3600"},
		}
		s.Projects[testPID] = project
	})

	rec := f.do(http.MethodGet, "/big.txt", nil)
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=3600" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := rec.Header().Get("Content-Length"); got == "999" {
		t.Fatalf("forbidden Content-Length rule was applied")
	}
}
