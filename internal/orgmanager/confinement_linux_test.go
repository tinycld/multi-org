//go:build linux

package orgmanager

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tinycld.org/multi-org/internal/store"
)

// These tests assert the OS-level boundary this whole architecture exists to
// establish. They require Linux (namespaces) and root (uid switching, mounts),
// so they cannot run on a developer's macOS box — they are the reason the
// project needs Linux CI.
//
// Run: sudo go test ./internal/orgmanager/ -run TestConfinement -v

func requireConfinementEnv(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("confinement tests require root (uid switching, mount namespaces)")
	}
}

// newConfinedManager wires the real Linux spawner with uid separation enabled.
func newConfinedManager(t *testing.T, orgs map[string]string) *OrgManager {
	t.Helper()
	requireConfinementEnv(t)
	bin := buildTenantBinary(t)
	root := t.TempDir()

	s := store.New(root)
	lookup := map[string]OrgRecord{}
	for slug, hook := range orgs {
		pkg := "@tinycld/" + slug
		if err := s.Publish(pkg, "1.0.0", map[string][]byte{
			"server/main.pb.js": []byte(hook),
		}); err != nil {
			t.Fatal(err)
		}
		lookup[slug] = OrgRecord{
			Slug:     slug,
			Status:   "active",
			Lockfile: []byte(fmt.Sprintf(`{%q:"1.0.0"}`, pkg)),
		}
	}

	mgr := New(Config{
		Root:  root,
		Store: s,
		Spawner: &linuxSpawner{conf: LinuxConfinement{
			UIDBase:  60000,
			UIDRange: 500,
		}},
		TenantBinary: bin,
		Logger:       quietLogger(),
		HooksPool:    2,
		LookupOrg:    stubLookup(lookup),
	})
	t.Cleanup(mgr.Shutdown)
	return mgr
}

// The headline exploit: a sandboxed hook could ATTACH another org's SQLite file
// and read its secrets. Under per-process confinement that open must fail at
// the OS layer, not because a SQL statement was blocklisted.
func TestConfinement_CannotAttachAnotherOrgsDatabase(t *testing.T) {
	requireConfinementEnv(t)

	victimDB := ""
	mgr := newConfinedManager(t, map[string]string{
		"victim": `routerAdd('GET','/ping',(e)=>e.json(200,{ok:true}))`,
		"attacker": `routerAdd('GET','/attack',(e)=>{
			try {
				$app.db().newQuery("ATTACH DATABASE '" + e.request.url.query().get('path') + "' AS stolen").execute()
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
	victimDB = filepath.Join(mgr.cfg.Root, "pb_orgs", "victim", "pb_data", "data.db")
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
		t.Fatalf("CROSS-ORG BREACH: attacker attached the victim's database: %s", body)
	}
	if !strings.Contains(body, `"attached":false`) {
		t.Fatalf("unexpected response from the attack probe: %s", body)
	}
}

// Each tenant must run as its own uid, so the kernel enforces file separation
// even for capabilities we have not thought of.
func TestConfinement_TenantsRunAsDistinctNonRootUIDs(t *testing.T) {
	requireConfinementEnv(t)

	mgr := newConfinedManager(t, map[string]string{
		"alpha": `routerAdd('GET','/ping',(e)=>e.json(200,{ok:true}))`,
	})

	inst, err := mgr.Get(context.Background(), "alpha")
	if err != nil {
		t.Fatal(err)
	}

	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", inst.proc.Pid()))
	if err != nil {
		t.Fatalf("read child status: %v", err)
	}
	uidLine := ""
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "Uid:") {
			uidLine = line
			break
		}
	}
	if uidLine == "" {
		t.Fatal("could not read the child's uid")
	}
	if strings.Contains(uidLine, "\t0\t") {
		t.Fatalf("tenant is running as root: %q", uidLine)
	}
}

// The package store is shared and immutable; a tenant must not be able to
// rewrite code another org will execute.
func TestConfinement_PackageStoreIsReadOnly(t *testing.T) {
	requireConfinementEnv(t)

	mgr := newConfinedManager(t, map[string]string{
		"writer": `routerAdd('GET','/write',(e)=>{
			try {
				$os.writeFile('/tmp/should-not-work', 'x', 0644)
				e.json(200, {wrote: true})
			} catch (err) {
				e.json(200, {wrote: false, error: String(err)})
			}
		})`,
	})

	inst, err := mgr.Get(context.Background(), "writer")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	inst.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/write", nil))
	if strings.Contains(rec.Body.String(), `"wrote":true`) {
		t.Fatalf("tenant wrote to the host filesystem: %s", rec.Body.String())
	}
}

// The child must not be able to read the host's environment.
func TestConfinement_ChildEnvironmentHoldsNoHostSecrets(t *testing.T) {
	requireConfinementEnv(t)
	t.Setenv("MT_SUPERUSER_PASSWORD", "super-secret-value")
	t.Setenv("MT_TLS_KEY", "/etc/tls/private.key")

	mgr := newConfinedManager(t, map[string]string{
		"probe": `routerAdd('GET','/ping',(e)=>e.json(200,{ok:true}))`,
	})

	inst, err := mgr.Get(context.Background(), "probe")
	if err != nil {
		t.Fatal(err)
	}

	environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", inst.proc.Pid()))
	if err != nil {
		t.Fatalf("read child environ: %v", err)
	}
	for _, secret := range []string{"super-secret-value", "/etc/tls/private.key", "MT_SUPERUSER", "MT_TLS"} {
		if strings.Contains(string(environ), secret) {
			t.Fatalf("host secret %q leaked into the tenant environment", secret)
		}
	}
}
