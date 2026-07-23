# TypeScript Hooks & Migrations Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let package authors write `.pb.ts` / `.ts` hooks and migrations in real TypeScript, transpiled to JS and run inside the multi-org router's tenant apps.

**Architecture:** Add an esbuild-Go transpile seam at the fork's single file-read chokepoint (`filesContent`), covering both hooks and migrations. Swap the JS engine goja→sobek (for ESM/ES2020+) using the `ohayocorp/sobek_nodejs` Node-compat port. Pre-transpile TS→JS at publish time in the router so production tenants only ever see `.js`.

**Tech Stack:** Go, esbuild (`github.com/evanw/esbuild/pkg/api`), sobek (`github.com/grafana/sobek`), `github.com/ohayocorp/sobek_nodejs`, PocketBase jsvm plugin.

**Spec:** `docs/superpowers/specs/2026-07-23-typescript-hooks-design.md`

**Two repos, verify each independently:**
- **Fork** — `~/code/tinycld/pocketbase` (branch `feat/multitenant-fork`)
- **Router** — `~/code/tinycld/multi-org` (branch `feat/operator-runnable`)

The router's `go.mod` has `replace github.com/pocketbase/pocketbase => ../pocketbase`, so fork changes are picked up live. **After any fork change to the `ProgramSource` interface, the router must be rebuilt** (Phase C).

---

## File Structure

**Fork (`~/code/tinycld/pocketbase`):**
- Create `plugins/jsvm/transform.go` — `transformSource(name, content)` + `isTypeScript(name)`. Stateless TS→JS via esbuild. One responsibility: source transformation.
- Create `plugins/jsvm/transform_test.go` — unit tests for the above.
- Modify `plugins/jsvm/jsvm.go:586` — call `transformSource` inside `filesContent` before storing content.
- Modify `plugins/jsvm/jsvm.go` (imports + engine sites, Phase B) — goja→sobek.
- Modify `plugins/jsvm/program_source.go` — `*goja.Program` → `*sobek.Program`.
- Modify `plugins/jsvm/binds.go`, and any other goja-referencing files (Phase B).

**Router (`~/code/tinycld/multi-org`):**
- Modify `internal/progcache/progcache.go` — `*goja.Program` → `*sobek.Program`.
- Create `internal/controlplane/transpile.go` — `transpileForStore(files)`.
- Create `internal/controlplane/transpile_test.go`.
- Modify `internal/controlplane/provisioning.go` — call `transpileForStore` in `PublishPackage`; add `POST /api/store/packages` route in `RegisterRoutes`.
- Modify `internal/controlplane/integration_test.go` — add a TS variant.

---

# Phase A — TS transpile seam on goja (fork)

Delivers working TypeScript hooks & migrations *before* the engine swap, so this phase is independently testable on the current goja engine.

## Task A1: esbuild dependency + `transformSource`

**Files:**
- Create: `~/code/tinycld/pocketbase/plugins/jsvm/transform.go`
- Test: `~/code/tinycld/pocketbase/plugins/jsvm/transform_test.go`

- [ ] **Step 1: Add the esbuild dependency**

Run (from `~/code/tinycld/pocketbase`):
```bash
go get github.com/evanw/esbuild/pkg/api@latest
```
Expected: `go.mod`/`go.sum` updated with `github.com/evanw/esbuild`.

- [ ] **Step 2: Write the failing test**

