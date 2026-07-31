package controlplane

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"tinycld.org/core/tenantcfg"
	"tinycld.org/multi-org/internal/lockfile"
	"tinycld.org/multi-org/internal/manifesteval"
)

// assertFullyPopulated walks v's exported fields recursively and fails for any
// zero-valued one. Func-typed fields are skipped: Go callbacks cannot cross a
// process boundary by design, so they are deliberately not wire-carried.
//
// This is what makes the DeepEqual round-trips below capable of failing: a
// field that is zero on BOTH sides of encode/decode compares equal, so a field
// added to core's Source but forgotten in the wire mirror would round-trip
// "equal" while a tenant silently loses the config. Forcing the fixture to
// populate every field means a new core field first fails here (update the
// fixture manifest to set it), after which a missing wire mapping fails the
// DeepEqual.
func assertFullyPopulated(t *testing.T, v any) {
	t.Helper()
	walkPopulated(t, reflect.ValueOf(v), reflect.TypeOf(v).Name())
}

func walkPopulated(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	switch v.Kind() {
	case reflect.Func:
		return
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			walkPopulated(t, v.Field(i), path+"."+f.Name)
		}
	default:
		if v.IsZero() {
			t.Errorf("%s is zero — populate it in the fixture manifest so the round-trip can prove the wire carries it", path)
		}
	}
}

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

// A manifest is untrusted package JS. Without an interrupt, `while(true){}`
// spins the POST /api/store/packages goroutine forever and nothing can
// recover it — the "pure object literal" the evaluator expects is an
// assumption about input, not an enforced property. (The evaluator itself
// lives in internal/manifesteval; this exercises it through the same
// entry emitManifestJSON uses.)
func TestEvalManifest_InterruptsRunawayScript(t *testing.T) {
	orig := manifesteval.Timeout
	manifesteval.Timeout = 200 * time.Millisecond
	t.Cleanup(func() { manifesteval.Timeout = orig })

	errCh := make(chan error, 1)
	go func() {
		_, err := manifesteval.EvalJSON("const manifest = {}\nwhile (true) {}\nexport default manifest", "manifest.ts")
		errCh <- err
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected a timeout error for a runaway manifest")
		}
	case <-time.After(manifesteval.Timeout + 5*time.Second):
		t.Fatal("EvalJSON never returned for a runaway script")
	}
}

// Size caps close the cheap paths regardless of evaluator: an oversized
// manifest source is refused before eval, and an eval that materializes an
// oversized export is refused before it is stored. (Memory isolation proper
// is the subprocess evaluator serve-multi wires — internal/manifesteval.)
func TestEmitManifestJSON_RefusesOversizedSource(t *testing.T) {
	huge := make([]byte, manifestSourceMaxBytes+1)
	for i := range huge {
		huge[i] = ' '
	}
	copy(huge, []byte("export default {name: 'x'}"))

	_, err := emitManifestJSON(map[string][]byte{"manifest.ts": huge})
	if err == nil {
		t.Fatal("oversized manifest source accepted")
	}
}

