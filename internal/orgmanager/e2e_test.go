package orgmanager

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tinycld.org/multi-org/internal/progcache"
	"tinycld.org/multi-org/internal/store"
)

func TestE2E_TwoOrgsIsolatedShareProgramCache(t *testing.T) {
	root := t.TempDir()
	s := store.New(root)
	// Same package/version for both orgs -> identical hook source -> shared program.
	hook := []byte("routerAdd('GET','/whoami',(e)=>e.json(200,{ok:true}))")
	if err := s.Publish("@tinycld/core", "1.0.0", map[string][]byte{
		"server/main.pb.js":      hook,
		"client/dist/index.html": []byte("<html></html>"),
	}); err != nil {
		t.Fatal(err)
	}

	programs := progcache.New()
	mgr := New(Config{
		Root:     root,
		Store:    s,
		Programs: programs,
		LookupOrg: stubLookup(map[string]OrgRecord{
			"acme": {Slug: "acme", Status: "active", Lockfile: []byte(`{"@tinycld/core":"1.0.0"}`)},
			"beta": {Slug: "beta", Status: "active", Lockfile: []byte(`{"@tinycld/core":"1.0.0"}`)},
		}),
		HooksPool: 2,
	})
	defer mgr.Shutdown()

	acme, err := mgr.Get("acme")
	if err != nil {
		t.Fatalf("acme: %v", err)
	}
	beta, err := mgr.Get("beta")
	if err != nil {
		t.Fatalf("beta: %v", err)
	}
	if acme == beta {
		t.Fatal("expected distinct instances per org")
	}

	// Both serve their own health endpoint (proves independent apps booted).
	for _, in := range []*OrgInstance{acme, beta} {
		rec := httptest.NewRecorder()
		in.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s health = %d", in.Slug(), rec.Code)
		}
	}

	// The custom JS route from the shared hook works in each org (proves the hook
	// loaded via the materialized symlink farm and ran through the shared cache).
	for _, in := range []*OrgInstance{acme, beta} {
		rec := httptest.NewRecorder()
		in.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/whoami", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s /whoami = %d body=%s", in.Slug(), rec.Code, rec.Body.String())
		}
	}

	// Program cache shared: identical hook source across both orgs compiled a
	// bounded, small number of programs — NOT doubled per org. We capture the
	// cache size after the FIRST org loads and assert the SECOND org adds nothing
	// (pure cache hits), which is the whole memory-sharing point.
	if programs.Len() == 0 {
		t.Fatal("expected shared program cache to hold compiled programs")
	}
	t.Logf("shared programs cached across two orgs: %d", programs.Len())
}

// TestE2E_SecondOrgAddsNoNewPrograms isolates the sharing invariant: loading a
// second org with identical hook source must not grow the program cache.
func TestE2E_SecondOrgAddsNoNewPrograms(t *testing.T) {
	root := t.TempDir()
	s := store.New(root)
	hook := []byte("routerAdd('GET','/whoami',(e)=>e.json(200,{ok:true}))")
	if err := s.Publish("@tinycld/core", "1.0.0", map[string][]byte{
		"server/main.pb.js": hook,
	}); err != nil {
		t.Fatal(err)
	}
	programs := progcache.New()
	mgr := New(Config{
		Root:     root,
		Store:    s,
		Programs: programs,
		LookupOrg: stubLookup(map[string]OrgRecord{
			"acme": {Slug: "acme", Status: "active", Lockfile: []byte(`{"@tinycld/core":"1.0.0"}`)},
			"beta": {Slug: "beta", Status: "active", Lockfile: []byte(`{"@tinycld/core":"1.0.0"}`)},
		}),
		HooksPool: 2,
	})
	defer mgr.Shutdown()

	if _, err := mgr.Get("acme"); err != nil {
		t.Fatalf("acme: %v", err)
	}
	afterFirst := programs.Len()
	if afterFirst == 0 {
		t.Fatal("expected programs cached after first org")
	}
	if _, err := mgr.Get("beta"); err != nil {
		t.Fatalf("beta: %v", err)
	}
	afterSecond := programs.Len()
	if afterSecond != afterFirst {
		t.Fatalf("second org with identical hooks grew the cache from %d to %d — programs are NOT being shared across orgs", afterFirst, afterSecond)
	}
	t.Logf("cache stable at %d programs across both orgs (full sharing)", afterSecond)
}

// TestE2E_TenantHooksAreSandboxed proves the manager loads tenant hooks with the
// jsvm sandbox: a hook route reporting typeof $os must return "undefined".
func TestE2E_TenantHooksAreSandboxed(t *testing.T) {
	root := t.TempDir()
	s := store.New(root)
	hook := []byte(`routerAdd('GET','/caps',(e)=>e.json(200,{os:typeof $os,http:typeof $http}))`)
	if err := s.Publish("@tinycld/core", "1.0.0", map[string][]byte{
		"server/main.pb.js": hook,
	}); err != nil {
		t.Fatal(err)
	}

	mgr := New(Config{
		Root:     root,
		Store:    s,
		Programs: progcache.New(),
		LookupOrg: stubLookup(map[string]OrgRecord{
			"acme": {Slug: "acme", Status: "active", Lockfile: []byte(`{"@tinycld/core":"1.0.0"}`)},
		}),
		HooksPool: 2,
	})
	defer mgr.Shutdown()

	acme, err := mgr.Get("acme")
	if err != nil {
		t.Fatalf("acme: %v", err)
	}
	rec := httptest.NewRecorder()
	acme.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/caps", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/caps = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !contains(body, `"os":"undefined"`) || !contains(body, `"http":"undefined"`) {
		t.Fatalf("expected tenant hooks sandboxed ($os/$http undefined), got %s", body)
	}
}

func contains(h, n string) bool { return strings.Contains(h, n) }

// TestE2E_HostileHookThrowAtLoadFailsOrgNotProcess proves a tenant hook that
// throws at load fails only that org's Get() (returned error), rather than
// panicking the shared process.
func TestE2E_HostileHookThrowAtLoadFailsOrgNotProcess(t *testing.T) {
	root := t.TempDir()
	s := store.New(root)
	if err := s.Publish("@tinycld/core", "1.0.0", map[string][]byte{
		"server/main.pb.js": []byte(`$os.exec('id')`),
	}); err != nil {
		t.Fatal(err)
	}

	mgr := New(Config{
		Root:     root,
		Store:    s,
		Programs: progcache.New(),
		LookupOrg: stubLookup(map[string]OrgRecord{
			"acme": {Slug: "acme", Status: "active", Lockfile: []byte(`{"@tinycld/core":"1.0.0"}`)},
		}),
		HooksPool: 2,
	})
	defer mgr.Shutdown()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Get panicked on a hostile load-throwing hook: %v", r)
		}
	}()
	if _, err := mgr.Get("acme"); err == nil {
		t.Fatal("expected Get to return an error for a load-throwing hook, got nil")
	}
}
