package controlplane

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/plugins/jsvm"

	"tinycld.org/core/pkgbuild"
	"tinycld.org/multi-org/internal/manifesteval"
	"tinycld.org/multi-org/internal/materialize"
	"tinycld.org/multi-org/internal/orgmanager"
	"tinycld.org/multi-org/internal/testsupport"
)

// contactsSchemaMigration creates the contacts collection a CardDAV-serving
// contacts tenant needs. `users` is a stock PB auth collection.
//
// Single-org: the contact's `owner` relation points directly at `users` (the
// former `user_org` junction is gone), so singleOrgScope filters contacts by
// owner == the authenticated user's id — no membership table.
const contactsSchemaMigration = `migrate((app) => {
	const users = app.findCollectionByNameOrId('users')
	const contacts = new Collection({ id: 'pbc_contacts01', name: 'contacts', type: 'base', fields: [
		{ id: 'c_owner', name: 'owner', type: 'relation', collectionId: users.id, maxSelect: 1 },
		{ id: 'c_uid', name: 'vcard_uid', type: 'text' },
		{ id: 'c_first', name: 'first_name', type: 'text' },
		{ id: 'c_last', name: 'last_name', type: 'text' },
		{ id: 'c_email', name: 'email', type: 'text' },
		{ id: 'c_del', name: 'deleted_at', type: 'text' },
		{ id: 'c_upd', name: 'updated', type: 'autodate', onCreate: true, onUpdate: true }
	]})
	app.save(contacts)
}, (app) => {
	app.delete(app.findCollectionByNameOrId('contacts'))
})`

const contactsCardDAVManifest = `
const manifest = {
	name: 'Contacts', slug: 'contacts', version: '1.0.0', description: 'contacts',
	carddav: {
		collection: 'contacts',
		listFilter: "owner = {:ownerId} && deleted_at = ''",
		sort: '-updated',
		ownerField: 'owner',
		uidField: 'vcard_uid',
		softDeleteField: 'deleted_at',
		vcard: {
			version: '4.0',
			name: { given: 'first_name', family: 'last_name' },
			simple: { EMAIL: 'email' },
			revField: 'updated',
		},
	},
}
export default manifest
`

