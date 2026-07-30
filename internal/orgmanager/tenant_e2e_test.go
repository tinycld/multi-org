package orgmanager

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"tinycld.org/multi-org/internal/store"
	"tinycld.org/multi-org/internal/testsupport"
)

// These tests spawn the real serve-org binary. They are the only coverage of
// the child's own wiring — listener injection, readiness reporting, the jsvm
// sandbox, drain-on-SIGTERM — none of which the fake spawner exercises.
//
// They cost a build plus a PocketBase boot per case, so they are skipped under
// -short.

// buildTenantBinary compiles cmd/serve-org once per test run (shared across
// packages via testsupport).
func buildTenantBinary(t *testing.T) string {
	t.Helper()
	return testsupport.BuildTenantBinary(t)
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
			"acme": {Slug: "acme", Status: "active", DisplayName: "Acme Incorporated", Lockfile: []byte(`{"@tinycld/core":"1.0.0"}`)},
		}),
		OrgURL:     func(slug string) string { return "https://" + slug + ".tenants.example.test" },
		BaseDomain: "tenants.example.test",
	})
	t.Cleanup(mgr.Shutdown)
	return mgr
}

// newRealMultiOrgManager is newRealManager for cases that need more than one
// org resident at once — cross-tenant probes, where the whole point is that a
// second org's data exists on disk beside the first.
//
// Unlike newConfinedManager it needs no root and no Linux: the properties it
// serves are enforced in-process (the database connector), not by the kernel.
func newRealMultiOrgManager(t *testing.T, orgs map[string]string) *OrgManager {
	t.Helper()
	bin := buildTenantBinary(t)
	root := t.TempDir()

	s := store.New(root)
	lookup := map[string]OrgRecord{}
	for slug, hook := range orgs {
		pkg := "@tinycld/" + slug
		if err := s.Publish(pkg, "1.0.0", map[string][]byte{
			"server/main.pb.js":      []byte(hook),
			"client/dist/index.html": []byte("<html></html>"),
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
		Root:         root,
		Store:        s,
		Spawner:      execSpawner{},
		TenantBinary: bin,
		Logger:       quietLogger(),
		HooksPool:    2,
		LookupOrg:    stubLookup(lookup),
	})
	t.Cleanup(mgr.Shutdown)
	return mgr
}

// Every org's auth emails interpolate {APP_URL} from Settings().Meta.AppURL.
// The router materializes the org's public URL into .runtime/app.json; the
// tenant must adopt it at boot, or every verification / password-reset /
// email-change link carries PocketBase's default http://localhost:8090.
func TestTenant_AdoptsMaterializedAppURL(t *testing.T) {
	mgr := newRealManager(t, `routerAdd('GET','/appurl',(e)=>e.json(200,{url:$app.settings().meta.appURL}))`)

	inst, err := mgr.Get(context.Background(), "acme")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	rec := httptest.NewRecorder()
	inst.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/appurl", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/appurl = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"url":"https://acme.tenants.example.test"`) {
		t.Fatalf("tenant settings did not adopt the materialized org URL: %s", rec.Body.String())
	}
}

// Org branding follows the AppURL rail: display_name → app.json orgName →
// Settings().Meta.AppName. That settings value is what the client's
// /api/org-info serves (document title, org avatar) and what PB interpolates
// into {APP_NAME} in auth emails.
func TestTenant_AdoptsMaterializedOrgName(t *testing.T) {
	mgr := newRealManager(t, `routerAdd('GET','/appname',(e)=>e.json(200,{name:$app.settings().meta.appName}))`)

	inst, err := mgr.Get(context.Background(), "acme")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	rec := httptest.NewRecorder()
	inst.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/appname", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/appname = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"name":"Acme Incorporated"`) {
		t.Fatalf("tenant settings did not adopt the materialized org name: %s", rec.Body.String())
	}
}

// A successful users auth must upsert this org's entry into the parent-domain
// tinycld_orgs switcher cookie (Set-Cookie through the router's proxy), and a
// failed auth must not. Drives the real chain: display_name + MT_BASE_DOMAIN →
// app.json → serve-org's OnRecordAuthRequest hook → Set-Cookie.
func TestTenant_SetsSwitcherCookieOnAuth(t *testing.T) {
	mgr := newRealManager(t, `routerAdd('POST','/mkuser',(e)=>{
        const col = $app.findCollectionByNameOrId('users')
        const r = new Record(col)
        r.setEmail('u@example.com')
        r.setPassword('Password123!')
        r.setVerified(true)
        $app.save(r)
        return e.json(200,{ok:true})
    })`)

	inst, err := mgr.Get(context.Background(), "acme")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	rec := httptest.NewRecorder()
	inst.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mkuser", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/mkuser = %d body=%s", rec.Code, rec.Body.String())
	}

	auth := func(password string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/collections/users/auth-with-password",
			strings.NewReader(`{"identity":"u@example.com","password":"`+password+`"}`))
		req.Header.Set("Content-Type", "application/json")
		inst.Mux().ServeHTTP(rec, req)
		return rec
	}

	good := auth("Password123!")
	if good.Code != http.StatusOK {
		t.Fatalf("auth = %d body=%s", good.Code, good.Body.String())
	}
	cookie := ""
	for _, c := range good.Result().Cookies() {
		if c.Name == "tinycld_orgs" {
			cookie = c.Value
			if c.Domain != "tenants.example.test" && c.Domain != ".tenants.example.test" {
				t.Fatalf("cookie domain = %q, want the parent domain", c.Domain)
			}
			if c.HttpOnly {
				t.Fatal("cookie must not be HttpOnly — the switcher UI reads document.cookie")
			}
		}
	}
	if cookie == "" {
		t.Fatalf("successful auth set no tinycld_orgs cookie; headers=%v", good.Result().Header)
	}
	decoded, err := url.QueryUnescape(cookie)
	if err != nil {
		t.Fatalf("unescape cookie: %v", err)
	}
	for _, want := range []string{`"slug":"acme"`, `"name":"Acme Incorporated"`} {
		if !strings.Contains(decoded, want) {
			t.Fatalf("cookie %s missing %s", decoded, want)
		}
	}
	// No URL: the cookie is writable by JS on sibling tenants, so a stored URL
	// is an attacker-controlled navigation target. The client derives the URL
	// from the slug and its own origin's parent domain instead.
	if strings.Contains(decoded, `"url"`) {
		t.Fatalf("cookie %s carries a url — the switcher target must be derived from the slug, never stored", decoded)
	}

	bad := auth("wrong-password")
	if bad.Code == http.StatusOK {
		t.Fatal("wrong password authenticated")
	}
	for _, c := range bad.Result().Cookies() {
		if c.Name == "tinycld_orgs" {
			t.Fatal("failed auth must not set the switcher cookie")
		}
	}
}

