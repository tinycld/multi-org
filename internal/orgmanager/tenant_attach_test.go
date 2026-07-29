package orgmanager

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The cross-org ATTACH DATABASE exploit, driven through a real tenant's JS.
//
// There is an equivalent assertion in confinement_linux_test.go, but it is
// gated on Linux + root because it proves the *kernel* half of the boundary
// (uid separation and file modes). That gating is why the hole survived: the
// test never ran in ordinary CI, on a developer machine, or on macOS.
//
// This one proves the other half — the database connector refuses ATTACH
// outright — and therefore needs no privileges and runs everywhere. The two
// layers are deliberately independent: uid confinement is absent on developer
// hosts and when the router runs unprivileged, and it fails for any pair of
// orgs that share a uid, while the connector holds regardless.
func TestTenant_CannotAttachAnotherOrgsDatabase(t *testing.T) {
	mgr := newRealMultiOrgManager(t, map[string]string{
		"victim": `routerAdd('GET','/ping',(e)=>e.json(200,{ok:true}))`,
		"attacker": `routerAdd('GET','/attack',(e)=>{
			try {
				$app.nonconcurrentDB().newQuery("ATTACH DATABASE '" + e.request.url.query().get('path') + "' AS stolen").execute()
				e.json(200, {attached: true})
			} catch (err) {
				e.json(200, {attached: false, error: String(err)})
			}
		})`,
	})

	// Boot the victim so its database exists on disk.
	if _, err := mgr.Get(context.Background(), "victim"); err != nil {
		t.Fatalf("victim: %v", err)
	}
	victimDB := filepath.Join(mgr.cfg.Root, "pb_orgs", "victim", "pb_data", "data.db")
	if _, err := os.Stat(victimDB); err != nil {
		t.Fatalf("victim db not found: %v", err)
	}

	attacker, err := mgr.Get(context.Background(), "attacker")
	if err != nil {
		t.Fatalf("attacker: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/attack?path="+victimDB, nil)
	attacker.Mux().ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, `"attached":true`) {
		t.Fatalf("CROSS-ORG BREACH: a sandboxed tenant attached another org's "+
			"database through $app's raw-SQL surface: %s", body)
	}
	if !strings.Contains(body, `"attached":false`) {
		t.Fatalf("unexpected response from the attack probe: %s", body)
	}
}

// runInTransaction pins a single connection, which is the variant that defeated
// a naive fix (the pooled db() handle returned "no such table" for unrelated
// reasons and read as blocked).
func TestTenant_CannotAttachViaTransaction(t *testing.T) {
	mgr := newRealMultiOrgManager(t, map[string]string{
		"victim": `routerAdd('GET','/ping',(e)=>e.json(200,{ok:true}))`,
		"attacker": `routerAdd('GET','/attack',(e)=>{
			try {
				$app.runInTransaction((txApp) => {
					txApp.db().newQuery("ATTACH DATABASE '" + e.request.url.query().get('path') + "' AS stolen").execute()
				})
				e.json(200, {attached: true})
			} catch (err) {
				e.json(200, {attached: false, error: String(err)})
			}
		})`,
	})

	if _, err := mgr.Get(context.Background(), "victim"); err != nil {
		t.Fatalf("victim: %v", err)
	}
	victimDB := filepath.Join(mgr.cfg.Root, "pb_orgs", "victim", "pb_data", "data.db")

	attacker, err := mgr.Get(context.Background(), "attacker")
	if err != nil {
		t.Fatalf("attacker: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/attack?path="+victimDB, nil)
	attacker.Mux().ServeHTTP(rec, req)

	if body := rec.Body.String(); strings.Contains(body, `"attached":true`) {
		t.Fatalf("CROSS-ORG BREACH via runInTransaction: %s", body)
	}
}

// A tenant must not be able to create files outside its own directory either —
// ATTACH on a non-existent path is a write primitive, not only a read one.
func TestTenant_CannotCreateFilesViaAttach(t *testing.T) {
	mgr := newRealMultiOrgManager(t, map[string]string{
		"attacker": `routerAdd('GET','/attack',(e)=>{
			try {
				$app.nonconcurrentDB().newQuery("ATTACH DATABASE '" + e.request.url.query().get('path') + "' AS planted").execute()
				$app.nonconcurrentDB().newQuery("CREATE TABLE planted.x (v TEXT)").execute()
				e.json(200, {wrote: true})
			} catch (err) {
				e.json(200, {wrote: false, error: String(err)})
			}
		})`,
	})

	attacker, err := mgr.Get(context.Background(), "attacker")
	if err != nil {
		t.Fatalf("attacker: %v", err)
	}

	planted := filepath.Join(t.TempDir(), "planted.db")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/attack?path="+planted, nil)
	attacker.Mux().ServeHTTP(rec, req)

	if _, err := os.Stat(planted); err == nil {
		t.Fatalf("tenant created a file outside its org dir at %s", planted)
	}
}
