package controlplane

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"tinycld.org/multi-org/internal/davconfig"
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
		"manifest.ts":               []byte(contactsManifestTS),
		"pb-hooks/contacts.pb.ts":   []byte(`onRecordCreate(() => {}, 'contacts')`),
		"pb-migrations/x_create.js": []byte(`migrate(() => {})`),
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

// driveManifestTS mirrors the `webdav` block drive ships.
const driveManifestTS = `
const manifest = {
    name: 'Drive',
    slug: 'drive',
    version: '0.2.2',
    description: 'Cloud file storage, with WebDAV',
    hooks: { directory: 'pb-hooks' },
    webdav: {
        prefix: '/drive',
        collection: 'drive_items',
        fields: {
            name: 'name',
            parent: 'parent',
            isFolder: 'is_folder',
            size: 'size',
            mimeType: 'mime_type',
            file: 'file',
            owner: 'created_by',
            updated: 'updated',
        },
    },
}
export default manifest
`

func TestWebDAVSources_ReadsManifestJSON(t *testing.T) {
	files, err := emitManifestJSON(map[string][]byte{"manifest.ts": []byte(driveManifestTS)})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, manifestJSONFile), files[manifestJSONFile], 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	sources, err := WebDAVSources([]lockfile.ResolvedPackage{{Name: "drive", Version: "0.2.2", Dir: dir}})
	if err != nil {
		t.Fatalf("WebDAVSources: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("got %d sources, want 1", len(sources))
	}
	s := sources[0]
	if s.Slug != "drive" || s.Prefix != "/drive" || s.Collection != "drive_items" {
		t.Errorf("source basics wrong: %+v", s)
	}
	if s.Fields.Name != "name" || s.Fields.Parent != "parent" || s.Fields.IsFolder != "is_folder" {
		t.Errorf("tree fields wrong: %+v", s.Fields)
	}
	if s.Fields.Owner != "created_by" || s.Fields.File != "file" || s.Fields.Size != "size" {
		t.Errorf("blob/owner fields wrong: %+v", s.Fields)
	}
	// Go hooks cannot cross the process boundary. Authorization no longer rides
	// here — core evaluates the collection's own PB rules, which DO travel in
	// the schema — but quota and versioning still do, so a tenant-served write
	// skips both. Asserting it keeps that limitation visible.
	if s.Hooks.CheckQuota != nil || s.Hooks.BeforeOverwrite != nil {
		t.Error("Sources read from a manifest must carry no Go hooks")
	}
}

func TestWebDAVSources_SkipsPackagesWithoutWebDAV(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, manifestJSONFile), []byte(`{"slug":"contacts"}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	empty := t.TempDir()

	sources, err := WebDAVSources([]lockfile.ResolvedPackage{
		{Name: "contacts", Dir: dir},
		{Name: "nothing", Dir: empty},
	})
	if err != nil {
		t.Fatalf("WebDAVSources: %v", err)
	}
	if len(sources) != 0 {
		t.Errorf("expected no sources, got %d", len(sources))
	}
}

// A Source that survives the wire round-trip must still build a FileSystem —
// i.e. the three mirrored struct definitions actually agree.
func TestWebDAVSources_RoundTripThroughWire(t *testing.T) {
	files, err := emitManifestJSON(map[string][]byte{"manifest.ts": []byte(driveManifestTS)})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, manifestJSONFile), files[manifestJSONFile], 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	sources, err := WebDAVSources([]lockfile.ResolvedPackage{{Name: "drive", Dir: dir}})
	if err != nil {
		t.Fatalf("WebDAVSources: %v", err)
	}

	decoded := davconfig.DecodeWebDAV(davconfig.EncodeWebDAV(sources))
	if len(decoded) != 1 {
		t.Fatalf("round trip produced %d sources, want 1", len(decoded))
	}
	if !reflect.DeepEqual(sources[0], decoded[0]) {
		t.Fatalf("round trip changed the Source:\n before %+v\n after  %+v", sources[0], decoded[0])
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
