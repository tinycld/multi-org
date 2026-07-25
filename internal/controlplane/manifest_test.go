package controlplane

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"tinycld.org/multi-org/internal/lockfile"
)

const contactsManifestTS = `
const manifest = {
    name: 'Contacts',
    slug: 'contacts',
    version: '0.1.1',
    description: 'Your personal contacts',
    hooks: { directory: 'pb-hooks' },
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
            simple: { EMAIL: 'email', TEL: 'phone' },
            revField: 'updated',
        },
    },
}
export default manifest
`

func TestEmitManifestJSON_ParsesTSDefaultExport(t *testing.T) {
	files := map[string][]byte{
		"manifest.ts":                  []byte(contactsManifestTS),
		"pb-hooks/contacts.pb.ts":      []byte(`onRecordCreate(() => {}, 'contacts')`),
		"pb-migrations/x_create.js":    []byte(`migrate(() => {})`),
	}
	out, err := emitManifestJSON(files)
	if err != nil {
		t.Fatalf("emitManifestJSON: %v", err)
	}
	raw, ok := out[manifestJSONFile]
	if !ok {
		t.Fatal("manifest.json not emitted")
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("manifest.json invalid: %v", err)
	}
	if m["slug"] != "contacts" {
		t.Errorf("slug = %v", m["slug"])
	}
	cd, ok := m["carddav"].(map[string]any)
	if !ok {
		t.Fatalf("carddav block missing/typed wrong: %T", m["carddav"])
	}
	if cd["collection"] != "contacts" || cd["uidField"] != "vcard_uid" {
		t.Errorf("carddav block wrong: %+v", cd)
	}
	// Other files pass through untouched.
	if _, ok := out["pb-hooks/contacts.pb.ts"]; !ok {
		t.Error("pb-hooks file dropped")
	}
}

func TestEmitManifestJSON_NoManifestIsNoop(t *testing.T) {
	files := map[string][]byte{"pb-migrations/x.js": []byte("migrate(()=>{})")}
	out, err := emitManifestJSON(files)
	if err != nil {
		t.Fatalf("emitManifestJSON: %v", err)
	}
	if _, ok := out[manifestJSONFile]; ok {
		t.Error("manifest.json should not be emitted without a manifest source")
	}
}

func TestCardDAVSources_ReadsManifestJSON(t *testing.T) {
	// Emit manifest.json the way publish does, then write it to a package dir and
	// resolve sources from it — the full host read path.
	files, err := emitManifestJSON(map[string][]byte{"manifest.ts": []byte(contactsManifestTS)})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, manifestJSONFile), files[manifestJSONFile], 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	sources, err := CardDAVSources([]lockfile.ResolvedPackage{{Name: "contacts", Version: "0.1.1", Dir: dir}})
	if err != nil {
		t.Fatalf("CardDAVSources: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("got %d sources, want 1", len(sources))
	}
	s := sources[0]
	if s.Slug != "contacts" || s.Collection != "contacts" || s.UIDField != "vcard_uid" {
		t.Errorf("source basics wrong: %+v", s)
	}
	if s.SoftDeleteField != "deleted_at" || s.Sort != "-updated" {
		t.Errorf("source fields wrong: %+v", s)
	}
	if s.VCard.Name.Given != "first_name" || s.VCard.Simple["EMAIL"] != "email" {
		t.Errorf("vcard map wrong: %+v", s.VCard)
	}
}

func TestCardDAVSources_SkipsPackagesWithoutCardDAV(t *testing.T) {
	dir := t.TempDir()
	// A manifest.json with no carddav block.
	if err := os.WriteFile(filepath.Join(dir, manifestJSONFile), []byte(`{"slug":"calc"}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Another package with no manifest.json at all.
	empty := t.TempDir()

	sources, err := CardDAVSources([]lockfile.ResolvedPackage{
		{Name: "calc", Dir: dir},
		{Name: "nothing", Dir: empty},
	})
	if err != nil {
		t.Fatalf("CardDAVSources: %v", err)
	}
	if len(sources) != 0 {
		t.Errorf("expected no sources, got %d", len(sources))
	}
}