// buildTenantBinary compiles the app shell's dual-mode binary once per test run
// (shared across packages via testsupport) — the real production tenant binary.
// CardDAV now runs inside the tenant process, so proving it works needs it.
func buildTenantBinary(t *testing.T) string {
	t.Helper()
	return testsupport.BuildTenantBinary(t)
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestIntegration_MultiOrgCardDAV drives the full multi-org CardDAV path:
// commit a contacts-bearing artifact (evaluated carddav manifest + schema) →
// provision two orgs from it → seed each tenant DB → REPORT each org's CardDAV
// endpoint through its proxy and confirm each serves ONLY its own contact.
//
// With per-process isolation this exercises the real path end to end: under
// the single-Register contract the tenant's own linked contacts Go mounts
// /carddav (feature self-mount — the router still materializes the manifest's
// carddav block into the org's runtime dir, but only pre-single-Register
// artifact binaries read it), the mount serves against the org's own DB, and
// the response travels back over the unix socket.
func TestIntegration_MultiOrgCardDAV(t *testing.T) {
	root := t.TempDir()
	mgr, p, artifactDir := setupCardDAVOrgs(t, root)
	defer mgr.Shutdown()

	seedOrg(t, p, root, artifactDir, "acme", "alice@acme.test", "Alice", "Acme")
	seedOrg(t, p, root, artifactDir, "globex", "bob@globex.test", "Bob", "Globex")

	acmeVCF := carddavReport(t, mgr, "acme", "alice@acme.test")
	if !strings.Contains(acmeVCF, "Alice") {
		t.Errorf("acme CardDAV should contain Alice, got:\n%s", acmeVCF)
	}
	if strings.Contains(acmeVCF, "Bob") {
		t.Errorf("acme CardDAV leaked globex's Bob:\n%s", acmeVCF)
	}

	globexVCF := carddavReport(t, mgr, "globex", "bob@globex.test")
	if !strings.Contains(globexVCF, "Bob") {
		t.Errorf("globex CardDAV should contain Bob, got:\n%s", globexVCF)
	}
	if strings.Contains(globexVCF, "Alice") {
		t.Errorf("globex CardDAV leaked acme's Alice:\n%s", globexVCF)
	}

	// The two content assertions above cannot fail on their own: Bob is only
	// ever inserted into globex's separate DB served by a separate process, so
	// "acme doesn't contain Bob" is true by construction. The falsifiable
	// boundary probe is the principal, not the payload: an org's user must not
	// authenticate against ANOTHER org's tenant. This goes red if the tenants
	// ever share a DB (both users would exist in both) or if the router routes
	// one slug to the other's socket (the foreign user would suddenly resolve).
	if code := carddavStatus(t, mgr, "globex", "alice@acme.test"); code != http.StatusUnauthorized {
		t.Errorf("acme's user authenticated against globex's tenant: got %d, want 401", code)
	}
	if code := carddavStatus(t, mgr, "acme", "bob@globex.test"); code != http.StatusUnauthorized {
		t.Errorf("globex's user authenticated against acme's tenant: got %d, want 401", code)
	}
}

// setupCardDAVOrgs boots a control plane, commits a contacts artifact, and
// returns a manager wired to spawn real tenant processes from it, plus the
// committed artifact directory (seeding reads its trees directly).
//
// The manifest fixture goes through manifesteval.EvalJSON — the same evaluator
// the builder runs when staging manifests/<slug>/manifest.json — so the test
// covers the manifest.ts → manifest.json → CardDAVSources read path the
// deleted publish-time emitManifestJSON used to provide.
func setupCardDAVOrgs(t *testing.T, root string) (*orgmanager.OrgManager, *Provisioner, string) {
	t.Helper()
	bin := buildTenantBinary(t)

	cp, err := New(filepath.Join(root, "pb_control", "pb_data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.App.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cp.App.ResetBootstrapState() })
	if err := cpInitForTest(cp); err != nil {
		t.Fatal(err)
	}

	manifestJSON, err := manifesteval.EvalJSON(contactsCardDAVManifest, "manifest.ts")
	if err != nil {
		t.Fatalf("eval contacts manifest: %v", err)
	}
	hash := commitArtifact(t, root, "carddav-contacts", bin, map[string]string{
		"pb_migrations/1700000000_contacts.js":   contactsSchemaMigration,
		"manifests/contacts/" + manifestJSONFile: string(manifestJSON),
	}, []pkgbuild.ResolvedMember{
		baseMember(),
		{Slug: "contacts", Name: "@tinycld/contacts", Version: "1.0.0"},
	})

	p := NewProvisioner(cp.App, root, func(string) {}, nil)
	p.deployer = newDeployer(cp.App, root, &fakeArtifactBuilder{hash: hash}, func(string) {}, nil, quietTestLogger())

	mgr := orgmanager.New(orgmanager.Config{
		Root:           root,
		Logger:         quietLogger(),
		LookupOrg:      OrgLookup(cp.App),
		HooksPool:      2,
		ResolveBuild:   artifactResolver(root),
		CardDAVSources: CardDAVSources,
	})

	artifactDir, err := artifactResolver(root)(hash)
	if err != nil {
		t.Fatalf("resolve committed artifact: %v", err)
	}
	return mgr, p, artifactDir.Dir
}

// seedOrg provisions an org and inserts a user + one contact into its DB.
//
// The host no longer holds a tenant app object, so seeding opens the tenant's
// database directly — the test process is the host and has filesystem access.
// This must happen BEFORE the tenant process spawns, since two processes must
// not hold the same SQLite database open for writing. CreateOrg no longer
// materializes trees (the manager does, at load), so the seed materializes the
// artifact itself to run its migrations — the same idempotent symlink swap the
// manager will repeat at spawn.
func seedOrg(t *testing.T, p *Provisioner, root, artifactDir, slug, email, first, orgName string) {
	t.Helper()
	lock := map[string]string{"tinycld": "1.0.0", "@tinycld/contacts": "1.0.0"}
	if _, err := p.CreateOrg(slug, orgName, lock); err != nil {
		t.Fatalf("CreateOrg(%s): %v", slug, err)
	}

	orgDir := filepath.Join(root, "pb_orgs", slug)
	if err := materialize.MaterializeBuild(orgDir, artifactDir); err != nil {
		t.Fatalf("materialize %s: %v", slug, err)
	}
	app := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir:  filepath.Join(orgDir, "pb_data"),
		HideStartBanner: true,
	})
	if err := jsvm.Register(app, jsvm.Config{
		HooksDir:      filepath.Join(orgDir, "pb_hooks"),
		MigrationsDir: filepath.Join(orgDir, "pb_migrations"),
		HooksWatch:    false,
		Sandboxed:     true,
	}); err != nil {
		t.Fatalf("seed jsvm register(%s): %v", slug, err)
	}
	if err := app.Bootstrap(); err != nil {
		t.Fatalf("seed bootstrap(%s): %v", slug, err)
	}
	defer app.ResetBootstrapState()
	if err := app.RunAllMigrations(); err != nil {
		t.Fatalf("seed migrations(%s): %v", slug, err)
	}

	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatalf("users collection: %v", err)
	}
	user := core.NewRecord(users)
	user.Set("email", email)
	user.Set("password", "password123")
	if err := app.Save(user); err != nil {
		t.Fatalf("save user: %v", err)
	}

	cCol, err := app.FindCollectionByNameOrId("contacts")
	if err != nil {
		t.Fatalf("contacts collection: %v", err)
	}
	c := core.NewRecord(cCol)
	c.Set("owner", user.Id)
	c.Set("vcard_uid", "urn:uuid:"+slug+"-1")
	c.Set("first_name", first)
	c.Set("last_name", "Person")
	c.Set("email", email)
	c.Set("deleted_at", "")
	if err := app.Save(c); err != nil {
		t.Fatalf("save contact: %v", err)
	}
}

