//go:build linux

package orgmanager

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"tinycld.org/multi-org/internal/store"
)

// The degraded mode NewSpawner promises: on a non-root Linux host, tenants
// spawn as plain unconfined subprocesses. Before the namespace flags were
// gated on root, every spawn failed clone(2) with EPERM — every org 503'd —
// while the spawner's warning claimed a degraded-but-working mode.
//
// This runs in CI's unprivileged unit step; the root-only TestConfinement_*
// suite covers the confined path.
func TestSpawner_NonRootSpawnsUnconfined(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("this test asserts the NON-root degraded mode")
	}
	bin := buildTenantBinary(t)
	root := t.TempDir()

	s := store.New(root)
	if err := s.Publish("@tinycld/core", "1.0.0", map[string][]byte{
		"client/dist/index.html": []byte("<html></html>"),
	}); err != nil {
		t.Fatal(err)
	}

	mgr := New(Config{
		Root:         root,
		Store:        s,
		Spawner:      NewSpawner(quietLogger()),
		TenantBinary: bin,
		Logger:       quietLogger(),
		HooksPool:    2,
		LookupOrg: stubLookup(map[string]OrgRecord{
			"acme": {Slug: "acme", Status: "active", Lockfile: []byte(`{"@tinycld/core":"1.0.0"}`)},
		}),
	})
	t.Cleanup(mgr.Shutdown)

	inst, err := mgr.Get(context.Background(), "acme")
	if err != nil {
		t.Fatalf("non-root spawn through the production spawner failed: %v", err)
	}
	rec := httptest.NewRecorder()
	inst.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("health = %d body=%s", rec.Code, rec.Body.String())
	}
}