Create `plugins/jsvm/transform_test.go`:
```go
package jsvm

import (
	"strings"
	"testing"
)

func TestIsTypeScript(t *testing.T) {
	cases := map[string]bool{
		"main.pb.ts":       true,
		"001_init.ts":      true,
		"main.pb.js":       false,
		"001_init.js":      false,
		"notes.txt":        false,
	}
	for name, want := range cases {
		if got := isTypeScript(name); got != want {
			t.Errorf("isTypeScript(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestTransformSource_TranspilesTS(t *testing.T) {
	// Type annotations + enum are TS-only syntax goja cannot parse.
	src := []byte("enum E { A, B }\nconst x: number = E.A\nrouterAdd('GET','/x',()=>{})")
	out, err := transformSource("main.pb.ts", src)
	if err != nil {
		t.Fatalf("transformSource: %v", err)
	}
	js := string(out)
	if strings.Contains(js, ": number") {
		t.Fatalf("type annotation not stripped: %s", js)
	}
	if !strings.Contains(js, "routerAdd") {
		t.Fatalf("expected routerAdd call preserved: %s", js)
	}
}

func TestTransformSource_PassesThroughJS(t *testing.T) {
	src := []byte("const x = 1 // plain js")
	out, err := transformSource("main.pb.js", src)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(src) {
		t.Fatalf(".js must pass through byte-identical; got %q", out)
	}
}

func TestTransformSource_SyntaxErrorIsClear(t *testing.T) {
	out, err := transformSource("bad.pb.ts", []byte("const x: = "))
	if err == nil {
		t.Fatalf("expected transpile error, got out=%q", out)
	}
	if !strings.Contains(err.Error(), "bad.pb.ts") {
		t.Fatalf("error should name the file: %v", err)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./plugins/jsvm/ -run 'TestIsTypeScript|TestTransformSource' -count=1`
Expected: FAIL — `undefined: isTypeScript` / `undefined: transformSource`.

- [ ] **Step 4: Write minimal implementation**

Create `plugins/jsvm/transform.go`:
```go
package jsvm

import (
	"fmt"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// isTypeScript reports whether a hook/migration filename is TypeScript source
// that must be transpiled before it reaches the JS engine. Matches the ".ts" and
// ".pb.ts" files already accepted by the Hooks/Migrations file patterns.
func isTypeScript(name string) bool {
	return strings.HasSuffix(name, ".ts")
}

// transformSource returns runnable JS for a hook/migration file. TypeScript files
// are transpiled via esbuild (in-process, no CGO); .js files pass through
// byte-for-byte. Called from filesContent so BOTH the hooks and the migrations
// load paths get it (migrations bypass p.compile — see jsvm.go).
func transformSource(name string, content []byte) ([]byte, error) {
	if !isTypeScript(name) {
		return content, nil
	}
	res := api.Transform(string(content), api.TransformOptions{
		Loader:    api.LoaderTS,
		Target:    api.ES2020,
		Sourcemap: api.SourceMapInline,
	})
	if len(res.Errors) > 0 {
		msgs := make([]string, 0, len(res.Errors))
		for _, e := range res.Errors {
			msgs = append(msgs, e.Text)
		}
		return nil, fmt.Errorf("transpile %s: %s", name, strings.Join(msgs, "; "))
	}
	return res.Code, nil
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./plugins/jsvm/ -run 'TestIsTypeScript|TestTransformSource' -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
cd ~/code/tinycld/pocketbase
git add plugins/jsvm/transform.go plugins/jsvm/transform_test.go go.mod go.sum
git commit -m "feat(jsvm): esbuild-backed transformSource for .ts hooks/migrations"
```

## Task A2: Wire the seam into `filesContent`

**Files:**
- Modify: `~/code/tinycld/pocketbase/plugins/jsvm/jsvm.go:586`

- [ ] **Step 1: Apply the transform where content is read**

In `filesContent`, replace the single storing line (currently `result[f.Name()] = raw`, jsvm.go:586):
```go
		transformed, err := transformSource(f.Name(), raw)
		if err != nil {
			return nil, err
		}
		result[f.Name()] = transformed
```

- [ ] **Step 2: Verify the whole plugin still builds**

Run: `go build ./plugins/jsvm/`
Expected: builds clean.

- [ ] **Step 3: Run the existing jsvm tests (no regression)**

Run: `go test ./plugins/jsvm/ -count=1`
Expected: PASS (existing `.js`-based tests unaffected — `.js` passes through).

- [ ] **Step 4: Commit**

```bash
git add plugins/jsvm/jsvm.go
git commit -m "feat(jsvm): transpile .ts files in filesContent (hooks + migrations)"
```

## Task A3: End-to-end `.pb.ts` hook + `.ts` migration tests

**Files:**
- Test: `~/code/tinycld/pocketbase/plugins/jsvm/transform_test.go` (append)