// carddavReport issues an addressbook-query REPORT against the org's CardDAV
// endpoint through the proxy and returns the multistatus body.
func carddavReport(t *testing.T, mgr *orgmanager.OrgManager, slug, email string) string {
	t.Helper()
	rec := doCarddavReport(t, mgr, slug, email)
	if rec.Code != http.StatusMultiStatus && rec.Code != http.StatusOK {
		t.Fatalf("%s CardDAV REPORT = %d, body:\n%s", slug, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// carddavStatus issues the same REPORT but returns the status code instead of
// requiring success — for the cross-org auth-denial probes.
func carddavStatus(t *testing.T, mgr *orgmanager.OrgManager, slug, email string) int {
	t.Helper()
	return doCarddavReport(t, mgr, slug, email).Code
}

func doCarddavReport(t *testing.T, mgr *orgmanager.OrgManager, slug, email string) *httptest.ResponseRecorder {
	t.Helper()
	inst, err := mgr.Get(context.Background(), slug)
	if err != nil {
		t.Fatalf("Get(%s): %v", slug, err)
	}

	body := `<?xml version="1.0" encoding="utf-8"?>
<C:addressbook-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:prop><D:getetag/><C:address-data/></D:prop>
</C:addressbook-query>`
	req := httptest.NewRequest("REPORT", "http://"+slug+".tinycld.test/carddav/u/ab/default/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/xml")
	req.Header.Set("Depth", "1")
	req.Header.Set("Authorization", "Basic "+basicAuth(email, "password123"))

	rec := httptest.NewRecorder()
	inst.Mux().ServeHTTP(rec, req)
	return rec
}

func basicAuth(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

// TestIntegration_MultiOrgCardDAV_Challenges401 confirms the tenant mounts its
// CardDAV routes from the config the host handed down, rather than 404ing.
func TestIntegration_MultiOrgCardDAV_Challenges401(t *testing.T) {
	root := t.TempDir()
	mgr, p, _ := setupCardDAVOrgs(t, root)
	defer mgr.Shutdown()

	if _, err := p.CreateOrg("acme", "Acme", map[string]string{"tinycld": "1.0.0", "@tinycld/contacts": "1.0.0"}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	inst, err := mgr.Get(context.Background(), "acme")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	rec := httptest.NewRecorder()
	inst.Mux().ServeHTTP(rec, httptest.NewRequest("PROPFIND", "http://acme.tinycld.test/carddav/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth CardDAV = %d, want 401 (route must be mounted from config)", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("WWW-Authenticate"), "Basic") {
		t.Errorf("missing Basic challenge header: %q", rec.Header().Get("WWW-Authenticate"))
	}
}
