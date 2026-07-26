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
    quota: [
        { collection: 'drive_items', sizeField: 'size', ownerField: 'created_by' },
        { collection: 'drive_item_versions', sizeField: 'size', ownerField: 'created_by' },
    ],
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
	// Go hooks cannot cross the process boundary, so a Source read from a
	// manifest carries none. That is no longer a security gap: authorization
	// comes from the collection's own PB rules and quota from core/quota, both
	// of which a tenant gets. What remains is the version snapshot, so a
	// tenant-served overwrite does not archive the previous blob.
	if s.Hooks.BeforeOverwrite != nil {
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

// A quota source with no ownerField is shared data: it counts toward the org
// ceiling but must never be charged to a user. Losing that distinction on the
// wire would bill one person for a whole mailbox.
func TestQuotaSources_ReadsManifestJSON(t *testing.T) {
	files, err := emitManifestJSON(map[string][]byte{"manifest.ts": []byte(driveManifestTS)})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, manifestJSONFile), files[manifestJSONFile], 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	sources, err := QuotaSources([]lockfile.ResolvedPackage{{Name: "drive", Dir: dir}})
	if err != nil {
		t.Fatalf("QuotaSources: %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("got %d sources, want 2", len(sources))
	}
	for _, s := range sources {
		if s.Slug != "drive" || s.SizeField != "size" || s.OwnerField != "created_by" {
			t.Errorf("source wrong: %+v", s)
		}
	}

	decoded := davconfig.DecodeQuota(davconfig.EncodeQuota(sources))
	if !reflect.DeepEqual(sources, decoded) {
		t.Fatalf("wire round trip changed the sources:\n before %+v\n after  %+v", sources, decoded)
	}
}

func TestQuotaSources_PreservesUnownedSource(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, manifestJSONFile),
		[]byte(`{"slug":"mail","quota":[{"collection":"mail_messages","sizeField":"total_size"}]}`),
		0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	sources, err := QuotaSources([]lockfile.ResolvedPackage{{Name: "mail", Dir: dir}})
	if err != nil {
		t.Fatalf("QuotaSources: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("got %d sources, want 1", len(sources))
	}
	if sources[0].OwnerField != "" {
		t.Fatalf("OwnerField = %q, want empty — shared data has nobody to charge", sources[0].OwnerField)
	}
	if davconfig.DecodeQuota(davconfig.EncodeQuota(sources))[0].OwnerField != "" {
		t.Fatal("the wire round trip must preserve an absent owner")
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