These prove the seam covers both load paths. The migration path is the important one — it bypasses `p.compile`, so only the `filesContent` placement makes it work.

- [ ] **Step 1: Inspect an existing jsvm end-to-end test for the harness pattern**

Run: `grep -n "func Test" plugins/jsvm/binds_app_reset_test.go`
The real end-to-end harness lives in `plugins/jsvm/binds_app_reset_test.go` (`TestHooksAppReset`, `TestRouterHandlerAppReset`) — it writes hook files to a temp `HooksDir`, registers the plugin, bootstraps a test app, and drives routes. Mirror that harness — do NOT invent a new one. (Note: there is no `binds_app_reset_test.go` in this fork.)

- [ ] **Step 2: Write the failing tests**

Append to `transform_test.go`, adapting the harness from Step 1 (the skeleton below shows intent; use the real app/registration helpers found in `binds_app_reset_test.go`):
```go
// A .pb.ts hook with TS syntax registers and serves a route.
func TestTSHook_EndToEnd(t *testing.T) {
	// Arrange: write a .pb.ts file with TS-only syntax into a temp HooksDir:
	//   interface Payload { ok: boolean }
	//   routerAdd('GET', '/tstest', (e) => e.json(200, { ok: true } as Payload))
	// Register jsvm on a test app (see binds_app_reset_test.go harness), bootstrap, build mux.
	// Act: GET /tstest.
	// Assert: 200 and body {"ok":true}.
	t.Skip("replace with real harness from binds_app_reset_test.go")
}

// A .ts migration with TS syntax runs and creates a collection.
func TestTSMigration_EndToEnd(t *testing.T) {
	// Arrange: write a .ts migration into a temp MigrationsDir:
	//   migrate((app) => {
	//     const c = new Collection({ id: 'pbc_ts_01', name: 'ts_widgets', type: 'base' } as any)
	//     app.save(c)
	//   }, (app) => { app.delete(app.findCollectionByNameOrId('ts_widgets')) })
	// Register jsvm (MigrationsDir set), bootstrap, RunAllMigrations.
	// Assert: FindCollectionByNameOrId('ts_widgets') succeeds.
	t.Skip("replace with real harness from binds_app_reset_test.go")
}
```

- [ ] **Step 3: Replace the skeletons with the real harness and run**

Fill in both tests using the harness from Step 1, remove the `t.Skip`.
Run: `go test ./plugins/jsvm/ -run 'TestTSHook_EndToEnd|TestTSMigration_EndToEnd' -count=1 -v`
Expected: PASS. If the migration test fails while the hook test passes, the seam was mis-placed (on `p.compile` instead of `filesContent`) — recheck Task A2.

- [ ] **Step 4: Commit**

```bash
git add plugins/jsvm/transform_test.go
git commit -m "test(jsvm): e2e .pb.ts hook and .ts migration run through the seam"
```

## Task A4: Phase A full verification

- [ ] **Step 1: Full green bar on the fork (still goja)**

Run (from `~/code/tinycld/pocketbase`):
```bash
go build ./... && go vet ./plugins/jsvm/... && go test ./plugins/jsvm/... -count=1 && go test -race ./plugins/jsvm/... -count=1
```
Expected: all PASS. **TypeScript hooks & migrations now work on the existing goja engine.**

---

# Phase B — goja → sobek engine swap (fork)

Measured scope: 6 files, 111 `goja.` sites, in `plugins/jsvm/` + `tools/types/`. The `sobek_nodejs` substitution (Task B2) carries the only real risk.

## Task B1: Vet `ohayocorp/sobek_nodejs` (the gate)

**Files:** none yet — this is a decision checkpoint.

- [ ] **Step 1: Fetch and read the port**

Run:
```bash
cd ~/code/tinycld/pocketbase
go get github.com/ohayocorp/sobek_nodejs@latest
```
Then read its `require`, `console`, `process`, `buffer` packages in the module cache (`go doc github.com/ohayocorp/sobek_nodejs/require` etc.). Confirm each exposes the same enable-shape jsvm uses today (`require.Registry` with `.Enable(vm)`; `console.Enable(vm)`; `process.Enable(vm)`; `buffer.Enable(vm)`).