func TestEmitManifestJSON_RefusesOversizedExport(t *testing.T) {
	// ~64 MiB string from a few bytes of source: the classic doubling bomb,
	// small enough to stay allocated briefly in a test.
	src := `
let s = 'xxxxxxxxxxxxxxxx'
for (let i = 0; i < 22; i++) s += s
export default { name: 'bomb', blob: s }
`
	_, err := emitManifestJSON(map[string][]byte{"manifest.ts": []byte(src)})
	if err == nil {
		t.Fatal("a manifest exporting tens of megabytes was accepted into the store")
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
        trash: {
            collection: 'drive_item_state',
            itemField: 'item',
            userField: 'user',
            trashedAtField: 'trashed_at',
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

// A webdav block that omits `prefix` must mount inside the reserved /dav
// namespace, not at the site root: an empty prefix registers Any("") +
// Any("/{path...}") — a site-wide Basic-Auth catch-all in front of the SPA.
// CalDAV has had this default since it shipped; WebDAV never did.
func TestWebDAVSources_DefaultsPrefixIntoReservedNamespace(t *testing.T) {
	dir := writeManifestPkg(t, `
const manifest = {
    name: 'Files', slug: 'files', version: '1.0.0', description: 'files',
    webdav: { collection: 'file_items', fields: { name: 'name', parent: 'parent' } },
}
export default manifest
`)

	sources, err := WebDAVSources([]lockfile.ResolvedPackage{{Name: "files", Dir: dir}})
	if err != nil {
		t.Fatalf("WebDAVSources: %v", err)
	}
	if got := sources[0].Prefix; got != "/dav/files" {
		t.Errorf("prefix = %q, want the reserved-namespace default /dav/files", got)
	}
}

// M8: a manifest claiming a reserved namespace shadows infrastructure — /api
// puts a Basic-Auth DAV handler in front of PocketBase's entire REST API, /_
// in front of its dashboard, /.well-known in front of protocol discovery. A
// hostile or sloppy package must not be able to intercept those.
func TestWebDAVSources_RejectsReservedNamespaces(t *testing.T) {
	for _, bad := range []string{"/api", "/api/v2", "/_", "/_/x", "/.well-known", "/.well-known/caldav"} {
		dir := writeManifestPkg(t, `
const manifest = {
    name: 'Files', slug: 'files', version: '1.0.0', description: 'files',
    webdav: { prefix: '`+bad+`', collection: 'file_items', fields: { name: 'name' } },
}
export default manifest
`)
		_, err := WebDAVSources([]lockfile.ResolvedPackage{{Name: "files", Dir: dir}})
		if err == nil {
			t.Errorf("prefix %q accepted — it shadows a reserved namespace", bad)
			continue
		}
		if !strings.Contains(err.Error(), "files") {
			t.Errorf("prefix %q: error %q does not name the package", bad, err)
		}
	}
}

// A malformed prefix (no leading slash, bare root, trailing slash) would mount
// somewhere other than the operator intended — reject it at load with an error
// naming the package instead of letting the router mount it.
func TestWebDAVSources_RejectsMalformedPrefix(t *testing.T) {
	for _, bad := range []string{"drive", "/", "/drive/"} {
		dir := writeManifestPkg(t, `
const manifest = {
    name: 'Files', slug: 'files', version: '1.0.0', description: 'files',
    webdav: { prefix: '`+bad+`', collection: 'file_items', fields: { name: 'name' } },
}
export default manifest
`)
		_, err := WebDAVSources([]lockfile.ResolvedPackage{{Name: "files", Dir: dir}})
		if err == nil {
			t.Errorf("prefix %q: expected an error, got none", bad)
			continue
		}
		if !strings.Contains(err.Error(), "files") {
			t.Errorf("prefix %q: error %q does not name the package", bad, err)
		}
	}
}

// Two packages claiming one prefix is a boot panic inside the tenant (duplicate
// route registration). Fail the load instead, naming both packages.
func TestWebDAVSources_RejectsDuplicatePrefixes(t *testing.T) {
	manifest := func(slug string) string {
		return `
const manifest = {
    name: 'X', slug: '` + slug + `', version: '1.0.0', description: 'x',
    webdav: { prefix: '/dav/files', collection: '` + slug + `_items', fields: { name: 'name' } },
}
export default manifest
`
	}
	dirA := writeManifestPkg(t, manifest("aaa"))
	dirB := writeManifestPkg(t, manifest("bbb"))

	_, err := WebDAVSources([]lockfile.ResolvedPackage{
		{Name: "aaa", Dir: dirA},
		{Name: "bbb", Dir: dirB},
	})
	if err == nil {
		t.Fatal("expected duplicate-prefix error, got none")
	}
	if !strings.Contains(err.Error(), "aaa") || !strings.Contains(err.Error(), "bbb") {
		t.Errorf("error %q should name both packages", err)
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

	// Without this, a field missing from BOTH the wire mirror and the mapping
	// compares zero == zero below and the drop is invisible.
	assertFullyPopulated(t, sources[0])

	decoded := tenantcfg.DecodeWebDAV(tenantcfg.EncodeWebDAV(sources))
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

	// Both fixture sources are fully populated, so a wire-mapping drop cannot
	// hide behind zero == zero in the DeepEqual below.
	for _, s := range sources {
		assertFullyPopulated(t, s)
	}

	decoded := tenantcfg.DecodeQuota(tenantcfg.EncodeQuota(sources))
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
	if tenantcfg.DecodeQuota(tenantcfg.EncodeQuota(sources))[0].OwnerField != "" {
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

// calendarManifestTS mirrors the `caldav` block calendar ships. Kept verbatim
// from the real manifest so a drift between the two shows up here.
const calendarManifestTS = `
const manifest = {
    name: 'Calendar',
    slug: 'calendar',
    version: '0.2.1',
    description: 'Shared calendars, events and reminders',
    hooks: { directory: 'pb-hooks' },
    caldav: {
        calendarCollection: 'calendar_calendars',
        eventCollection: 'calendar_events',
        calendar: {
            name: 'name',
            description: 'description',
        },
        event: {
            calendar: 'calendar',
            uid: 'ical_uid',
            owner: 'created_by',
            title: 'title',
            description: 'description',
            location: 'location',
            start: 'start',
            end: 'end',
            allDay: 'all_day',
            recurrence: 'recurrence',
            guests: 'guests',
            reminder: 'reminder',
            busyStatus: 'busy_status',
            visibility: 'visibility',
            updated: 'updated',
            created: 'created',
            defaults: {
                busy_status: 'busy',
                visibility: 'default',
            },
        },
    },
}
export default manifest
`

// writeManifestPkg emits manifest.json from a manifest.ts source into a fresh
// temp dir, as the store does when a package is published.
func writeManifestPkg(t *testing.T, manifestTS string) string {
	t.Helper()
	files, err := emitManifestJSON(map[string][]byte{"manifest.ts": []byte(manifestTS)})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, manifestJSONFile), files[manifestJSONFile], 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return dir
}

func TestCalDAVSources_ReadsManifestJSON(t *testing.T) {
	dir := writeManifestPkg(t, calendarManifestTS)

	sources, err := CalDAVSources([]lockfile.ResolvedPackage{{Name: "calendar", Version: "0.2.1", Dir: dir}})
	if err != nil {
		t.Fatalf("CalDAVSources: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("got %d sources, want 1", len(sources))
	}
	s := sources[0]

	if s.Slug != "calendar" {
		t.Errorf("slug = %q", s.Slug)
	}
	if s.CalendarCollection != "calendar_calendars" || s.EventCollection != "calendar_events" {
		t.Errorf("collections wrong: %+v", s)
	}
	if s.Calendar.Name != "name" || s.Calendar.Description != "description" {
		t.Errorf("calendar map wrong: %+v", s.Calendar)
	}
	if s.Event.UID != "ical_uid" || s.Event.Calendar != "calendar" || s.Event.Owner != "created_by" {
		t.Errorf("event identity fields wrong: %+v", s.Event)
	}
	if s.Event.Start != "start" || s.Event.End != "end" || s.Event.AllDay != "all_day" {
		t.Errorf("event time fields wrong: %+v", s.Event)
	}
	// Recurrence is the field a flat config could not express on its own — the
	// codec translates it — so losing the mapping silently drops every RRULE.
	if s.Event.Recurrence != "recurrence" {
		t.Errorf("recurrence field = %q, want 'recurrence'", s.Event.Recurrence)
	}
	// Defaults cover schema-required fields a minimal VEVENT omits. A tenant
	// that loses them rejects those PUTs with "cannot be blank" — the live
	// failure this block exists to prevent.
	if s.Event.Defaults["busy_status"] != "busy" || s.Event.Defaults["visibility"] != "default" {
		t.Errorf("event defaults wrong: %+v", s.Event.Defaults)
	}
	// A Go func cannot cross the process boundary, so a Source read from a
	// manifest carries no error reporter. Access checks are unaffected: they
	// come from the collections' PB rules, which travel in the schema.
	if s.OnError != nil {
		t.Error("Sources read from a manifest must carry no Go callbacks")
	}
}

// calendar's manifest omits `prefix`, so the default has to fill in — core
// builds every calendar and object URL from it, and an empty prefix would mount
// the tree at the site root.
func TestCalDAVSources_DefaultsPrefix(t *testing.T) {
	dir := writeManifestPkg(t, calendarManifestTS)

	sources, err := CalDAVSources([]lockfile.ResolvedPackage{{Name: "calendar", Dir: dir}})
	if err != nil {
		t.Fatalf("CalDAVSources: %v", err)
	}
	if got := sources[0].Prefix; got != "/caldav" {
		t.Errorf("prefix = %q, want the /caldav default", got)
	}
}

func TestCalDAVSources_HonorsExplicitPrefix(t *testing.T) {
	dir := writeManifestPkg(t, `
const manifest = {
    name: 'Rooms', slug: 'rooms', version: '1.0.0', description: 'rooms',
    caldav: {
        prefix: '/rooms-cal',
        calendarCollection: 'room_calendars',
        eventCollection: 'room_events',
        calendar: { name: 'name' },
        event: { calendar: 'calendar', uid: 'uid', start: 'start', end: 'end' },
    },
}
export default manifest
`)

	sources, err := CalDAVSources([]lockfile.ResolvedPackage{{Name: "rooms", Dir: dir}})
	if err != nil {
		t.Fatalf("CalDAVSources: %v", err)
	}
	if got := sources[0].Prefix; got != "/rooms-cal" {
		t.Errorf("prefix = %q, want the manifest's own value", got)
	}
}

// Same boot-panic class as WebDAV: two caldav blocks on one prefix (e.g. both
// left to the /caldav default) must fail the load, not panic the tenant.
func TestCalDAVSources_RejectsDuplicatePrefixes(t *testing.T) {
	manifest := func(slug string) string {
		return `
const manifest = {
    name: 'X', slug: '` + slug + `', version: '1.0.0', description: 'x',
    caldav: {
        calendarCollection: '` + slug + `_calendars',
        eventCollection: '` + slug + `_events',
        calendar: { name: 'name' },
        event: { calendar: 'calendar', uid: 'uid', start: 'start', end: 'end' },
    },
}
export default manifest
`
	}
	dirA := writeManifestPkg(t, manifest("aaa"))
	dirB := writeManifestPkg(t, manifest("bbb"))

	_, err := CalDAVSources([]lockfile.ResolvedPackage{
		{Name: "aaa", Dir: dirA},
		{Name: "bbb", Dir: dirB},
	})
	if err == nil {
		t.Fatal("expected duplicate-prefix error, got none")
	}
	if !strings.Contains(err.Error(), "aaa") || !strings.Contains(err.Error(), "bbb") {
		t.Errorf("error %q should name both packages", err)
	}
}

func TestCalDAVSources_SkipsPackagesWithoutCalDAV(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, manifestJSONFile), []byte(`{"slug":"contacts"}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	empty := t.TempDir()

	sources, err := CalDAVSources([]lockfile.ResolvedPackage{
		{Name: "contacts", Dir: dir},
		{Name: "nothing", Dir: empty},
	})
	if err != nil {
		t.Fatalf("CalDAVSources: %v", err)
	}
	if len(sources) != 0 {
		t.Errorf("expected no sources, got %d", len(sources))
	}
}

// The three mirrored struct definitions (manifest block, wire type, core Source)
// must actually agree. A field added to one and forgotten in another silently
// stops being mapped, which for CalDAV means a property vanishing from every
// VEVENT a tenant serves.
func TestCalDAVSources_RoundTripThroughWire(t *testing.T) {
	dir := writeManifestPkg(t, calendarManifestTS)

	sources, err := CalDAVSources([]lockfile.ResolvedPackage{{Name: "calendar", Dir: dir}})
	if err != nil {
		t.Fatalf("CalDAVSources: %v", err)
	}

	// Without this, a field missing from BOTH the wire mirror and the mapping
	// compares zero == zero below and the drop is invisible. (OnError is a Go
	// func and deliberately not carried; the walker skips func fields.)
	assertFullyPopulated(t, sources[0])

	decoded := tenantcfg.DecodeCalDAV(tenantcfg.EncodeCalDAV(sources))
	if len(decoded) != 1 {
		t.Fatalf("round trip produced %d sources, want 1", len(decoded))
	}
	if !reflect.DeepEqual(sources[0], decoded[0]) {
		t.Fatalf("round trip changed the Source:\n before %+v\n after  %+v", sources[0], decoded[0])
	}
}

// Serialized JSON must survive a real file write/read, which is how the tenant
// actually receives it.
func TestCalDAVSources_SurvivesJSONFile(t *testing.T) {
	dir := writeManifestPkg(t, calendarManifestTS)
	sources, err := CalDAVSources([]lockfile.ResolvedPackage{{Name: "calendar", Dir: dir}})
	if err != nil {
		t.Fatalf("CalDAVSources: %v", err)
	}

	body, err := json.Marshal(tenantcfg.EncodeCalDAV(sources))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire []tenantcfg.CalDAVSource
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	assertFullyPopulated(t, sources[0])
	decoded := tenantcfg.DecodeCalDAV(wire)
	if !reflect.DeepEqual(sources[0], decoded[0]) {
		t.Fatalf("JSON round trip changed the Source:\n before %+v\n after  %+v", sources[0], decoded[0])
	}
}

// The CardDAV mirror (tenantcfg.Source) had no round-trip coverage at all —
// the same silent-drop class the WebDAV and CalDAV round-trips guard.
func TestCardDAVSources_RoundTripThroughWire(t *testing.T) {
	dir := writeManifestPkg(t, contactsManifestTS)

	sources, err := CardDAVSources([]lockfile.ResolvedPackage{{Name: "contacts", Dir: dir}})
	if err != nil {
		t.Fatalf("CardDAVSources: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("got %d sources, want 1", len(sources))
	}
	assertFullyPopulated(t, sources[0])

	decoded := tenantcfg.Decode(tenantcfg.Encode(sources))
	if len(decoded) != 1 {
		t.Fatalf("round trip produced %d sources, want 1", len(decoded))
	}
	if !reflect.DeepEqual(sources[0], decoded[0]) {
		t.Fatalf("round trip changed the Source:\n before %+v\n after  %+v", sources[0], decoded[0])
	}
}
