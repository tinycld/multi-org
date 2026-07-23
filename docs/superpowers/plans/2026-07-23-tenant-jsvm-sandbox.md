# Tenant JSVM Sandbox Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the fork's jsvm plugin a `Sandboxed` mode that installs a deny-by-default allowlist of JS bindings, and have the router run all untrusted tenant hooks and migrations under it — so a hostile org author cannot exec, read the host filesystem, read the process env, or make outbound HTTP.

**Architecture:** Add one nil-default `Sandboxed bool` to `jsvm.Config`. When set, both bind sites (`registerHooks` → `sharedBinds`, and `registerMigrations`) install only the capability-safe binds (`Core`/`Dbx`/`Security`/`Mails`/`Forms`/`Apis`/router+cron+hook registration + `$template`) and **omit** `BindOS`/`BindHTTP`/`BindFilesystem`/`BindFilepath`. The vendored `process` node-shim (which populates `process.env` from `os.Environ()`) is neutered per-VM in the sandboxed path. The router flips two call sites to `Sandboxed: true`: `orgmanager.load` (runtime) and `controlplane.bootstrapTenantOnce` (provision-time migrations, which run in the control-plane process).

**Tech Stack:** Go, `github.com/grafana/sobek`, the forked PocketBase jsvm plugin (`~/code/tinycld/pocketbase`, branch `feat/multitenant-fork`), the router module (`~/code/tinycld/multi-org`, branch `feat/operator-runnable`).

**Spec:** `docs/superpowers/specs/2026-07-23-tenant-jsvm-sandbox-design.md`

