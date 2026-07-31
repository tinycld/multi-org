package controlplane

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/apis"
)

func TestOrgLookup_ReturnsActiveOrgRecord(t *testing.T) {
	cp, root := newProvCP(t)
	p := newFakeProvisioner(cp, root, func(string) {}, nil)
	if _, err := p.CreateOrg("acme", "Acme", baseLock); err != nil {
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
	// The manager refuses to load an org without a build reference, so the
	// lookup must surface the recipe hash CreateOrg recorded.
	if rec.RecipeHash != hashOld {
		t.Fatalf("RecipeHash = %q, want %s", rec.RecipeHash, hashOld)
	}
	if _, ok := lookup("ghost"); ok {
		t.Fatal("expected ghost to be absent")
	}
}

func TestRegisterRoutes_OrgsRequiresSuperuser(t *testing.T) {
	cp, root := newProvCP(t)
	p := NewProvisioner(cp.App, root, func(string) {}, nil)
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