- [ ] **Step 2: Decision gate**

- **If the four modules map 1:1** → proceed to Task B2 depending on the module.
- **If the API differs or the module looks abandoned** → STOP and choose:
  - **Vendor:** copy the needed packages into `plugins/jsvm/internal/sobeknodejs/` and depend on that instead. Then proceed.
  - **Fallback:** abandon the sobek swap; keep goja + the Phase A TS seam (the TS goal is already met). Record the decision in the spec's §7 and skip Phase B/C entirely. Surface this to the user before committing to it.

- [ ] **Step 3: Record the outcome**

Note in the commit message for B2 which path was taken (dependency vs vendored).

## Task B2: Swap engine imports and types

**Files:**
- Modify: `~/code/tinycld/pocketbase/plugins/jsvm/*.go` (all goja-referencing files)

- [ ] **Step 1: Find every goja reference site**

Run:
```bash
cd ~/code/tinycld/pocketbase
grep -rl 'dop251/goja' --include='*.go' plugins/jsvm/ | grep -v goja_nodejs
```
Expected: the set of files to edit (jsvm.go, binds.go, program_source.go, + tests).

- [ ] **Step 2: Swap the engine import and node modules**

Across `plugins/jsvm/`:
- `github.com/dop251/goja` → `github.com/grafana/sobek`
- `github.com/dop251/goja_nodejs/require` → `github.com/ohayocorp/sobek_nodejs/require` (and `console`, `process`, `buffer`) — or the vendored path chosen in B1.
- Every `goja.` identifier → `sobek.` (`goja.New()`, `goja.Runtime`, `goja.Value`, `goja.Object`, `goja.Compile`, `goja.Program`, `goja.FunctionCall`, etc.).

Do this file-by-file, letting the compiler drive. Example for `program_source.go`:
```go
import "github.com/grafana/sobek"

type ProgramSource interface {
	Compile(name, src string, strict bool) (*sobek.Program, error)
}

func (p *plugin) compile(src string, strict bool) (*sobek.Program, error) {
	if p.config.ProgramSource != nil {
		return p.config.ProgramSource.Compile(defaultScriptPath, src, strict)
	}
	return sobek.Compile(defaultScriptPath, src, strict)
}
```

- [ ] **Step 3: Build until clean**

Run: `go build ./plugins/jsvm/`
Expected: iterate on any remaining `goja.` references the compiler flags until it builds.

- [ ] **Step 4: Handle `tools/types` (tygoja)**

Run: `grep -rl 'dop251/goja' --include='*.go' tools/types/`
Read those files. tygoja generates `types.d.ts` from bindings and is goja-typed.
- If it builds after the same import swap, do that.
- If tygoja hard-depends on goja types, keep goja as a **codegen-only** dep in `tools/types` (it never touches the runtime engine) and leave a comment saying so.
Run: `go build ./tools/types/...`
Expected: builds clean.

- [ ] **Step 5: Run jsvm tests under sobek**

Run: `go test ./plugins/jsvm/ -count=1`
Expected: PASS. Fix any sobek API differences surfaced (e.g. method renames). The Phase A TS tests must still pass — TS on sobek is the target state.

- [ ] **Step 6: Commit**

```bash
git add plugins/jsvm/ tools/types/ go.mod go.sum
git commit -m "feat(jsvm): swap goja->sobek engine + ohayocorp/sobek_nodejs"
```

## Task B3: Node-compat + ES2020 assertion tests

**Files:**
- Test: `~/code/tinycld/pocketbase/plugins/jsvm/transform_test.go` (append)

- [ ] **Step 1: Write the node-compat + ES-feature tests**