// Over the unix socket a tenant's RemoteAddr is empty, so unless it trusts the
// router's X-Forwarded-For header every request resolves to the same (empty)
// client IP and per-IP rate limiting is one shared bucket. The router
// materializes that trust into .runtime/app.json; the tenant must adopt it as
// Settings().TrustedProxy. This drives the whole chain: proxy header
// construction → materialization → adoption → PB's RealIP.
func TestTenant_ResolvesClientIPThroughForwardedChain(t *testing.T) {
	mgr := newRealManager(t, `routerAdd('GET','/ip',(e)=>e.json(200,{ip:e.realIP()}))`)

	inst, err := mgr.Get(context.Background(), "acme")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// Proxy mode: the LB's chain names the client; the router forwards it
	// verbatim and the tenant takes the rightmost entry.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ip", nil)
	req.RemoteAddr = "10.0.0.5:4711"
	req.Header.Set("X-Forwarded-For", "198.51.100.4")
	inst.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/ip = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ip":"198.51.100.4"`) {
		t.Fatalf("tenant did not resolve the forwarded client IP: %s", rec.Body.String())
	}
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

// An org must stay reachable through an Evict-then-immediate-traffic cycle —
// the Deploy path. socketPath(slug) is deterministic, so the replacement binds
// the same path the evicted child still holds; the old teardown then removes
// that path AFTER drain+kill, and without the ownership guard it deletes the
// NEW child's socket: every dial ENOENTs → 502 while the supervisor still
// thinks the instance is healthy, so nothing respawns it.
func TestTenant_EvictThenImmediateTrafficStaysReachable(t *testing.T) {
	mgr := newRealManager(t, `routerAdd('GET','/ping',(e)=>e.json(200,{ok:true}))`)

	first, err := mgr.Get(context.Background(), "acme")
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}

	// An evicted org with any open browser tab holds an SSE stream, so the
	// old child's drain runs its FULL budget — plenty of time for the
	// replacement to bind the same path. Model that by holding an in-flight
	// request; mimic Evict with a compressed drain so the test doesn't sit
	// through the production 10s window (Evict itself is just this map delete
	// plus shutdown with the production budgets).
	first.inflight.Add(1)
	mgr.mu.Lock()
	delete(mgr.orgs, "acme")
	mgr.mu.Unlock()
	teardownDone := make(chan struct{})
	go func() {
		defer close(teardownDone)
		first.shutdown(4*time.Second, 2*time.Second)
	}()

	// The replacement spawns while the evicted child is still draining — the
	// exact window the unlink race lives in.
	second, err := mgr.Get(context.Background(), "acme")
	if err != nil {
		first.inflight.Done()
		t.Fatalf("Get during teardown: %v", err)
	}
	if first.proc.Pid() == second.proc.Pid() {
		first.inflight.Done()
		t.Fatal("Evict did not produce a fresh process")
	}

	rec := httptest.NewRecorder()
	second.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusOK {
		first.inflight.Done()
		t.Fatalf("replacement not serving before teardown finished: %d", rec.Code)
	}

	// Release the stream; the old teardown proceeds to TERM, reap, and its
	// socket-removal step. The replacement must stay reachable through it.
	first.inflight.Done()
	<-teardownDone

	if _, err := os.Stat(second.sockPath); err != nil {
		t.Fatalf("the replacement's socket was removed by the old teardown: %v", err)
	}
	rec = httptest.NewRecorder()
	second.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("org went dark after the old teardown ran: health = %d body=%s", rec.Code, rec.Body.String())
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
//
// NOTE: this is defence-in-depth evidence, not the guard. The fork's sandbox
// empties process.env in every VM, so this stays green even if the spawner
// leaked os.Environ() into the child. The falsifiable guards are
// TestBuildCmd_EnvIsAllowlistOnly (spawn_exec_test.go) on the constructed env,
// and TestConfinement_ChildEnvironmentHoldsNoHostSecrets at the OS level.
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

	// Socket removal runs AFTER the child is reaped (the parent owns the
	// file's lifecycle now), so wait for the whole teardown, not just dead.
	select {
	case <-inst.torndown:
	case <-time.After(30 * time.Second):
		t.Fatal("teardown never completed after Evict")
	}

	// The socket must be cleaned up so the next spawn binds fresh.
	if _, err := os.Stat(inst.sockPath); !os.IsNotExist(err) {
		t.Fatalf("socket %s still present after eviction", inst.sockPath)
	}

	// And the OS process itself must be gone — signal 0 probes existence
	// without delivering anything. The manager has already reaped the child
	// (teardown completed), so a live pid here means Evict leaked the process.
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("tenant process %d still exists after eviction (kill 0 => %v)", pid, err)
	}
}
