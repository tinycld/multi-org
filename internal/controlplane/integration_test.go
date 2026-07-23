package controlplane

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/pocketbase/pocketbase"

	"tinycld.org/multitenant/internal/orgmanager"
	"tinycld.org/multitenant/internal/progcache"
	"tinycld.org/multitenant/internal/store"
)

// widgetsMigration is a minimal PocketBase JS migration that creates one
// application collection, standing in for a real @tinycld package's schema.
const widgetsMigration = `migrate(
	(app) => {
		const c = new Collection({ id: 'pbc_widgets_01', name: 'widgets', type: 'base', fields: [
			{ id: 'w_name', name: 'name', type: 'text', required: true, max: 100 },
		]})
		app.save(c)
	},
	(app) => { app.delete(app.findCollectionByNameOrId('widgets')) }
)`

// TestIntegration_CreateOrgToLoadWithSchema drives the real
// CreateOrg → OrgLookup → orgmanager.load chain (no stub lookup) and asserts a
// provisioned tenant boots with the application collection its package's
// pb-migrations declared — the whole point of the tenant-schema materialization.
func TestIntegration_CreateOrgToLoadWithSchema(t *testing.T) {
	root := t.TempDir()

	cp, err := New(filepath.Join(root, "pb_control", "pb_data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.App.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer cp.App.ResetBootstrapState()
	if err := cp.App.RunAllMigrations(); err != nil {
		t.Fatal(err)
	}

	// A package that ships a hook (so the org serves a route) and a schema
	// migration (so the tenant DB gains a collection).
	s := store.New(root)
	if err := s.Publish("@tinycld/core", "1.0.0", map[string][]byte{
		"server/main.pb.js":                   []byte("routerAdd('GET','/whoami',(e)=>e.json(200,{ok:true}))"),
		"pb-migrations/1700000000_widgets.js": []byte(widgetsMigration),
	}); err != nil {
		t.Fatal(err)
	}

	p := NewProvisioner(cp.App, root, s, func(string) {})
	if _, err := p.CreateOrg("acme", "Acme", map[string]string{"@tinycld/core": "1.0.0"}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	// The migration must have persisted into acme's own tenant DB at provision
	// time. Open that DB fresh and confirm the collection exists — proving schema
	// reached the tenant, not just the control plane.
	assertTenantHasWidgets(t, filepath.Join(root, "pb_orgs", "acme", "pb_data"))

	// Now drive the full runtime chain: OrgLookup (real, DB-backed) → manager.load.
	mgr := orgmanager.New(orgmanager.Config{
		Root:      root,
		Store:     s,
		Programs:  progcache.New(),
		LookupOrg: OrgLookup(cp.App),
		HooksPool: 2,
	})
	defer mgr.Shutdown()

	inst, err := mgr.Get("acme")
	if err != nil {
		t.Fatalf("manager.Get(acme) via real lookup: %v", err)
	}

	// The hook route serves (proves hooks materialized + app booted via the
	// lazy-load path, not just the provision-time bootstrap).
	rec := httptest.NewRecorder()
	inst.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/whoami", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/whoami via manager = %d body=%s", rec.Code, rec.Body.String())
	}
}

func assertTenantHasWidgets(t *testing.T, dataDir string) {
	t.Helper()
	tenant := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir:  dataDir,
		HideStartBanner: true,
	})
	if err := tenant.Bootstrap(); err != nil {
		t.Fatalf("bootstrap tenant DB: %v", err)
	}
	defer tenant.App.ResetBootstrapState()
	if _, err := tenant.App.FindCollectionByNameOrId("widgets"); err != nil {
		t.Fatalf("tenant DB missing 'widgets' collection from package migration: %v", err)
	}
}
