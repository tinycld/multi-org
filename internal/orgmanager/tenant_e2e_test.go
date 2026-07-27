package orgmanager

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tinycld.org/multi-org/internal/store"
)

// These tests spawn the real serve-org binary. They are the only coverage of
// the child's own wiring — listener injection, readiness reporting, the jsvm
// sandbox, drain-on-SIGTERM — none of which the fake spawner exercises.
//
// They cost a build plus a PocketBase boot per case, so they are skipped under
// -short.

var (
	tenantBinOnce sync.Once
	tenantBinPath string
	tenantBinErr  error
)

// buildTenantBinary compiles cmd/serve-org once per test run.
func buildTenantBinary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping real-binary test in short mode")
	}

	tenantBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "serve-org-bin")
		if err != nil {
			tenantBinErr = err
			return
		}
		out := filepath.Join(dir, "serve-org")
		cmd := exec.Command("go", "build", "-o", out, "tinycld.org/multi-org/cmd/serve-org")
		if combined, err := cmd.CombinedOutput(); err != nil {
			tenantBinErr = fmt.Errorf("build serve-org: %v\n%s", err, combined)
			return
		}
		// MkdirTemp creates the dir 0700. The confinement tests exec this
		// binary as a tenant uid, which needs traversal on the dir and read
		// on the binary.
		if err := os.Chmod(dir, 0o755); err != nil {
			tenantBinErr = err
			return
		}
		if err := os.Chmod(out, 0o755); err != nil {
			tenantBinErr = err
			return
		}
		tenantBinPath = out
	})

	if tenantBinErr != nil {
		t.Fatal(tenantBinErr)
	}
	return tenantBinPath
}

// newRealManager wires a manager over the real exec spawner and the real
// tenant binary, with one package whose hook source is given by the caller.
func newRealManager(t *testing.T, hookSource string) *OrgManager {
	t.Helper()
	bin := buildTenantBinary(t)
	root := t.TempDir()

	s := store.New(root)
	files := map[string][]byte{"client/dist/index.html": []byte("<html></html>")}
	if hookSource != "" {
		files["server/main.pb.js"] = []byte(hookSource)
	}
	if err := s.Publish("@tinycld/core", "1.0.0", files); err != nil {
		t.Fatal(err)
	}

	mgr := New(Config{
		Root:         root,
		Store:        s,
		Spawner:      execSpawner{},
		TenantBinary: bin,
		Logger:       quietLogger(),
		HooksPool:    2,
		LookupOrg: stubLookup(map[string]OrgRecord{
			"acme": {Slug: "acme", Status: "active", Lockfile: []byte(`{"@tinycld/core":"1.0.0"}`)},
		}),
	})
	t.Cleanup(mgr.Shutdown)
	return mgr
}

func TestTenant_ServesOverUnixSocket(t *testing.T) {
	mgr := newRealManager(t, `routerAdd('GET','/ping',(e)=>e.json(200,{ok:true}))`)

	inst, err := mgr.Get(context.Background(), "acme")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	rec := httptest.NewRecorder()
	inst.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("health = %d body=%s", rec.Code, rec.Body.String())
	}

	// The materialized hook must have loaded and registered its route.
	rec = httptest.NewRecorder()
	inst.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ping", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/ping = %d body=%s — the tenant's hook did not load", rec.Code, rec.Body.String())
	}
}

// Each org must get its own process; that separation is the whole deliverable.
func TestTenant_OrgsRunInSeparateProcesses(t *testing.T) {
	bin := buildTenantBinary(t)
	root := t.TempDir()

	s := store.New(root)
	if err := s.Publish("@tinycld/core", "1.0.0", map[string][]byte{
		"server/main.pb.js": []byte(`routerAdd('GET','/ping',(e)=>e.json(200,{ok:true}))`),
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
		LookupOrg: stubLookup(map[string]OrgRecord{
			"acme": {Slug: "acme", Status: "active", Lockfile: []byte(`{"@tinycld/core":"1.0.0"}`)},
			"beta": {Slug: "beta", Status: "active", Lockfile: []byte(`{"@tinycld/core":"1.0.0"}`)},
		}),
	})
	defer mgr.Shutdown()

	acme, err := mgr.Get(context.Background(), "acme")
	if err != nil {
		t.Fatalf("acme: %v", err)
	}
	beta, err := mgr.Get(context.Background(), "beta")
	if err != nil {
		t.Fatalf("beta: %v", err)
	}

	if acme.proc.Pid() == beta.proc.Pid() {
		t.Fatal("both orgs share a PID — they are not isolated")
	}
	for _, in := range []*OrgInstance{acme, beta} {
		rec := httptest.NewRecorder()
		in.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s health = %d", in.Slug(), rec.Code)
		}
	}
}

// The jsvm sandbox stays on inside the tenant as defence in depth.
func TestTenant_HooksAreSandboxed(t *testing.T) {
	mgr := newRealManager(t, `routerAdd('GET','/caps',(e)=>e.json(200,{os:typeof $os,http:typeof $http}))`)

	inst, err := mgr.Get(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	inst.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/caps", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/caps = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"os":"undefined"`) || !strings.Contains(body, `"http":"undefined"`) {
		t.Fatalf("expected $os/$http withheld from tenant hooks, got %s", body)
	}
}

// A hook that throws at load must fail exactly one org — reported cleanly to
// the host — and must not take down the router.
func TestTenant_HostileHookFailsOnlyThatOrg(t *testing.T) {
	mgr := newRealManager(t, `$os.exec('id')`)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("host panicked on a load-throwing tenant hook: %v", r)
		}
	}()

	_, err := mgr.Get(context.Background(), "acme")
	if err == nil {
		t.Fatal("expected an error for a load-throwing hook")
	}
	// The child's own diagnosis should reach the host, not just a timeout.
	if !strings.Contains(err.Error(), "jsvm") && !strings.Contains(err.Error(), "exec") {
		t.Fatalf("err = %v, want the child's reported reason", err)
	}
}

// The host must not hand its secrets to a tenant.
func TestTenant_DoesNotInheritHostSecrets(t *testing.T) {
	t.Setenv("MT_SUPERUSER_PASSWORD", "super-secret-value")

	mgr := newRealManager(t, `routerAdd('GET','/env',(e)=>e.json(200,{v:String(process.env.MT_SUPERUSER_PASSWORD)}))`)

	inst, err := mgr.Get(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	inst.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/env", nil))
	if strings.Contains(rec.Body.String(), "super-secret-value") {
		t.Fatalf("tenant could read a host secret: %s", rec.Body.String())
	}
}

// Evicting must actually stop the OS process, not just drop the reference.
func TestTenant_EvictTerminatesTheProcess(t *testing.T) {
	mgr := newRealManager(t, `routerAdd('GET','/ping',(e)=>e.json(200,{ok:true}))`)

	inst, err := mgr.Get(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	pid := inst.proc.Pid()

	mgr.Evict("acme")

	if !waitFor(30*time.Second, func() bool {
		select {
		case <-inst.dead:
			return true
		default:
			return false
		}
	}) {
		t.Fatal("tenant process was never reaped after Evict")
	}

	// The socket must be cleaned up so the next spawn binds fresh.
	if _, err := os.Stat(inst.sockPath); !os.IsNotExist(err) {
		t.Fatalf("socket %s still present after eviction", inst.sockPath)
	}
	_ = pid
}
