package orgmanager

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"tinycld.org/multitenant/internal/progcache"
	"tinycld.org/multitenant/internal/store"
)

func buildTestManager(t *testing.T) (*OrgManager, string) {
	t.Helper()
	root := t.TempDir()

	s := store.New(root)
	if err := s.Publish("@tinycld/core", "1.0.0", map[string][]byte{
		"server/main.pb.js":      []byte("routerAdd('GET','/ping',(e)=>e.json(200,{ok:true}))"),
		"client/dist/index.html": []byte("<html></html>"),
	}); err != nil {
		t.Fatal(err)
	}

	mgr := New(Config{
		Root:      root,
		Store:     s,
		Programs:  progcache.New(),
		LookupOrg: stubLookup(map[string]OrgRecord{"acme": {Slug: "acme", Status: "active", Lockfile: []byte(`{"@tinycld/core":"1.0.0"}`)}}),
		HooksPool: 2,
	})
	return mgr, filepath.Join(root, "pb_orgs")
}

func TestOrgManager_GetLoadsAndServes(t *testing.T) {
	mgr, _ := buildTestManager(t)
	defer mgr.Shutdown()

	inst, err := mgr.Get("acme")
	if err != nil {
		t.Fatalf("Get(acme): %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	inst.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("health = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestOrgManager_GetUnknownOrgErrors(t *testing.T) {
	mgr, _ := buildTestManager(t)
	defer mgr.Shutdown()
	if _, err := mgr.Get("ghost"); err == nil {
		t.Fatal("expected error for unknown org")
	}
}

func TestOrgManager_ConcurrentGetLoadsOnce(t *testing.T) {
	mgr, _ := buildTestManager(t)
	defer mgr.Shutdown()

	var wg sync.WaitGroup
	insts := make([]*OrgInstance, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			in, err := mgr.Get("acme")
			if err != nil {
				t.Error(err)
				return
			}
			insts[i] = in
		}(i)
	}
	wg.Wait()
	for i := 1; i < 10; i++ {
		if insts[i] != insts[0] {
			t.Fatal("concurrent Get returned different instances — singleflight failed")
		}
	}
}

func TestOrgManager_EvictClosesInstance(t *testing.T) {
	mgr, _ := buildTestManager(t)
	defer mgr.Shutdown()
	first, _ := mgr.Get("acme")
	mgr.Evict("acme")
	second, err := mgr.Get("acme")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("expected a fresh instance after Evict")
	}
}
