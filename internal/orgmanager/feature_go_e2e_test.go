package orgmanager

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Feature Go reaches a tenant ONLY through the pinned menu, gated by the org's
// resolved package set (docs/SCOPE-tenant-feature-go.md): the members staged
// in the org's artifact decide which slugs land in packages.json, which
// serve-org uses to gate feature Go registration. The probe is the `$contacts`
// jsvm binding: it exists in a tenant's VMs iff contacts' RegisterTenant ran
// in that process, so a hook that reports `typeof $contacts` observes exactly
// "did this org get the feature's Go" — through the real serve-org binary,
// packages.json, and --packages-config.
func TestTenant_FeatureGoIsGatedByPackageSet(t *testing.T) {
	bin := buildTenantBinary(t)
	root := t.TempDir()

	probe := `routerAdd('GET','/feature-probe',(e)=>e.json(200,{contacts: typeof $contacts !== 'undefined'}))`
	files := map[string]string{
		"pb_public/index.html": "<html></html>",
		"pb_hooks/main.pb.js":  probe,
	}
	// The artifact stages only contacts' manifest — the tenant's contacts Go
	// is linked into the binary; the staged member is what puts "contacts" in
	// the org's resolved set.
	withPkg := buildArtifact(t, buildsRootFor(root), artifactSpec{
		Hash:    "withpkg1",
		Binary:  bin,
		Files:   files,
		Members: []artifactMember{{Slug: "contacts"}},
	})
	withoutPkg := buildArtifact(t, buildsRootFor(root), artifactSpec{
		Hash:   "withoutpkg1",
		Binary: bin,
		Files:  files,
	})

	mgr := New(Config{
		Root:      root,
		Spawner:   execSpawner{},
		Logger:    quietLogger(),
		HooksPool: 2,
		// The production hook lives in controlplane (which imports this
		// package, so it can't be imported here); slugsFromManifests reads the
		// same manifest.json slug from the artifact's staged member dirs.
		PackageSlugs: slugsFromManifests,
		ResolveBuild: resolveBuilds(map[string]BuildRef{
			"withpkg1":    withPkg,
			"withoutpkg1": withoutPkg,
		}),
		LookupOrg: stubLookup(map[string]OrgRecord{
			"withpkg":    {Slug: "withpkg", Status: "active", RecipeHash: "withpkg1"},
			"withoutpkg": {Slug: "withoutpkg", Status: "active", RecipeHash: "withoutpkg1"},
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