Append (adapting the end-to-end harness from Task A3):
```go
// Proves ohayocorp/sobek_nodejs require/console/process/Buffer work in a hook.
func TestSobekNodeCompat_EndToEnd(t *testing.T) {
	// Hook body exercising the node modules, e.g.:
	//   console.log('hi')
	//   routerAdd('GET','/node',(e)=>e.json(200,{ buf: Buffer.from('x').length, pid: typeof process.pid }))
	// Assert: 200 and body reflects Buffer/process available.
	t.Skip("replace with real harness from binds_app_reset_test.go")
}

// Proves an ES2020 feature (e.g. optional chaining / nullish coalescing) runs.
func TestES2020Feature_EndToEnd(t *testing.T) {
	// Hook: routerAdd('GET','/es',(e)=>{ const o={}; return e.json(200,{v: o?.a ?? 'ok'}) })
	// Assert: 200 and body {"v":"ok"}.
	t.Skip("replace with real harness from binds_app_reset_test.go")
}
```

- [ ] **Step 2: Fill in the harness and run**

Remove the `t.Skip`s, wire the real harness.
Run: `go test ./plugins/jsvm/ -run 'TestSobekNodeCompat_EndToEnd|TestES2020Feature_EndToEnd' -count=1 -v`
Expected: PASS. `TestSobekNodeCompat_EndToEnd` failing is the concrete signal the port is not a true drop-in → revisit B1's vendor/fallback decision.

- [ ] **Step 3: Commit**

```bash
git add plugins/jsvm/transform_test.go
git commit -m "test(jsvm): node-compat and ES2020 feature run under sobek"
```

## Task B4: Phase B full verification

- [ ] **Step 1: Full green bar on the fork (now sobek)**

Run:
```bash
go build ./... && go vet ./plugins/jsvm/... ./tools/types/... && go test ./plugins/jsvm/... -count=1 && go test -race ./plugins/jsvm/... -count=1
```
Expected: all PASS.

---

# Phase C — Router: adopt the sobek seam type (multi-org)

The `ProgramSource.Compile` return type changed to `*sobek.Program`. The router's `SharedProgramCache` implements that interface, so it must follow. This is the only router change the engine swap forces.

## Task C1: Update `progcache` to `*sobek.Program`

**Files:**
- Modify: `~/code/tinycld/multi-org/internal/progcache/progcache.go`
- Modify: `~/code/tinycld/multi-org/internal/progcache/progcache_test.go`

- [ ] **Step 1: Confirm the fork drives the type**

The router `go.mod` already `replace`s pocketbase → `../pocketbase`. Run:
```bash
cd ~/code/tinycld/multi-org
go build ./internal/progcache/ 2>&1 | head
```
Expected: FAIL — `*goja.Program` no longer satisfies `jsvm.ProgramSource` (which now wants `*sobek.Program`).

- [ ] **Step 2: Swap goja→sobek in progcache**

In `internal/progcache/progcache.go`:
- import `github.com/dop251/goja` → `github.com/grafana/sobek`
- `map[string]*goja.Program` → `map[string]*sobek.Program` (both the struct field at line 24 and the `New()` initializer at line 28)
- `goja.Compile(...)` (line 49) → `sobek.Compile(...)`
- the `Compile` method return type → `*sobek.Program`

Do the same import/type swap in `progcache_test.go`.

- [ ] **Step 3: Build + test**

Run:
```bash
go build ./internal/progcache/ && go test ./internal/progcache/ -count=1
```
Expected: PASS — including the interface-assertion `var _ jsvm.ProgramSource = (*SharedProgramCache)(nil)`.

- [ ] **Step 4: Whole-router build**

Run: `go build ./...`
Expected: builds clean (nothing else references `goja` directly).

- [ ] **Step 5: Commit**

```bash
cd ~/code/tinycld/multi-org
git add internal/progcache/
git commit -m "chore(progcache): follow fork seam to *sobek.Program"
```

## Task C2: Phase C verification

- [ ] **Step 1: Full green bar on the router**

