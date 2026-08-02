package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pocketbase/pocketbase"

	"tinycld.org/core/pkgbuild"
	"tinycld.org/core/tenantcfg"
	"tinycld.org/multi-org/internal/builder"
	"tinycld.org/multi-org/internal/orgmanager"
)

// countingSpawner wraps the real spawner and counts tenant process starts, so
// a test can pin WHERE a spawn happened: provision-time verification must
// spawn exactly once, and a later Get must reuse that instance rather than
// booting a second process.
type countingSpawner struct {
	inner orgmanager.Spawner
	n     atomic.Int32
}

func (c *countingSpawner) Spawn(ctx context.Context, req orgmanager.SpawnRequest, log *slog.Logger) (orgmanager.Process, error) {
	c.n.Add(1)
	return c.inner.Spawn(ctx, req, log)
}

func (c *countingSpawner) Confines() bool { return c.inner.Confines() }

// baseMember is the app-shell entry every recipe carries. BuildResolver skips
// it when deriving the resolved feature list (it ships no manifest).
func baseMember() pkgbuild.ResolvedMember {
	return pkgbuild.ResolvedMember{Slug: pkgbuild.BaseMemberSlug, Name: pkgbuild.BaseMemberSlug, Version: "1.0.0"}
}

// commitArtifact fabricates a committed build artifact under <root>/builds the
// way the trusted builder lays one out: the flattened pb_hooks/pb_migrations/
// pb_public runtime trees (the store era was per-package and materialize
// flattened; the artifact carries the final trees directly), the tenant binary
// at <dir>/tinycld (production resolves the binary from the artifact itself),
// any manifests/<slug>/manifest.json entries via files, and recipe.json naming
// the members. Returns the recipe hash, derived from seed so distinct fixtures
// in one root get distinct CAS entries. The layout satisfies builder.Store, so
// tests resolve it through the production BuildResolver.
func commitArtifact(t *testing.T, root, seed, binary string, files map[string]string, members []pkgbuild.ResolvedMember) string {
	t.Helper()
	sum := sha256.Sum256([]byte(seed))
	hexName := fmt.Sprintf("%x", sum)
	hash := "sha256:" + hexName
	dir := filepath.Join(root, "builds", hexName)
	for _, name := range []string{"pb_hooks", "pb_public", "pb_migrations"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for rel, body := range files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	bin := filepath.Join(dir, "tinycld")
	if err := os.Link(binary, bin); err != nil {
		body, readErr := os.ReadFile(binary)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if err := os.WriteFile(bin, body, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	recipe, err := json.Marshal(tenantcfg.ArtifactRecipe{RecipeHash: hash, Members: members})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, builder.RecipeFile), recipe, 0o644); err != nil {
		t.Fatal(err)
	}
	return hash
}

// artifactResolver is the production build resolution over the fixture store —
// the same wiring serve-multi uses.
func artifactResolver(root string) func(string) (orgmanager.BuildRef, error) {
	return BuildResolver(builder.NewStore(filepath.Join(root, "builds")), "tinycld")
}

// newVerifiedProvisioner builds the production-shaped provisioning wiring — a
// real manager booting the org's committed artifact (its own binary, its own
// trees), and a Provisioner whose verify hook boots the org through it,
// exactly as serve-multi wires it. The org's migrations therefore run inside
// the tenant process; the control plane itself never executes tenant JS. The
// deployer's builder is a fake returning recipeHash: the artifact is already
// committed, so CreateOrg's build is a cache hit by construction.
func newVerifiedProvisioner(t *testing.T, cp *ControlPlane, root, recipeHash string) (*orgmanager.OrgManager, *Provisioner, *countingSpawner) {
	t.Helper()
	spawner := &countingSpawner{inner: orgmanager.NewSpawner(quietLogger())}
	mgr := orgmanager.New(orgmanager.Config{
		Root:         root,
		Logger:       quietLogger(),
		LookupOrg:    OrgLookup(cp.App),
		HooksPool:    2,
		Spawner:      spawner,
		ResolveBuild: artifactResolver(root),
	})
	t.Cleanup(mgr.Shutdown)
	verify := func(ctx context.Context, slug string) error {
		_, err := mgr.Get(ctx, slug)
		return err
	}
	p := NewProvisioner(cp.App, root, mgr.Evict, verify)
	p.deployer = newDeployer(cp.App, root, &fakeArtifactBuilder{hash: recipeHash}, mgr.Evict, verify, quietTestLogger())
	stubOwnerStep(p)
	return mgr, p, spawner
}

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
// provisioned tenant boots with the application collection its artifact's
// pb_migrations declared — the whole point of booting from a committed build.
func TestIntegration_CreateOrgToLoadWithSchema(t *testing.T) {
	root := t.TempDir()

	cp, err := New(filepath.Join(root, "pb_control", "pb_data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.App.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer func() { WaitForAppDeploys(cp.App); _ = cp.App.ResetBootstrapState() }()
	if err := cpInitForTest(cp); err != nil {
		t.Fatal(err)
	}

	// An artifact that ships a hook (so the org serves a route) and a schema
	// migration (so the tenant DB gains a collection).
	hash := commitArtifact(t, root, "widgets", buildTenantBinary(t), map[string]string{
		"pb_hooks/main.pb.js":                 "routerAdd('GET','/whoami',(e)=>e.json(200,{ok:true}))",
		"pb_migrations/1700000000_widgets.js": widgetsMigration,
	}, []pkgbuild.ResolvedMember{baseMember()})

	mgr, p, spawner := newVerifiedProvisioner(t, cp, root, hash)
	if _, _, err := p.CreateOrg("acme", "Acme", map[string]string{"tinycld": "1.0.0"}, OwnerAccount{Email: "owner@example.com"}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	// Provision-time verification must have booted the tenant — in its own
	// process, which is where the org's migrations now run.
	if got := spawner.n.Load(); got != 1 {
		t.Fatalf("expected CreateOrg to spawn the tenant once for verification, got %d spawns", got)
	}

	// A request reuses the verified instance rather than booting a second one.
	inst, err := mgr.Get(context.Background(), "acme")
	if err != nil {
		t.Fatalf("manager.Get(acme) via real lookup: %v", err)
	}
	rec := httptest.NewRecorder()
	inst.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/whoami", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/whoami via manager = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := spawner.n.Load(); got != 1 {
		t.Fatalf("Get after provisioning respawned the tenant: %d spawns", got)
	}

	// The migration must have persisted into acme's own tenant DB during the
	// provision-time boot. Quiesce the tenant first — the assertion opens the
	// DB in this process — then confirm the collection exists, proving schema
	// reached the tenant.
	mgr.Shutdown()
	assertTenantHasWidgets(t, filepath.Join(root, "pb_orgs", "acme", "pb_data"))
}

// TestIntegration_MaliciousMigrationCannotExec proves provision-time migrations
// run sandboxed INSIDE the tenant process: a migration whose top-level code
// touches $os fails the tenant boot (the binding is absent in a sandboxed VM),
// the reason travels back through the readiness handshake, and provisioning
// fails with the org left resumable — the migration never executes in the
// control-plane process, which no longer opens tenant apps at all.
func TestIntegration_MaliciousMigrationCannotExec(t *testing.T) {
	root := t.TempDir()

	cp, err := New(filepath.Join(root, "pb_control", "pb_data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.App.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer func() { WaitForAppDeploys(cp.App); _ = cp.App.ResetBootstrapState() }()
	if err := cpInitForTest(cp); err != nil {
		t.Fatal(err)
	}

	// $os at top level runs at migration load — under sandbox it must throw.
	hash := commitArtifact(t, root, "evil-migration", buildTenantBinary(t), map[string]string{
		"pb_migrations/1700000000_evil.js": "$os.exec('id'); migrate((app)=>{}, (app)=>{})",
	}, []pkgbuild.ResolvedMember{baseMember()})

	_, p, _ := newVerifiedProvisioner(t, cp, root, hash)
	_, _, err = p.CreateOrg("evilorg", "Evil", map[string]string{"tinycld": "1.0.0"}, OwnerAccount{Email: "owner@example.com"})
	if err == nil {
		t.Fatal("expected provisioning to fail for a migration touching $os under sandbox")
	}
	// A bare err != nil would stay green with Sandboxed removed if provisioning
	// failed for any unrelated reason (a fixture path typo, a broken artifact).
	// The failure must be the sandbox refusing the binding: $os is simply not
	// defined in a sandboxed VM, so the load throws a ReferenceError naming it —
	// and that reason must survive the trip through the readiness pipe.
	if !strings.Contains(err.Error(), "$os") {
		t.Fatalf("provisioning failed, but not because the sandbox withheld $os: %v", err)
	}
	// The failed org must be rolled back to a resumable state, never active.
	rec, err := cp.App.FindFirstRecordByData("orgs", "slug", "evilorg")
	if err != nil {
		t.Fatalf("org row should survive for resume: %v", err)
	}
	if got := rec.GetString("status"); got != "provisioning" {
		t.Fatalf("expected failed org rolled back to provisioning, got %q", got)
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
	defer func() { WaitForAppDeploys(tenant.App); _ = tenant.App.ResetBootstrapState() }()
	if _, err := tenant.App.FindCollectionByNameOrId("widgets"); err != nil {
		t.Fatalf("tenant DB missing 'widgets' collection from package migration: %v", err)
	}
}

// TestIntegration_TenantHasNoControlPlaneCollections proves the control-plane
// schema (orgs/deployments) does NOT leak into a tenant DB — a tenant must
// know nothing about the registry of other orgs. Guards against the
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
	defer func() { WaitForAppDeploys(cp.App); _ = cp.App.ResetBootstrapState() }()
	if err := cpInitForTest(cp); err != nil {
		t.Fatal(err)
	}

	// The control-plane itself MUST have the schema (it's the registry).
	for _, name := range []string{"orgs", "deployments"} {
		if _, err := cp.App.FindCollectionByNameOrId(name); err != nil {
			t.Fatalf("control-plane missing its own %q collection: %v", name, err)
		}
	}

	hash := commitArtifact(t, root, "widgets-only", buildTenantBinary(t), map[string]string{
		"pb_migrations/1700000000_widgets.js": widgetsMigration,
	}, []pkgbuild.ResolvedMember{baseMember()})
	mgr, p, _ := newVerifiedProvisioner(t, cp, root, hash)
	if _, _, err := p.CreateOrg("acme", "Acme", map[string]string{"tinycld": "1.0.0"}, OwnerAccount{Email: "owner@example.com"}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	// Quiesce the tenant the verification boot left running before opening its
	// DB in this process.
	mgr.Shutdown()

	tenant := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir:  filepath.Join(root, "pb_orgs", "acme", "pb_data"),
		HideStartBanner: true,
	})
	if err := tenant.Bootstrap(); err != nil {
		t.Fatalf("bootstrap tenant DB: %v", err)
	}
	defer func() { WaitForAppDeploys(tenant.App); _ = tenant.App.ResetBootstrapState() }()

	for _, name := range []string{"orgs", "deployments", "control_settings", "org_mail_domains"} {
		if _, err := tenant.App.FindCollectionByNameOrId(name); err == nil {
			t.Errorf("tenant DB leaked control-plane collection %q", name)
		}
	}
	// The tenant's own package schema must still be present.
	if _, err := tenant.App.FindCollectionByNameOrId("widgets"); err != nil {
		t.Errorf("tenant DB missing its own 'widgets' collection: %v", err)
	}
}

// TestIntegration_MaliciousHookCannotCrashControlPlane proves an artifact whose
// hook throws at load fails provisioning with a returned error — the tenant
// boot fails and reports through the readiness handshake — rather than
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
	defer func() { WaitForAppDeploys(cp.App); _ = cp.App.ResetBootstrapState() }()
	if err := cpInitForTest(cp); err != nil {
		t.Fatal(err)
	}

	hash := commitArtifact(t, root, "evil-hook", buildTenantBinary(t), map[string]string{
		"pb_hooks/main.pb.js": `$os.exec('id')`,
	}, []pkgbuild.ResolvedMember{baseMember()})

	_, p, _ := newVerifiedProvisioner(t, cp, root, hash)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("provisioning panicked on a load-throwing hook: %v", r)
		}
	}()
	if _, _, err := p.CreateOrg("evilhook", "EvilHook", map[string]string{"tinycld": "1.0.0"}, OwnerAccount{Email: "owner@example.com"}); err == nil {
		t.Fatal("expected provisioning to fail for a load-throwing hook, got nil")
	}
}