**Repo/branch preconditions:**
- `~/code/tinycld/pocketbase` MUST be on `feat/multitenant-fork` (the router's `../pocketbase` replace points at its working tree). Verify: `cd ~/code/tinycld/pocketbase && git branch --show-current` → `feat/multitenant-fork`.
- `~/code/tinycld/multi-org` on `feat/operator-runnable`.
- Fork edits and router edits are **separate repos with separate commits**. Commit in the fork first (the router builds against the fork working tree, so router tests won't pass until the fork change is in place — no push needed, the `replace` reads the working tree).

---

## Task 1: Add the `Sandboxed` config flag (fork)

**Files:**
- Modify: `~/code/tinycld/pocketbase/plugins/jsvm/jsvm.go` (Config struct, ~line 61)

- [ ] **Step 1: Add the field to `Config`**

In `~/code/tinycld/pocketbase/plugins/jsvm/jsvm.go`, inside the `Config` struct (just after the `ProgramSource ProgramSource` field, ~line 70), add:

```go
	// Sandboxed, when true, installs only the capability-safe JS bindings and
	// omits the host-capability bindings ($os, $http, $filesystem, $filepath)
	// from BOTH the hook and migration runtimes, and neuters process.env /
	// process.argv. Intended for running untrusted (multi-tenant) code.
	//
	// Default false preserves the full stock single-app API (byte-for-byte).
	Sandboxed bool
```

- [ ] **Step 2: Build to confirm it compiles**

Run: `cd ~/code/tinycld/pocketbase && go build ./plugins/jsvm/`
Expected: no output (success). The field is unused so far — that's fine.

- [ ] **Step 3: Commit**

```bash
cd ~/code/tinycld/pocketbase
git add plugins/jsvm/jsvm.go
git commit -m "feat(jsvm): add Sandboxed config flag (unused)"
```

---

## Task 2: Split the host-capability binds behind the flag (fork)

**Files:**
- Modify: `~/code/tinycld/pocketbase/plugins/jsvm/jsvm.go` (`registerMigrations` ~lines 204-231; `registerHooks`/`sharedBinds` ~lines 294-318)

The two bind sites currently call, unconditionally:
`BindCore, BindDbx, BindSecurity, BindOS, BindFilepath, BindHTTP, BindFilesystem, BindForms, BindMails` (migrations) plus `BindApis` (hooks). We gate the four host binds behind `!p.config.Sandboxed`.

- [ ] **Step 1: Gate the host binds in `registerMigrations`**

In `~/code/tinycld/pocketbase/plugins/jsvm/jsvm.go`, find this block in `registerMigrations` (~lines 212-221):

```go
		BindCore(vm)
		BindDbx(vm)
		BindSecurity(vm)
		BindOS(vm)
		BindFilepath(vm)
		BindHTTP(vm)
		BindFilesystem(vm)
		BindForms(vm)
		BindMails(vm)
```

Replace it with:

```go
		BindCore(vm)
		BindDbx(vm)
		BindSecurity(vm)
		if !p.config.Sandboxed {
			BindOS(vm)
			BindFilepath(vm)
			BindHTTP(vm)
			BindFilesystem(vm)
		}
		BindForms(vm)
		BindMails(vm)
```

- [ ] **Step 2: Gate the host binds in `sharedBinds` (inside `registerHooks`)**

In the same file, find this block inside the `sharedBinds` closure in `registerHooks` (~lines 300-309):

```go
		BindCore(vm)
		BindDbx(vm)
		BindSecurity(vm)
		BindOS(vm)
		BindFilepath(vm)
		BindHTTP(vm)
		BindFilesystem(vm)
		BindForms(vm)
		BindMails(vm)
		BindApis(vm)
```

Replace it with:

```go
		BindCore(vm)
		BindDbx(vm)
		BindSecurity(vm)
		if !p.config.Sandboxed {
			BindOS(vm)
			BindFilepath(vm)
			BindHTTP(vm)
			BindFilesystem(vm)
		}
		BindForms(vm)
		BindMails(vm)
		BindApis(vm)
```

> Note: `BindApis` stays in both sandboxed and non-sandboxed hook paths — it is routing/middleware/error helpers only. Its one FS-adjacent member `$apis.static` serves the org's own `pb_public` via an HTTP handler (not a JS read primitive). Task 6 adds the confirming test.

- [ ] **Step 3: Build**

Run: `cd ~/code/tinycld/pocketbase && go build ./plugins/jsvm/`
Expected: success.

- [ ] **Step 4: Run the existing jsvm suite (default-off regression)**

Run: `cd ~/code/tinycld/pocketbase && go test ./plugins/jsvm/ -count=1`
Expected: PASS. The existing tests never set `Sandboxed`, so they exercise the full API unchanged.

- [ ] **Step 5: Commit**

```bash
cd ~/code/tinycld/pocketbase
git add plugins/jsvm/jsvm.go
git commit -m "feat(jsvm): omit host-capability binds when Sandboxed"
```

---

## Task 3: Neuter `process.env` / `process.argv` in the sandboxed path (fork)

**Why:** the vendored `process` shim (`plugins/jsvm/internal/nodejs/process/module.go:23-29`) populates `process.env` from `os.Environ()`. With `$os.getenv` denied, `process.env.MT_SUPERUSER_PASSWORD` would still leak every secret. We overwrite `process.env` and `process.argv` on each sandboxed VM after `process.Enable(vm)`.

**Files:**
- Modify: `~/code/tinycld/pocketbase/plugins/jsvm/jsvm.go` (`registerMigrations` after `process.Enable(vm)` ~line 209; `sharedBinds` after `process.Enable(vm)` ~line 298)

- [ ] **Step 1: Write the failing test + shared helpers**

Create `~/code/tinycld/pocketbase/plugins/jsvm/sandbox_test.go` with the complete file below (single import block; helpers + first test together). **Note:** `tests.NewTestApp()` already calls `Bootstrap()` + `RunAllMigrations()` before returning (see `tests/app.go:112,125`), so these tests must **not** call `app.Bootstrap()` again — `routerAdd` routes are wired via the plugin's `OnServe` binding, which fires when `serveRoute` calls `apis.BuildServeMux`.

```go
package jsvm

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/tests"
)

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

// newSandboxApp registers a sandboxed jsvm plugin over a single hook file and
// returns the (already-bootstrapped) test app.
func newSandboxApp(t *testing.T, hookSrc string) *tests.TestApp {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)

	hooksDir := filepath.Join(t.TempDir(), "pb_hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "main.pb.js"), []byte(hookSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	MustRegister(app, Config{HooksDir: hooksDir, Sandboxed: true})
	return app
}

// serveRoute builds the app's serve mux (firing OnServe, which registers hook
// routes) and issues one request against it.
func serveRoute(t *testing.T, app *tests.TestApp, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux, err := apis.BuildServeMux(app, apis.ServeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestSandboxProcessEnvEmpty(t *testing.T) {
	t.Setenv("SANDBOX_SECRET", "leak-me")

	hook := `
		routerAdd('GET', '/leak', (e) => {
			return e.json(200, { secret: process.env.SANDBOX_SECRET ?? null, keys: Object.keys(process.env).length })
		})
	`
	app := newSandboxApp(t, hook)
	rec := serveRoute(t, app, "GET", "/leak")
	if rec.Code != 200 {
		t.Fatalf("route status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !contains(body, `"secret":null`) {
		t.Fatalf("expected sandboxed process.env.SANDBOX_SECRET to be null, got %s", body)
	}
	if !contains(body, `"keys":0`) {
		t.Fatalf("expected sandboxed process.env to be empty, got %s", body)
	}
}
```

> If the fork already has an equivalent route-serving test helper (grep `plugins/jsvm/*_test.go` for `BuildServeMux` / `httptest`), reuse it and drop the local `serveRoute`.

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd ~/code/tinycld/pocketbase && go test ./plugins/jsvm/ -run TestSandboxProcessEnvEmpty -v`
Expected: FAIL — `process.env.SANDBOX_SECRET` returns `"leak-me"` and `keys` > 0 (the shim still populated env).

- [ ] **Step 3: Neuter process env/argv in both sandboxed bind sites**

In `~/code/tinycld/pocketbase/plugins/jsvm/jsvm.go`, in **`registerMigrations`**, immediately after the four `*.Enable(vm)` lines (`registry.Enable(vm)`, `console.Enable(vm)`, `process.Enable(vm)`, `buffer.Enable(vm)` ~lines 207-210), add:

```go
		if p.config.Sandboxed {
			scrubProcess(vm)
		}
```

In **`sharedBinds`** (inside `registerHooks`), after its `requireRegistry.Enable(vm)` / `console.Enable(vm)` / `process.Enable(vm)` / `buffer.Enable(vm)` block (~lines 295-298), add the same:

```go
		if p.config.Sandboxed {
			scrubProcess(vm)
		}
```

Then add the helper near the bottom of `jsvm.go` (package-level func):

```go
// scrubProcess replaces the node-compat process.env / process.argv on a
// sandboxed VM with an empty object / empty array, so untrusted code cannot read
// host environment variables (e.g. MT_SUPERUSER_PASSWORD) or argv through the
// process shim after $os.getenv has been withheld.
func scrubProcess(vm *sobek.Runtime) {
	proc := vm.Get("process")
	obj, ok := proc.(*sobek.Object)
	if !ok || obj == nil {
		return // process shim absent; nothing to scrub
	}
	_ = obj.Set("env", vm.NewObject())
	_ = obj.Set("argv", vm.NewArray())
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd ~/code/tinycld/pocketbase && go test ./plugins/jsvm/ -run TestSandboxProcessEnvEmpty -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd ~/code/tinycld/pocketbase
git add plugins/jsvm/jsvm.go plugins/jsvm/sandbox_test.go
git commit -m "feat(jsvm): scrub process.env/argv in sandboxed VMs"
```

---

## Task 4: Deny-binding tests for hooks (fork)

**Files:**
- Modify: `~/code/tinycld/pocketbase/plugins/jsvm/sandbox_test.go`

- [ ] **Step 1: Write the deny tests**

Append to `~/code/tinycld/pocketbase/plugins/jsvm/sandbox_test.go`:

```go
func TestSandboxHostBindingsAbsent(t *testing.T) {
	// Each global must be undefined under Sandboxed. The hook reports typeof for
	// each dangerous global via a route.
	hook := `
		routerAdd('GET', '/caps', (e) => {
			return e.json(200, {
				os:         typeof $os,
				http:       typeof $http,
				filesystem: typeof $filesystem,
				filepath:   typeof $filepath,
			})
		})
	`
	app := newSandboxApp(t, hook)
	rec := serveRoute(t, app, "GET", "/caps")
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	for _, cap := range []string{"os", "http", "filesystem", "filepath"} {
		want := `"` + cap + `":"undefined"`
		if !contains(rec.Body.String(), want) {
			t.Fatalf("expected $%s undefined under sandbox, got %s", cap, rec.Body.String())
		}
	}
}

func TestSandboxSafeBindingsPresent(t *testing.T) {
	// The safe subset must still work: routing already proven by the routes above;
	// assert $security (crypto) and $app (DB) are present and callable.
	hook := `
		routerAdd('GET', '/safe', (e) => {
			const token = $security.randomString(10)
			return e.json(200, { security: typeof $security, app: typeof $app, tokenLen: token.length })
		})
	`
	app := newSandboxApp(t, hook)
	rec := serveRoute(t, app, "GET", "/safe")
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`"security":"object"`, `"app":"object"`, `"tokenLen":10`} {
		if !contains(body, want) {
			t.Fatalf("expected %s in safe-bindings body, got %s", want, body)
		}
	}
}

func TestNonSandboxedStillHasHostBindings(t *testing.T) {
	// Regression: with Sandboxed unset, $os must still be present (full API).
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	hooksDir := filepath.Join(t.TempDir(), "pb_hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hook := `routerAdd('GET','/caps',(e)=>e.json(200,{os:typeof $os}))`
	if err := os.WriteFile(filepath.Join(hooksDir, "main.pb.js"), []byte(hook), 0o644); err != nil {
		t.Fatal(err)
	}
	MustRegister(app, Config{HooksDir: hooksDir}) // Sandboxed defaults false
	rec := serveRoute(t, app, "GET", "/caps")
	if !contains(rec.Body.String(), `"os":"object"`) {
		t.Fatalf("expected $os present when not sandboxed, got %s", rec.Body.String())
	}
}
```

- [ ] **Step 2: Run the tests**

Run: `cd ~/code/tinycld/pocketbase && go test ./plugins/jsvm/ -run 'TestSandbox|TestNonSandboxed' -v`
Expected: PASS (Task 2 + Task 3 already made the binds absent and env scrubbed).

- [ ] **Step 3: Full jsvm suite + race**

Run: `cd ~/code/tinycld/pocketbase && go test ./plugins/jsvm/ -count=1 && go test -race ./plugins/jsvm/ -count=1`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
cd ~/code/tinycld/pocketbase
git add plugins/jsvm/sandbox_test.go
git commit -m "test(jsvm): assert host bindings absent and safe subset present under sandbox"
```

---

## Task 5: Deny test for sandboxed migrations (fork)

**Why:** migrations run author-controlled JS through a *separate* VM (`registerMigrations`). Prove the flag gates that path too — a migration referencing `$os` must fail (ReferenceError), not exec.

**Files:**
- Modify: `~/code/tinycld/pocketbase/plugins/jsvm/sandbox_test.go`

- [ ] **Step 1: Write the failing-closed migration test**

Append to `sandbox_test.go`:

```go
func TestSandboxMigrationHasNoHostBindings(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)

	migDir := filepath.Join(t.TempDir(), "pb_migrations")
	if err := os.MkdirAll(migDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A migration that touches $os must throw at load: $os is undefined under sandbox.
	mig := `migrate((app) => { $os.exec('id') }, (app) => {})`
	if err := os.WriteFile(filepath.Join(migDir, "1700000000_evil.js"), []byte(mig), 0o644); err != nil {
		t.Fatal(err)
	}

	// registerMigrations runs at Register() time. Under sandbox, running the
	// migration file must error because $os is not defined.
	err = Register(app, Config{MigrationsDir: migDir, Sandboxed: true})
	if err == nil {
		t.Fatal("expected sandboxed migration referencing $os to fail registration, got nil")
	}
	if !contains(err.Error(), "os") && !contains(err.Error(), "not defined") && !contains(err.Error(), "ReferenceError") {
		t.Fatalf("expected a $os-not-defined error, got %v", err)
	}
}
```

> `registerMigrations` runs each migration's top-level code via `vm.RunScript` at `Register` time, so a bare `$os.exec(...)` at top level throws immediately. (The evil code is at top level, not inside the `migrate(up,down)` callbacks, so it executes during load — which is exactly the attack we're blocking.)

- [ ] **Step 2: Run it**

Run: `cd ~/code/tinycld/pocketbase && go test ./plugins/jsvm/ -run TestSandboxMigrationHasNoHostBindings -v`
Expected: PASS (the migration errors because `$os` is undefined).

- [ ] **Step 3: Sanity — non-sandboxed migration referencing `$os` does NOT error at load**

Append:

```go
func TestNonSandboxedMigrationHasHostBindings(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	migDir := filepath.Join(t.TempDir(), "pb_migrations")
	if err := os.MkdirAll(migDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// References $os only INSIDE the up callback (not executed at load), so a
	// non-sandboxed load succeeds — proving $os exists in the non-sandbox path.
	mig := `migrate((app) => { const _ = typeof $os }, (app) => {})`
	if err := os.WriteFile(filepath.Join(migDir, "1700000000_ok.js"), []byte(mig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Register(app, Config{MigrationsDir: migDir}); err != nil {
		t.Fatalf("non-sandboxed migration load failed: %v", err)
	}
}
```

Run: `cd ~/code/tinycld/pocketbase && go test ./plugins/jsvm/ -run TestNonSandboxedMigrationHasHostBindings -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
cd ~/code/tinycld/pocketbase
git add plugins/jsvm/sandbox_test.go
git commit -m "test(jsvm): sandboxed migrations have no host bindings"
```

---

## Task 6: Confirm `$apis.static` cannot escape the org dir (fork)

**Why:** spec §3.2.1 — `$apis.static` stays in the sandboxed hook binds. Confirm it only serves a rooted dir and cannot be used to read arbitrary host paths into an HTTP response outside that root.

**Files:**
- Modify: `~/code/tinycld/pocketbase/plugins/jsvm/sandbox_test.go`

- [ ] **Step 1: Write the traversal test**

Append:

```go
func TestSandboxApisStaticNoTraversal(t *testing.T) {
	root := t.TempDir()
	// A secret file OUTSIDE the served root.
	secret := filepath.Join(filepath.Dir(root), "outside_secret.txt")
	if err := os.WriteFile(secret, []byte("TOPSECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(secret) })

	served := filepath.Join(root, "public")
	if err := os.MkdirAll(served, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(served, "ok.txt"), []byte("PUBLIC"), 0o644); err != nil {
		t.Fatal(err)
	}

	hook := `routerAdd('GET','/assets/{path...}', $apis.static(` + "`" + served + "`" + `, false))`
	app := newSandboxApp(t, hook)

	// Legit file serves.
	rec := serveRoute(t, app, "GET", "/assets/ok.txt")
	if rec.Code != 200 || !contains(rec.Body.String(), "PUBLIC") {
		t.Fatalf("expected to serve ok.txt, got %d %s", rec.Code, rec.Body.String())
	}

	// Traversal to the outside secret must NOT succeed. Build the request with a
	// raw (un-normalized) target so the traversal actually reaches the mux — a
	// plain path string would be cleaned by net/http before dispatch.
	req := httptest.NewRequest("GET", "/assets/ok.txt", nil)
	req.URL.Path = "/assets/../outside_secret.txt"
	req.URL.RawPath = "/assets/%2e%2e/outside_secret.txt"
	mux, err := apis.BuildServeMux(app, apis.ServeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req)
	if rec2.Code == 200 && contains(rec2.Body.String(), "TOPSECRET") {
		t.Fatalf("SECURITY: $apis.static leaked a file outside its root: %s", rec2.Body.String())
	}
}
```

- [ ] **Step 2: Run it**

Run: `cd ~/code/tinycld/pocketbase && go test ./plugins/jsvm/ -run TestSandboxApisStaticNoTraversal -v`
Expected: PASS (`os.DirFS`, which `$apis.static` uses, is rooted and refuses `..` escapes; PocketBase's static handler also cleans paths).

> **If this test FAILS** (traversal leaks): do not ship `$apis.static` in the sandboxed binds. Split it out of `BindApis` the way `BindOS` is split, gate it behind `!Sandboxed`, and update the spec §3.2.1 decision. Add a follow-up note and stop for review.

- [ ] **Step 3: Commit**

```bash
cd ~/code/tinycld/pocketbase
git add plugins/jsvm/sandbox_test.go
git commit -m "test(jsvm): \$apis.static cannot traverse outside its root under sandbox"
```

---

## Task 7: Flip the router runtime load path to sandboxed (router)

**Files:**
- Modify: `~/code/tinycld/multi-org/internal/orgmanager/manager.go:120-126`
- Modify: `~/code/tinycld/multi-org/internal/orgmanager/e2e_test.go` (add a test)

- [ ] **Step 1: Write the failing e2e sandbox test**

Append to `~/code/tinycld/multi-org/internal/orgmanager/e2e_test.go`:

```go
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
```

Add `"strings"` to the test file's imports.

> If `contains` already exists in the package's test files, drop the local definition here to avoid a redeclaration.

- [ ] **Step 2: Run it to verify it fails**

Run: `cd ~/code/tinycld/multi-org && go test ./internal/orgmanager/ -run TestE2E_TenantHooksAreSandboxed -v`
Expected: FAIL — `$os` is currently `"object"` (manager loads without `Sandboxed`).

- [ ] **Step 3: Set `Sandboxed: true` in `manager.load`**

In `~/code/tinycld/multi-org/internal/orgmanager/manager.go`, in the `jsvm.MustRegister` call inside `load` (~lines 120-126), add the field:

```go
	jsvm.MustRegister(pb, jsvm.Config{
		HooksDir:      filepath.Join(orgDir, "pb_hooks"),
		MigrationsDir: filepath.Join(orgDir, "pb_migrations"),
		HooksWatch:    false,
		HooksPoolSize: m.cfg.HooksPool,
		ProgramSource: m.cfg.Programs,
		Sandboxed:     true, // untrusted tenant code
	})
```

- [ ] **Step 4: Run it to verify it passes**

Run: `cd ~/code/tinycld/multi-org && go test ./internal/orgmanager/ -run TestE2E_TenantHooksAreSandboxed -v`
Expected: PASS.

- [ ] **Step 5: Run the whole orgmanager suite (existing e2e must still pass)**

Run: `cd ~/code/tinycld/multi-org && go test ./internal/orgmanager/ -count=1`
Expected: PASS — the `/whoami` routing tests still work because `routerAdd` is in the safe subset.

- [ ] **Step 6: Commit**

```bash
cd ~/code/tinycld/multi-org
git add internal/orgmanager/manager.go internal/orgmanager/e2e_test.go
git commit -m "feat: run tenant hooks under the jsvm sandbox"
```

---

## Task 8: Flip the provision path to sandboxed (router)

**Why:** `bootstrapTenantOnce` runs tenant migration JS **inside the control-plane process** — the worst place for an escape. It must sandbox too.

**Files:**
- Modify: `~/code/tinycld/multi-org/internal/controlplane/provisioning.go:289-293`
- Modify: `~/code/tinycld/multi-org/internal/controlplane/integration_test.go` (add a test)

- [ ] **Step 1: Write the failing provision-deny test**

Append to `~/code/tinycld/multi-org/internal/controlplane/integration_test.go`. This mirrors the exact setup of `TestIntegration_CreateOrgToLoadWithSchema` (control-plane app via `New`, `store.New`, `NewProvisioner(cp.App, root, s, func(string){})`, `p.CreateOrg(slug, name, lock map[string]string)`):

```go
// TestIntegration_MaliciousMigrationCannotExec proves provision-time migrations
// run sandboxed: a migration whose top-level code touches $os fails provisioning
// (the binding is absent) rather than executing in the control-plane process.
func TestIntegration_MaliciousMigrationCannotExec(t *testing.T) {
	root := t.TempDir()

	cp, err := New(filepath.Join(root, "pb_control", "pb_data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.App.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer cp.App.ResetBootstrapState()
	if err := cp.App.RunAllMigrations(); err != nil {
		t.Fatal(err)
	}

	s := store.New(root)
	// $os at top level runs at migration load — under sandbox it must throw.
	evil := []byte("$os.exec('id'); migrate((app)=>{}, (app)=>{})")
	if err := s.Publish("@tinycld/evil", "1.0.0", map[string][]byte{
		"pb-migrations/1700000000_evil.js": evil,
	}); err != nil {
		t.Fatal(err)
	}

	p := NewProvisioner(cp.App, root, s, func(string) {})
	if _, err := p.CreateOrg("evilorg", "Evil", map[string]string{"@tinycld/evil": "1.0.0"}); err == nil {
		t.Fatal("expected provisioning to fail for a migration touching $os under sandbox")
	}
}
```

> `CreateOrg` calls `bootstrapTenantOnce`, which registers jsvm and runs the migration. The assertion is simply that `CreateOrg` returns a non-nil error (the `$os` reference throws at load). Confirm `filepath` and `store` are already imported in this test file (they are, per `TestIntegration_CreateOrgToLoadWithSchema`).

- [ ] **Step 2: Run it to verify it fails**

Run: `cd ~/code/tinycld/multi-org && go test ./internal/controlplane/ -run TestIntegration_MaliciousMigrationCannotExec -v`
Expected: FAIL — provisioning currently succeeds (or `$os.exec` actually runs) because `bootstrapTenantOnce` is not sandboxed.

- [ ] **Step 3: Set `Sandboxed: true` in `bootstrapTenantOnce`**

In `~/code/tinycld/multi-org/internal/controlplane/provisioning.go`, in the `jsvm.MustRegister` call (~lines 289-293):

```go
	jsvm.MustRegister(pb, jsvm.Config{
		HooksDir:      filepath.Join(orgDir, "pb_hooks"),
		MigrationsDir: filepath.Join(orgDir, "pb_migrations"),
		HooksWatch:    false,
		Sandboxed:     true, // untrusted tenant migration JS, run in the control-plane process
	})
```

- [ ] **Step 4: Run it to verify it passes**

Run: `cd ~/code/tinycld/multi-org && go test ./internal/controlplane/ -run TestIntegration_MaliciousMigrationCannotExec -v`
Expected: PASS (provisioning fails closed).

- [ ] **Step 5: Run the whole controlplane suite (benign migration still provisions)**

Run: `cd ~/code/tinycld/multi-org && go test ./internal/controlplane/ -count=1`
Expected: PASS — `TestIntegration_CreateOrgToLoadWithSchema` / `...WithTSSchema` still provision their benign collections (their migrations use `migrate(up,down)` with only DB APIs, all in the safe subset).

- [ ] **Step 6: Commit**

```bash
cd ~/code/tinycld/multi-org
git add internal/controlplane/provisioning.go internal/controlplane/integration_test.go
git commit -m "feat: sandbox provision-time tenant migrations"
```

---

## Task 8.5: Fail closed (not panic) on a load-time hook error (fork + router)

**Why (found during Task 8 review):** When `HooksWatch: false` (the config BOTH router call sites use), `compileHookFiles` (`plugins/jsvm/jsvm.go`) **re-`panic`s** on any hook file that throws at *load* time — e.g. a hostile tenant hook with top-level `$os.exec(...)` or any top-level `throw`. That panic propagates uncaught through `Register`/`MustRegister`, crashing the **entire shared multi-tenant process** when an org is lazily loaded (`orgmanager.Get`) or provisioned. The capability sandbox denies `$os`, but the *throw* still panics → a process-wide DoS any org operator can trigger by publishing a hook bundle that throws on load. Task 8 fixed this for the migration path (via `Register`+return); this task closes the same class of bug on the **hook-load** path at both sites.

**Design:** Make `compileHookFiles` **return** the load error instead of re-panicking **when `p.config.Sandboxed`** (stock non-sandboxed behavior stays byte-for-byte: it still panics, matching upstream's trusted-author assumption). Then switch `orgmanager.load`'s `MustRegister` → `Register`+return so a hostile hook fails only that one org's load (surfacing as a 5xx for that tenant) rather than panicking the process. The provision path already uses `Register`+return after Task 8, so it inherits the fix automatically once the fork stops panicking under `Sandboxed`.

**Files:**
- Modify: `~/code/tinycld/pocketbase/plugins/jsvm/jsvm.go` (`compileHookFiles`)
- Modify: `~/code/tinycld/pocketbase/plugins/jsvm/sandbox_test.go` (fork deny test)
- Modify: `~/code/tinycld/multi-org/internal/orgmanager/manager.go` (`MustRegister` → `Register`+return)
- Modify: `~/code/tinycld/multi-org/internal/orgmanager/e2e_test.go` (router deny test)
- Modify: `~/code/tinycld/multi-org/internal/controlplane/integration_test.go` (provision hook-throw deny test)

### Part A — fork: return instead of panic under Sandboxed

- [ ] **Step 1: Write the failing fork test (a sandboxed hook that throws at load must make `Register` return an error, not panic)**

Append to `~/code/tinycld/pocketbase/plugins/jsvm/sandbox_test.go`:

```go
func TestSandboxHookThrowAtLoadReturnsError(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)

	hooksDir := filepath.Join(t.TempDir(), "pb_hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Top-level code runs at hook LOAD. Under sandbox $os is undefined, so this
	// throws at load. It must be RETURNED as an error, not panic the process.
	if err := os.WriteFile(filepath.Join(hooksDir, "main.pb.js"), []byte(`$os.exec('id')`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Register must not panic; it must return a non-nil error.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Register panicked on a load-time hook error under sandbox: %v", r)
		}
	}()
	if err := Register(app, Config{HooksDir: hooksDir, Sandboxed: true}); err == nil {
		t.Fatal("expected Register to return an error for a load-throwing sandboxed hook, got nil")
	}
}
```

- [ ] **Step 2: Run it — expect FAIL (panic)**

Run: `cd ~/code/tinycld/pocketbase && go test ./plugins/jsvm/ -run TestSandboxHookThrowAtLoadReturnsError -v`
Expected: FAIL — the test's `recover` catches a panic and calls `t.Fatalf` ("Register panicked ..."), because `compileHookFiles` currently panics when `HooksWatch` is false.

- [ ] **Step 3: Make `compileHookFiles` return under Sandboxed**

In `~/code/tinycld/pocketbase/plugins/jsvm/jsvm.go`, `compileHookFiles` currently does (in the deferred recover): `if p.config.HooksWatch { color.Red(...) } else { panic(fmtErr) }`, and the per-file work is wrapped in an inner `func(){ ... }()` closure so a panic doesn't abort the loop. Change it so that under `Sandboxed`, a load error is captured and returned instead of panicked. Replace the body of `compileHookFiles` with:

```go
func (p *plugin) compileHookFiles(loader *sobek.Runtime, files map[string][]byte) error {
	var loadErr error
	for file, content := range files {
		func() {
			defer func() {
				if r := recover(); r != nil {
					fmtErr := fmt.Errorf("failed to execute %s:\n - %v", file, r)
					switch {
					case p.config.Sandboxed:
						// Untrusted code: a load-time throw must fail this app's
						// registration (returned to the caller), never panic the
						// shared multi-tenant process.
						if loadErr == nil {
							loadErr = fmtErr
						}
					case p.config.HooksWatch:
						color.Red("%v", fmtErr)
					default:
						panic(fmtErr)
					}
				}
			}()

			prog, cerr := p.compile(string(content), false)
			if cerr != nil {
				panic(cerr)
			}
			if _, rerr := loader.RunProgram(prog); rerr != nil {
				panic(rerr)
			}
		}()
		if loadErr != nil {
			return loadErr
		}
	}
	return loadErr
}
```

> This preserves stock behavior exactly: non-sandboxed + `HooksWatch` → log; non-sandboxed + no watch → panic (unchanged). Only the sandboxed path is new: capture the first load error and return it (the outer `registerHooks` already does `if err := p.compileHookFiles(...); err != nil { return err }`).

- [ ] **Step 4: Run the fork test — expect PASS**

Run: `cd ~/code/tinycld/pocketbase && go test ./plugins/jsvm/ -run TestSandboxHookThrowAtLoadReturnsError -v`
Expected: PASS (no panic; `Register` returns an error).

- [ ] **Step 5: Full fork jsvm suite + race (no regression to the panic/log behavior)**

Run: `cd ~/code/tinycld/pocketbase && go test ./plugins/jsvm/ -count=1 && go test -race ./plugins/jsvm/ -count=1`
Expected: PASS.

- [ ] **Step 6: Commit (fork)**

```bash
cd ~/code/tinycld/pocketbase
git add plugins/jsvm/jsvm.go plugins/jsvm/sandbox_test.go
git commit -m "feat(jsvm): return (not panic) on load-time hook error when Sandboxed"
```

### Part B — router: orgmanager fails that one org, not the process

- [ ] **Step 7: Write the failing router test (a hostile hook that throws at load makes `Get` return an error, not panic)**

Append to `~/code/tinycld/multi-org/internal/orgmanager/e2e_test.go`:

```go
// TestE2E_HostileHookThrowAtLoadFailsOrgNotProcess proves a tenant hook that
// throws at load fails only that org's Get() (returned error), rather than
// panicking the shared process.
func TestE2E_HostileHookThrowAtLoadFailsOrgNotProcess(t *testing.T) {
	root := t.TempDir()
	s := store.New(root)
	// Top-level $os throws at load; under sandbox $os is undefined.
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
```

- [ ] **Step 8: Run it — expect FAIL (panic)**

Run: `cd ~/code/tinycld/multi-org && go test ./internal/orgmanager/ -run TestE2E_HostileHookThrowAtLoadFailsOrgNotProcess -v`
Expected: FAIL — `Get` panics (via `MustRegister`), the test's `recover` fires `t.Fatalf`. (Even with the Part A fork fix, `manager.load` still calls `MustRegister`, which panics on the returned error — that's what Step 9 fixes.)

- [ ] **Step 9: Switch `manager.load` to `Register` + return**

In `~/code/tinycld/multi-org/internal/orgmanager/manager.go`, in `load`, replace the `jsvm.MustRegister(pb, jsvm.Config{...})` call with the error-returning form:

```go
	if err := jsvm.Register(pb, jsvm.Config{
		HooksDir:      filepath.Join(orgDir, "pb_hooks"),
		MigrationsDir: filepath.Join(orgDir, "pb_migrations"),
		HooksWatch:    false,
		HooksPoolSize: m.cfg.HooksPool,
		ProgramSource: m.cfg.Programs,
		Sandboxed:     true, // untrusted tenant code
	}); err != nil {
		_ = pb.App.ResetBootstrapState()
		return nil, fmt.Errorf("jsvm register %s: %w", slug, err)
	}
```

> Match the existing error-handling pattern already used a few lines below for `pb.Bootstrap()` / `RunAllMigrations()` (they `ResetBootstrapState()` then return a wrapped error). Confirm `fmt` is imported (it is — used elsewhere in the file). Note `jsvm.Register` runs during plugin registration, BEFORE `pb.Bootstrap()`; if the reset-on-error at this point is unnecessary because bootstrap hasn't run, keep it anyway for symmetry — it is a safe no-op. If `ResetBootstrapState` before Bootstrap errors in practice, drop that line and just return the wrapped error.

- [ ] **Step 10: Run the router test — expect PASS**

Run: `cd ~/code/tinycld/multi-org && go test ./internal/orgmanager/ -run TestE2E_HostileHookThrowAtLoadFailsOrgNotProcess -v`
Expected: PASS (Get returns an error, no panic).

- [ ] **Step 11: Full orgmanager suite (existing e2e + Task 7 test still pass)**

Run: `cd ~/code/tinycld/multi-org && go test ./internal/orgmanager/ -count=1 && go test -race ./internal/orgmanager/ -count=1`
Expected: PASS.

### Part C — router: provision path rejects a load-throwing hook too

- [ ] **Step 12: Add a provision-path hook-throw deny test**

Append to `~/code/tinycld/multi-org/internal/controlplane/integration_test.go` (mirror `TestIntegration_MaliciousMigrationCannotExec`'s setup, but ship the throw in a HOOK file, not a migration):

```go
// TestIntegration_MaliciousHookCannotCrashControlPlane proves a package whose
// hook throws at load fails provisioning with a returned error, rather than
// panicking the control-plane process.
func TestIntegration_MaliciousHookCannotCrashControlPlane(t *testing.T) {
	root := t.TempDir()

	cp, err := New(filepath.Join(root, "pb_control", "pb_data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.App.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer cp.App.ResetBootstrapState()
	if err := cp.App.RunAllMigrations(); err != nil {
		t.Fatal(err)
	}

	s := store.New(root)
	if err := s.Publish("@tinycld/evilhook", "1.0.0", map[string][]byte{
		"server/main.pb.js": []byte(`$os.exec('id')`),
	}); err != nil {
		t.Fatal(err)
	}

	p := NewProvisioner(cp.App, root, s, func(string) {})
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("provisioning panicked on a load-throwing hook: %v", r)
		}
	}()
	if _, err := p.CreateOrg("evilhook", "EvilHook", map[string]string{"@tinycld/evilhook": "1.0.0"}); err == nil {
		t.Fatal("expected provisioning to fail for a load-throwing hook, got nil")
	}
}
```

- [ ] **Step 13: Run it — expect PASS (Part A already made the fork return the error; Task 8 already made bootstrapTenantOnce use Register+return)**

Run: `cd ~/code/tinycld/multi-org && go test ./internal/controlplane/ -run TestIntegration_MaliciousHookCannotCrashControlPlane -v`
Expected: PASS. If it FAILS with a panic, `bootstrapTenantOnce` must be double-checked to use `jsvm.Register` (Task 8) AND the fork Part A fix must be in place — verify both, do not weaken the test.

- [ ] **Step 14: Full controlplane suite**

Run: `cd ~/code/tinycld/multi-org && go test ./internal/controlplane/ -count=1`
Expected: PASS.

- [ ] **Step 15: Commit (router)**

```bash
cd ~/code/tinycld/multi-org
git add internal/orgmanager/manager.go internal/orgmanager/e2e_test.go internal/controlplane/integration_test.go
git commit -m "feat: fail closed (not panic) on a hostile load-throwing tenant hook"
```

---

## Task 9: Full-suite verification (both repos)

- [ ] **Step 1: Fork — build, vet, test, race**

Run:
```bash
cd ~/code/tinycld/pocketbase && go build ./... && go vet ./plugins/jsvm/... && go test ./plugins/jsvm/... -count=1 && go test -race ./plugins/jsvm/ -count=1
```
Expected: all PASS.

- [ ] **Step 2: Router — build, vet, test, race**

Run:
```bash
cd ~/code/tinycld/multi-org && go build ./... && go vet ./... && go test ./... -count=1 && go test -race ./...
```
Expected: all PASS.

- [ ] **Step 3: Re-run the named proof tests from HANDOFF §8**

Run:
```bash
cd ~/code/tinycld/multi-org
go test ./internal/orgmanager/ -run TestE2E -v
go test ./internal/controlplane/ -run 'TestIntegration_CreateOrgToLoadWith(TS)?Schema' -v
```
Expected: PASS — cross-org sharing + full provisioning chain unbroken by the sandbox.

- [ ] **Step 4: No commit** (verification only). If anything failed, diagnose and fix at the source before proceeding — do not weaken a test to reach green.

---

## Task 10: Document the honest ceiling (both repos)

**Files:**
- Modify: `~/code/tinycld/multi-org/README.md` (or `HANDOFF.md` §5)

- [ ] **Step 1: Add a security-boundary note**

Add a short section to `~/code/tinycld/multi-org/README.md` (or update `HANDOFF.md` §5 "Remaining gaps"):

```markdown
## Tenant JS security boundary

Tenant hooks and migrations run under the fork's jsvm **Sandboxed** mode: a
deny-by-default allowlist that withholds `$os` (exec/env/raw filesystem),
`$http` (outbound HTTP), `$filesystem`, and `$filepath`, and neuters
`process.env`/`process.argv`. Legitimate file access goes through org-scoped
`$app` record-file APIs.

**This is blast-radius reduction, not attacker containment** — the WordPress
`disable_functions`/`open_basedir` tier. sobek is not a hard sandbox: engine
escapes, CPU/memory DoS (no resource limits yet), and shared-process risks
remain because all orgs share one OS process. Hostile-grade isolation requires
OS-level per-process/per-uid isolation (deferred; the `GetOrg → http.Handler`
seam in `frontrouter` is where a reverse-proxy-to-subprocess would drop in).
```

- [ ] **Step 2: Commit**

```bash
cd ~/code/tinycld/multi-org
git add README.md   # or HANDOFF.md
git commit -m "docs: document the tenant JS sandbox boundary and its honest ceiling"
```

---

## Self-review checklist (for the implementer, before declaring done)

- [ ] Both `jsvm.MustRegister` tenant call sites pass `Sandboxed: true` (`orgmanager/manager.go`, `controlplane/provisioning.go`). The control-plane's *own* app (if it registers jsvm for operator hooks) is NOT sandboxed.
- [ ] `$os`, `$http`, `$filesystem`, `$filepath` all `undefined` in a sandboxed hook AND a sandboxed migration.
- [ ] `process.env` is empty and `process.argv` empty under sandbox.
- [ ] Safe subset (`routerAdd`, `$app` DB, `$security`, `$template`) still works.
- [ ] `$apis.static` traversal test passes (or `static` was split out — see Task 6 escape hatch).
- [ ] Default-off (`Sandboxed` unset) is byte-for-byte the old behavior — existing jsvm suite green, unmodified.
- [ ] `HANDOFF.md` §5 finding about the sandbox is resolved / cross-referenced; the honest-ceiling note is present.
- [ ] Fork commits are on `feat/multitenant-fork`; router commits on `feat/operator-runnable`. Neither pushed unless you intend to.
