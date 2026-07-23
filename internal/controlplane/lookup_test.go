package controlplane

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/apis"

	"tinycld.org/multi-org/internal/store"
)

func TestOrgLookup_ReturnsActiveOrgRecord(t *testing.T) {
	root := t.TempDir()
	cp, _ := New(filepath.Join(root, "pb_control", "pb_data"))
	if err := cp.App.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer cp.App.ResetBootstrapState()
	if err := cp.App.RunAllMigrations(); err != nil {
		t.Fatal(err)
	}

	s := store.New(root)
	_ = s.Publish("@tinycld/core", "1.0.0", map[string][]byte{"server/a.pb.js": []byte("1")})
	p := NewProvisioner(cp.App, root, s, func(string) {})
	if _, err := p.CreateOrg("acme", "Acme", map[string]string{"@tinycld/core": "1.0.0"}); err != nil {
		t.Fatal(err)
	}

	lookup := OrgLookup(cp.App)
	rec, ok := lookup("acme")
	if !ok {
		t.Fatal("expected acme to be found")
	}
	if rec.Status != "active" || rec.Slug != "acme" {
		t.Fatalf("unexpected: %+v", rec)
	}
	if len(rec.Lockfile) == 0 {
		t.Fatal("expected lockfile bytes to be populated")
	}
	if _, ok := lookup("ghost"); ok {
		t.Fatal("expected ghost to be absent")
	}
}

func TestRegisterRoutes_OrgsRequiresSuperuser(t *testing.T) {
	root := t.TempDir()
	cp, _ := New(filepath.Join(root, "pb_control", "pb_data"))
	if err := cp.App.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer cp.App.ResetBootstrapState()
	if err := cp.App.RunAllMigrations(); err != nil {
		t.Fatal(err)
	}
	s := store.New(root)
	p := NewProvisioner(cp.App, root, s, func(string) {})
	p.RegisterRoutes()

	mux, err := apis.BuildServeMux(cp.App, apis.ServeConfig{})
	if err != nil {
		t.Fatal(err)
	}

	// Unauthenticated POST /api/orgs must be rejected (401), proving the route is
	// wired AND superuser-guarded.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/orgs", strings.NewReader(`{"slug":"acme","lockfile":{}}`))
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated provisioning, got %d: %s", rec.Code, rec.Body.String())
	}
}