Run:
```bash
go build ./... && go vet ./... && go test ./... -count=1 && go test -race ./... -count=1
```
Expected: all PASS (the existing cross-org sharing + integration tests confirm the swap didn't break isolation or provisioning).

---

# Phase D — Publish-time pre-transpile + publish route (router)

Store holds `.js` for production; production tenants never trip the load-time seam. Also closes HANDOFF finding #1 (no publish route).

## Task D1: `transpileForStore`

**Files:**
- Create: `~/code/tinycld/multi-org/internal/controlplane/transpile.go`
- Test: `~/code/tinycld/multi-org/internal/controlplane/transpile_test.go`

- [ ] **Step 1: Add esbuild to the router module**

Run:
```bash
cd ~/code/tinycld/multi-org
go get github.com/evanw/esbuild/pkg/api@latest
```

- [ ] **Step 2: Write the failing test**

Create `internal/controlplane/transpile_test.go`:
```go
package controlplane

import (
	"strings"
	"testing"
)

func TestTranspileForStore_RewritesTSKeysAndContent(t *testing.T) {
	in := map[string][]byte{
		"server/main.pb.ts":         []byte("const x: number = 1\nrouterAdd('GET','/x',()=>{})"),
		"pb-migrations/001_init.ts": []byte("migrate((app)=>{},(app)=>{})"),
		"server/util.pb.js":         []byte("const y = 2"),
		"client/dist/index.html":    []byte("<html></html>"),
	}
	out, err := transpileForStore(in)
	if err != nil {
		t.Fatal(err)
	}
	// .pb.ts key rewritten to .pb.js, content transpiled (no type annotation).
	js, ok := out["server/main.pb.js"]
	if !ok {
		t.Fatalf("expected server/main.pb.js key; got keys %v", keys(out))
	}
	if strings.Contains(string(js), ": number") {
		t.Fatalf("type annotation not stripped: %s", js)
	}
	if _, stillTS := out["server/main.pb.ts"]; stillTS {
		t.Fatal("original .pb.ts key must be gone")
	}
	// .ts migration key rewritten to .js.
	if _, ok := out["pb-migrations/001_init.js"]; !ok {
		t.Fatalf("expected pb-migrations/001_init.js; got %v", keys(out))
	}
	// .js and client assets untouched, byte-identical.
	if string(out["server/util.pb.js"]) != "const y = 2" {
		t.Fatal(".js must pass through byte-identical")
	}
	if string(out["client/dist/index.html"]) != "<html></html>" {
		t.Fatal("client asset must pass through byte-identical")
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./internal/controlplane/ -run TestTranspileForStore -count=1`
Expected: FAIL — `undefined: transpileForStore`.

- [ ] **Step 4: Implement**

Create `internal/controlplane/transpile.go`:
```go
package controlplane

import (
	"fmt"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// transpileForStore converts TypeScript entries in a package's file map to JS
// before it is written to the immutable store, so production tenants materialize
// only .js (the fork's load-time transpile seam then no-ops). It rewrites the
// map KEY (foo.pb.ts -> foo.pb.js, foo.ts -> foo.js) and the content. Non-.ts
// files (.js, client assets) pass through byte-for-byte. Uses the same esbuild
// loader/target as the fork so behavior matches whether transpile happens here
// or at load.
func transpileForStore(files map[string][]byte) (map[string][]byte, error) {
	out := make(map[string][]byte, len(files))
	for name, content := range files {
		if !strings.HasSuffix(name, ".ts") {
			out[name] = content
			continue
		}
		res := api.Transform(string(content), api.TransformOptions{
			Loader:    api.LoaderTS,
			Target:    api.ES2020,
			Sourcemap: api.SourceMapInline,
		})
		if len(res.Errors) > 0 {
			msgs := make([]string, 0, len(res.Errors))
			for _, e := range res.Errors {
				msgs = append(msgs, e.Text)
			}
			return nil, fmt.Errorf("transpile %s: %s", name, strings.Join(msgs, "; "))
		}
		out[strings.TrimSuffix(name, ".ts")+".js"] = res.Code
	}
	return out, nil
}
```

- [ ] **Step 5: Run to verify it passes**

Run: `go test ./internal/controlplane/ -run TestTranspileForStore -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/controlplane/transpile.go internal/controlplane/transpile_test.go go.mod go.sum
git commit -m "feat(controlplane): transpileForStore (TS->JS at publish time)"
```

## Task D2: Call `transpileForStore` in `PublishPackage`

**Files:**
- Modify: `~/code/tinycld/multi-org/internal/controlplane/provisioning.go` (`PublishPackage`, ~line 173)

- [ ] **Step 1: Read the current PublishPackage**

Run: `sed -n '167,187p' internal/controlplane/provisioning.go`
Confirm it forwards `files` straight to `p.store.Publish(name, version, files)` then records a `packages` row.

- [ ] **Step 2: Transpile before Publish**

At the top of `PublishPackage`, before the `store.Publish` call, insert:
```go
	files, err := transpileForStore(files)
	if err != nil {
		return fmt.Errorf("transpile package %s@%s: %w", name, version, err)
	}
```
(Reuse the existing `err` var if one is already in scope; otherwise declare it. Ensure `fmt` is imported — it already is in this file.)

- [ ] **Step 3: Build + existing tests**

Run: `go build ./internal/controlplane/ && go test ./internal/controlplane/ -count=1`
Expected: PASS (existing publish test uses `.js`/`server/*.pb.js` inputs → untouched by transpile).

- [ ] **Step 4: Commit**

```bash
git add internal/controlplane/provisioning.go
git commit -m "feat(controlplane): transpile TS packages on publish"
```

## Task D3: Add the `POST /api/store/packages` route

**Files:**
- Modify: `~/code/tinycld/multi-org/internal/controlplane/provisioning.go` (`RegisterRoutes`)

Closes HANDOFF finding #1. The route accepts a package (name, version, kind, files as base64) and calls `PublishPackage`.

- [ ] **Step 1: Read the existing route pattern**

Run: `sed -n '189,247p' internal/controlplane/provisioning.go`
Note the exact idiom: `g.POST("/orgs", func(re *core.RequestEvent) error {...}).Bind(apis.RequireSuperuserAuth())`, `re.BindBody(&body)`, `re.BadRequestError(...)`, `re.JSON(200, ...)`.

- [ ] **Step 2: Add the route inside RegisterRoutes**

Add alongside the other `g.POST` routes (files arrive base64-encoded since JSON can't carry raw bytes):
```go
			g.POST("/store/packages", func(re *core.RequestEvent) error {
				var body struct {
					Name    string            `json:"name"`
					Version string            `json:"version"`
					Kind    string            `json:"kind"`
					Files   map[string]string `json:"files"` // path -> base64 content
				}
				if err := re.BindBody(&body); err != nil {
					return re.BadRequestError("invalid body", err)
				}
				files := make(map[string][]byte, len(body.Files))
				for path, b64 := range body.Files {
					raw, err := base64.StdEncoding.DecodeString(b64)
					if err != nil {
						return re.BadRequestError("invalid base64 for "+path, err)
					}
					files[path] = raw
				}
				if err := p.PublishPackage(body.Name, body.Version, files, body.Kind); err != nil {
					return re.BadRequestError(err.Error(), err)
				}
				return re.JSON(200, map[string]any{"name": body.Name, "version": body.Version})
			}).Bind(apis.RequireSuperuserAuth())
```
Add `"encoding/base64"` to the file's imports.

- [ ] **Step 3: Build**

Run: `go build ./internal/controlplane/`
Expected: builds clean.

- [ ] **Step 4: Write a route test**

Append to `internal/controlplane/provisioning_test.go` a test that: bootstraps the control plane, creates a superuser, builds the serve mux, and POSTs a package (with a `pb-migrations/*.ts` file base64-encoded) to `/api/store/packages` with superuser auth; asserts 200 and that a `packages` row exists with the given name/version. (Follow the auth+mux harness from `integration_test.go`.)

Run: `go test ./internal/controlplane/ -run TestPublishRoute -count=1 -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controlplane/provisioning.go internal/controlplane/provisioning_test.go
git commit -m "feat(controlplane): POST /api/store/packages publish route"
```

## Task D4: Extend the integration test with a TS variant

**Files:**
- Modify: `~/code/tinycld/multi-org/internal/controlplane/integration_test.go`

- [ ] **Step 1: Read the existing integration test**

Run: `sed -n '1,60p' internal/controlplane/integration_test.go`
It publishes a JS package, `CreateOrg`s, asserts the tenant DB has the collection, and serves the hook via the manager.

- [ ] **Step 2: Add a TS-authored variant**

Add `TestIntegration_CreateOrgToLoadWithTSSchema` mirroring the existing test but publishing **TypeScript** source:
```go
	// Package with a TS hook and a TS migration. transpileForStore converts these
	// to .js at publish; the tenant should still get the collection and route.
	if err := p.PublishPackage("@tinycld/core", "1.0.0", map[string][]byte{
		"server/main.pb.ts": []byte(
			"interface P { ok: boolean }\n" +
				"routerAdd('GET','/whoami',(e)=>e.json(200,{ ok: true } as P))"),
		"pb-migrations/1700000000_widgets.ts": []byte(
			"migrate((app)=>{\n" +
				"  const c = new Collection({ id:'pbc_widgets_01', name:'widgets', type:'base', fields:[{ id:'w_name', name:'name', type:'text' }] } as any)\n" +
				"  app.save(c)\n" +
				"},(app)=>{ app.delete(app.findCollectionByNameOrId('widgets')) })"),
	}, "official"); err != nil {
		t.Fatalf("PublishPackage(TS): %v", err)
	}
```
Then reuse the existing test's `CreateOrg` + `assertTenantHasWidgets` + manager `/whoami` assertions verbatim.

- [ ] **Step 3: Run**

Run: `go test ./internal/controlplane/ -run TestIntegration_CreateOrgToLoadWithTSSchema -count=1 -v`
Expected: PASS — full stack: TS authored → transpiled at publish → materialized → run on sobek in a real tenant.

- [ ] **Step 4: Commit**

```bash
git add internal/controlplane/integration_test.go
git commit -m "test(controlplane): TS-authored package provisions and serves end-to-end"
```

## Task D5: Phase D verification + live smoke test

- [ ] **Step 1: Full green bar on the router**

Run:
```bash
go build ./... && go vet ./... && go test ./... -count=1 && go test -race ./... -count=1
```
Expected: all PASS.

- [ ] **Step 2: Live smoke test**

Build and boot in proxy mode, then publish a TS package over the new route and drive the tenant:
```bash
go build -o /tmp/serve-multi ./cmd/serve-multi
SM=/tmp/mt_ts_smoke; rm -rf "$SM"
MT_ROOT="$SM" MT_BASE_DOMAIN=tinycld.test MT_TLS_MODE=proxy MT_ADDR=127.0.0.1:8544 \
  MT_SUPERUSER_EMAIL=admin@tinycld.test MT_SUPERUSER_PASSWORD='smoke-pw-1234' \
  /tmp/serve-multi > /tmp/mt_ts_smoke.log 2>&1 &
sleep 4
# auth as superuser (Host: admin.*), POST a TS package (base64 files) to /api/store/packages,
# POST /api/orgs {slug:acme, lockfile:{"@tinycld/core":"1.0.0"}},
# GET Host: acme.tinycld.test /whoami  -> expect {"ok":true}
# sqlite3 "$SM/pb_orgs/acme/pb_data/data.db" "select name from _collections where name='widgets';" -> widgets
```
Expected: `/whoami` returns `{"ok":true}` and the `widgets` collection exists in the tenant DB — proving a TypeScript-authored package runs in a live tenant.

- [ ] **Step 3: Tear down**

```bash
kill %1 2>/dev/null; rm -rf /tmp/mt_ts_smoke /tmp/mt_ts_smoke.log /tmp/serve-multi
```

---

# Final verification (both repos)

- [ ] **Fork:**
```bash
cd ~/code/tinycld/pocketbase && go build ./... && go vet ./plugins/jsvm/... ./tools/types/... && go test ./plugins/jsvm/... -count=1 && go test -race ./plugins/jsvm/... -count=1
```

- [ ] **Router:**
```bash
cd ~/code/tinycld/multi-org && go build ./... && go vet ./... && go test ./... -count=1 && go test -race ./... -count=1
```

Both green ⇒ TypeScript hooks & migrations are authored in TS, transpiled at publish (or load), and run on sobek inside real multi-org tenants.
