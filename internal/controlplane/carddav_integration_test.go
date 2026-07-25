package controlplane

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"

	"tinycld.org/multi-org/internal/orgmanager"
	"tinycld.org/multi-org/internal/progcache"
	"tinycld.org/multi-org/internal/store"
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

// TestIntegration_MultiOrgCardDAV drives the full multi-org CardDAV path:
// publish a contacts package (carddav manifest + schema) → provision an org →
// seed a user/org/contact in that tenant DB → PROPFIND/REPORT the org's CardDAV
// endpoint through inst.Mux() and confirm the seeded contact is returned as a
// vCard. A second org is provisioned and confirmed to serve ONLY its own
// contact — the multiplex proof.
func TestIntegration_MultiOrgCardDAV(t *testing.T) {
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

	// Publish via PublishPackage so emitManifestJSON runs (manifest.ts → manifest.json).
	files := map[string][]byte{
		"manifest.ts":                          []byte(contactsCardDAVManifest),
		"pb-migrations/1700000000_contacts.js": []byte(contactsSchemaMigration),
	}
	if err := p.PublishPackage("contacts", "1.0.0", files, "official"); err != nil {
		t.Fatalf("PublishPackage: %v", err)
	}

	mgr := orgmanager.New(orgmanager.Config{
		Root:           root,
		Store:          s,
		Programs:       progcache.New(),
		LookupOrg:      OrgLookup(cp.App),
		HooksPool:      2,
		CardDAVSources: CardDAVSources,
	})
	defer mgr.Shutdown()

	// Two orgs, each with its own contact, prove independent backends.
	seedOrg(t, p, mgr, "acme", "alice@acme.test", "Alice", "Acme")
	seedOrg(t, p, mgr, "globex", "bob@globex.test", "Bob", "Globex")

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
}

// seedOrg provisions an org and inserts a user + one contact owned by that user
// into the tenant's DB via the tenant app (driving records directly is the
// standard PB integration-test approach). Single-org: the contact's owner is the
// user's id directly — no membership row.
func seedOrg(t *testing.T, p *Provisioner, mgr *orgmanager.OrgManager, slug, email, first, orgName string) {
	t.Helper()
	if _, err := p.CreateOrg(slug, orgName, map[string]string{"contacts": "1.0.0"}); err != nil {
		t.Fatalf("CreateOrg(%s): %v", slug, err)
	}
	inst, err := mgr.Get(slug)
	if err != nil {
		t.Fatalf("Get(%s): %v", slug, err)
	}
	app := inst.App()

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

	cCol, _ := app.FindCollectionByNameOrId("contacts")
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
// endpoint through inst.Mux() and returns the multistatus body.
func carddavReport(t *testing.T, mgr *orgmanager.OrgManager, slug, email string) string {
	t.Helper()
	inst, err := mgr.Get(slug)
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

	if rec.Code != http.StatusMultiStatus && rec.Code != http.StatusOK {
		t.Fatalf("%s CardDAV REPORT = %d, body:\n%s", slug, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func basicAuth(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

// TestIntegration_MultiOrgCardDAV_Challenges401 confirms the CardDAV endpoint
// challenges for auth (route mounted from config) rather than 404ing.
func TestIntegration_MultiOrgCardDAV_Challenges401(t *testing.T) {
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
	files := map[string][]byte{
		"manifest.ts":                          []byte(contactsCardDAVManifest),
		"pb-migrations/1700000000_contacts.js": []byte(contactsSchemaMigration),
	}
	if err := p.PublishPackage("contacts", "1.0.0", files, "official"); err != nil {
		t.Fatalf("PublishPackage: %v", err)
	}
	if _, err := p.CreateOrg("acme", "Acme", map[string]string{"contacts": "1.0.0"}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	mgr := orgmanager.New(orgmanager.Config{
		Root: root, Store: s, Programs: progcache.New(),
		LookupOrg: OrgLookup(cp.App), HooksPool: 2, CardDAVSources: CardDAVSources,
	})
	defer mgr.Shutdown()

	inst, err := mgr.Get("acme")
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
