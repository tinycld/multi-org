package orgmanager

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tinycld.org/multi-org/internal/lockfile"
	"tinycld.org/multi-org/internal/store"
)

// Feature Go reaches a tenant ONLY through the pinned menu, gated by the org's
// resolved package set (docs/SCOPE-tenant-feature-go.md). The probe is the
// `$contacts` jsvm binding: it exists in a tenant's VMs iff contacts'
// RegisterTenant ran in that process, so a hook that reports `typeof
// $contacts` observes exactly "did this org get the feature's Go" — through
// the real serve-org binary, packages.json, and --packages-config.
func TestTenant_FeatureGoIsGatedByPackageSet(t *testing.T) {
	bin := buildTenantBinary(t)
	root := t.TempDir()

	s := store.New(root)
	probe := `routerAdd('GET','/feature-probe',(e)=>e.json(200,{contacts: typeof $contacts !== 'undefined'}))`
	if err := s.Publish("@tinycld/core", "1.0.0", map[string][]byte{
		"client/dist/index.html": []byte("<html></html>"),
		"server/main.pb.js":      []byte(probe),
	}); err != nil {
		t.Fatal(err)
	}
	// The store package carries only manifest.json here — the tenant's contacts
	// Go is linked into the binary; the store copy is what puts "contacts" in
	// the org's resolved set.
	if err := s.Publish("@tinycld/contacts", "1.0.0", map[string][]byte{
		"manifest.json": []byte(`{"slug":"contacts"}`),
	}); err != nil {
		t.Fatal(err)
	}

	mgr := New(Config{
		Root:         root,
		Store:        s,
		Spawner:      execSpawner{},
		TenantBinary: bin,
		Logger:       quietLogger(),
		HooksPool:    2,
		// The production hook lives in controlplane (which imports this
		// package, so it can't be imported here); this mirror reads the same
		// manifest.json slug with the same name fallback.
		PackageSlugs: func(resolved []lockfile.ResolvedPackage) ([]string, error) {
			var slugs []string
			for _, pkg := range resolved {
				slug := pkg.Name
				if body, err := os.ReadFile(filepath.Join(pkg.Dir, "manifest.json")); err == nil {
					var m struct {
						Slug string `json:"slug"`
					}
					if json.Unmarshal(body, &m) == nil && m.Slug != "" {
						slug = m.Slug
					}
				}
				slugs = append(slugs, slug)
			}
			return slugs, nil
		},
		LookupOrg: stubLookup(map[string]OrgRecord{
			"withpkg": {Slug: "withpkg", Status: "active",
				Lockfile: []byte(`{"@tinycld/core":"1.0.0","@tinycld/contacts":"1.0.0"}`)},
			"withoutpkg": {Slug: "withoutpkg", Status: "active",
				Lockfile: []byte(`{"@tinycld/core":"1.0.0"}`)},
		}),
	})
	t.Cleanup(mgr.Shutdown)

	probeOrg := func(slug string) string {
		t.Helper()
		inst, err := mgr.Get(context.Background(), slug)
		if err != nil {
			t.Fatalf("Get(%s): %v", slug, err)
		}
		rec := httptest.NewRecorder()
		inst.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/feature-probe", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s probe = %d body=%s", slug, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	if body := probeOrg("withpkg"); !strings.Contains(body, `"contacts":true`) {
		t.Fatalf("org WITH contacts in its set has no $contacts binding — feature Go did not register: %s", body)
	}
	if body := probeOrg("withoutpkg"); !strings.Contains(body, `"contacts":false`) {
		t.Fatalf("org WITHOUT contacts in its set got the $contacts binding — the gate is open: %s", body)
	}
}
