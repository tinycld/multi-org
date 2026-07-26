package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/pocketbase/pocketbase"

	"tinycld.org/multi-org/internal/orgmanager"
	"tinycld.org/multi-org/internal/store"
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
	if err := cpInitForTest(cp); err != nil {
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
		Root:         root,
		Store:        s,
		TenantBinary: buildTenantBinary(t),
		Logger:       quietLogger(),
		LookupOrg:    OrgLookup(cp.App),
		HooksPool:    2,
	})
	defer mgr.Shutdown()

	inst, err := mgr.Get(context.Background(), "acme")
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

// TestIntegration_CreateOrgToLoadWithTSSchema mirrors the JS test, but authors
// the package in TypeScript and publishes via Provisioner.PublishPackage — which
// runs transpileForStore at publish time (.pb.ts→.pb.js, .ts→.js). The source
// uses TS-only syntax (interface, `as P`, `as any`); if the publish-time
// transpile didn't run, that source would reach the JS engine verbatim and the
// tenant would fail to load it. So this passing proves the full
// TS-authored → transpile-at-publish → materialize → run-on-sobek chain.
func TestIntegration_CreateOrgToLoadWithTSSchema(t *testing.T) {
	root := t.TempDir()

	cp, err := New(filepath.Join(root, "pb_control", "pb_data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.App.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer cp.App.ResetBootstrapState()
	if err := cpInitForTest(cp); err != nil {
		t.Fatal(err)
	}

	s := store.New(root)
	p := NewProvisioner(cp.App, root, s, func(string) {})

	// TypeScript source with TS-only syntax — must be transpiled before it can
	// run on the JS (sobek) engine.
	files := map[string][]byte{
		"server/main.pb.ts": []byte(
			"interface P { ok: boolean }\n" +
				"routerAdd('GET','/whoami',(e)=>e.json(200,{ ok: true } as P))"),
		"pb-migrations/1700000000_widgets.ts": []byte(
			"migrate((app)=>{\n" +
				"  const c = new Collection({ id:'pbc_widgets_01', name:'widgets', type:'base', fields:[{ id:'w_name', name:'name', type:'text' }] } as any)\n" +
				"  app.save(c)\n" +
				"},(app)=>{ app.delete(app.findCollectionByNameOrId('widgets')) })"),
	}
	if err := p.PublishPackage("@tinycld/core", "1.0.0", files, "official"); err != nil {
		t.Fatalf("PublishPackage: %v", err)
	}

	if _, err := p.CreateOrg("acme", "Acme", map[string]string{"@tinycld/core": "1.0.0"}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	// The TS migration (transpiled to JS, materialized, run) must have created the
	// collection in acme's own tenant DB.
	assertTenantHasWidgets(t, filepath.Join(root, "pb_orgs", "acme", "pb_data"))

	// Drive the full runtime chain: OrgLookup (real, DB-backed) → manager.load.
	mgr := orgmanager.New(orgmanager.Config{
		Root:         root,
		Store:        s,
		TenantBinary: buildTenantBinary(t),
		Logger:       quietLogger(),
		LookupOrg:    OrgLookup(cp.App),
		HooksPool:    2,
	})
	defer mgr.Shutdown()

	inst, err := mgr.Get(context.Background(), "acme")
	if err != nil {
		t.Fatalf("manager.Get(acme) via real lookup: %v", err)
	}

	// The TS hook (transpiled, materialized) serves on sobek.
	rec := httptest.NewRecorder()
	inst.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/whoami", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/whoami via manager = %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestIntegration_MaliciousMigrationCannotExec proves provision-time migrations
// run sandboxed: a migration whose top-level code touches $os fails provisioning
// (the binding is absent) rather than executing in the control-plane process.
func TestIntegration_MaliciousMigrationCannotExec(t *testing.T) {
	root := t.TempDir()

	cp, err := New(filepath.Join(root, "pb_control", "pb_data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.App.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer cp.App.ResetBootstrapState()
	if err := cpInitForTest(cp); err != nil {
		t.Fatal(err)
	}

	s := store.New(root)
	// $os at top level runs at migration load — under sandbox it must throw.
	evil := []byte("$os.exec('id'); migrate((app)=>{}, (app)=>{})")
	if err := s.Publish("@tinycld/evil", "1.0.0", map[string][]byte{
		"pb-migrations/1700000000_evil.js": evil,
	}); err != nil {
		t.Fatal(err)
	}

	p := NewProvisioner(cp.App, root, s, func(string) {})
	if _, err := p.CreateOrg("evilorg", "Evil", map[string]string{"@tinycld/evil": "1.0.0"}); err == nil {
		t.Fatal("expected provisioning to fail for a migration touching $os under sandbox")
	}
}

// cpInitForTest runs the control-plane's system migrations + app-scoped schema on
// an already-bootstrapped test app. Mirrors ControlPlane.Init but assumes the
// caller already did Bootstrap (the tests do, so they control the defer of
// ResetBootstrapState).
func cpInitForTest(cp *ControlPlane) error {
	if err := cp.App.RunSystemMigrations(); err != nil {
		return err
	}
	return RunSchema(cp.App)
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

// TestIntegration_TenantHasNoControlPlaneCollections proves the control-plane
// schema (orgs/packages/deployments) does NOT leak into a tenant DB — a tenant
// must know nothing about the registry of other orgs. Guards against the
// process-global AppMigrations leak.
func TestIntegration_TenantHasNoControlPlaneCollections(t *testing.T) {
	root := t.TempDir()

	cp, err := New(filepath.Join(root, "pb_control", "pb_data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.App.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer cp.App.ResetBootstrapState()
	if err := cpInitForTest(cp); err != nil {
		t.Fatal(err)
	}

	// The control-plane itself MUST have the schema (it's the registry).
	for _, name := range []string{"orgs", "packages", "deployments"} {
		if _, err := cp.App.FindCollectionByNameOrId(name); err != nil {
			t.Fatalf("control-plane missing its own %q collection: %v", name, err)
		}
	}

	s := store.New(root)
	if err := s.Publish("@tinycld/core", "1.0.0", map[string][]byte{
		"pb-migrations/1700000000_widgets.js": []byte(widgetsMigration),
	}); err != nil {
		t.Fatal(err)
	}
	p := NewProvisioner(cp.App, root, s, func(string) {})
	if _, err := p.CreateOrg("acme", "Acme", map[string]string{"@tinycld/core": "1.0.0"}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	tenant := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir:  filepath.Join(root, "pb_orgs", "acme", "pb_data"),
		HideStartBanner: true,
	})
	if err := tenant.Bootstrap(); err != nil {
		t.Fatalf("bootstrap tenant DB: %v", err)
	}
	defer tenant.App.ResetBootstrapState()

	for _, name := range []string{"orgs", "packages", "deployments"} {
		if _, err := tenant.App.FindCollectionByNameOrId(name); err == nil {
			t.Errorf("tenant DB leaked control-plane collection %q", name)
		}
	}
	// The tenant's own package schema must still be present.
	if _, err := tenant.App.FindCollectionByNameOrId("widgets"); err != nil {
		t.Errorf("tenant DB missing its own 'widgets' collection: %v", err)
	}
}

// TestIntegration_MaliciousHookCannotCrashControlPlane proves a package whose
// hook throws at load fails provisioning with a returned error, rather than
// panicking the control-plane process.
func TestIntegration_MaliciousHookCannotCrashControlPlane(t *testing.T) {
	root := t.TempDir()

	cp, err := New(filepath.Join(root, "pb_control", "pb_data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.App.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer cp.App.ResetBootstrapState()
	if err := cpInitForTest(cp); err != nil {
		t.Fatal(err)
	}

	s := store.New(root)
	if err := s.Publish("@tinycld/evilhook", "1.0.0", map[string][]byte{
		"server/main.pb.js": []byte(`$os.exec('id')`),
	}); err != nil {
		t.Fatal(err)
	}

	p := NewProvisioner(cp.App, root, s, func(string) {})
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("provisioning panicked on a load-throwing hook: %v", r)
		}
	}()
	if _, err := p.CreateOrg("evilhook", "EvilHook", map[string]string{"@tinycld/evilhook": "1.0.0"}); err == nil {
		t.Fatal("expected provisioning to fail for a load-throwing hook, got nil")
	}
}
