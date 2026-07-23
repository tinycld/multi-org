# TypeScript hooks & migrations in the PocketBase fork — Design Spec

**Date:** 2026-07-23
**Repos:** `~/code/tinycld/pocketbase` (the fork), `~/code/tinycld/multi-org` (the router).
**Status:** Approved design, pending implementation plan.

## 1. Goal & context

Let package authors write `.pb.ts` / `.ts` hooks and migrations in real
TypeScript (type annotations, `enum`, `interface`, `as`, optional chaining, …),
running inside the multi-org router's tenant apps, while keeping the PocketBase
fork **thin and rebaseable**.

Today the fork's jsvm plugin *matches* `.pb.ts` / `.ts` files (the patterns at
`plugins/jsvm/jsvm.go:146,150`) but passes their raw bytes straight to the goja
engine — so TS support is cosmetic (an IDE-linter hint). Any real TS syntax fails
to parse. This spec closes that gap.

Context: this is the next step after the multi-org router became operator-runnable
(see `HANDOFF.md`). Tenant hooks/migrations are materialized from a
version-addressed package store; a `ProgramSource` seam already shares compiled
programs across orgs.

### Goals (ranked, from the brainstorm)

1. **TypeScript support** — the actual goal.
2. **More ES6+/ESM features** at runtime — nice-to-have.
3. **Fast steady-state hook execution** — strongly cared about.
4. ~~Persistable bytecode for fast loads~~ — **dropped** (see §7).

## 2. Decisions (locked)

| # | Decision | Choice | Rationale |
|---|---|---|---|
| D1 | TS transpiler | **esbuild-Go** (`github.com/evanw/esbuild/pkg/api`, `Transform`, `Loader: LoaderTS`, `Target: ES2020`, inline sourcemaps) | Pure Go, **no CGO**, in-process, single dep. `github.com/koltyakov/tsgo` proves the esbuild+goja pattern in the wild. |
| D2 | JS engine | **goja → sobek** (`github.com/grafana/sobek`) + **`github.com/ohayocorp/sobek_nodejs`** for require/console/process/buffer | sobek gives ESM + ES2020+ and active maintenance; the Node-compat port exists (goja/sobek APIs are near-identical). |
| D3 | Transpile point | **Publish-time primary** (store holds `.js`), **load-time fallback** (fork transpiles raw `.ts`) | esbuild is importable so load-time is viable; pre-transpiling at publish keeps prod loads pure-JS passthrough. |
| D4 | Bytecode on disk | **Dropped** | Neither goja nor sobek can serialize `*Program` (unexported fields; gob can't). The in-memory `ProgramSource` cache covers load speed instead. |
| D5 | TS 7 native compiler (`microsoft/typescript-go`) | **Not used for transpile** | GA 2026-07-08 but ships **no importable Go API** (all `internal/` packages; binary-only). May later type-check in CI, never at runtime. |

**Why not V8/v8go** (the only path to real bytecode + JIT): CGO + a full rewrite of
PocketBase's goja-coupled binding layer — abandons the thin-fork posture. Rejected
in favor of D2/D4.

## 3. Architecture — the transpile seam

Two entry points read file content into the VM, and they are **asymmetric**:

| Path | Entry | Compiles via | Seam today? |
|---|---|---|---|
| **Hooks** | `registerHooks` → `compileHookFiles` (`jsvm.go:360`) | `p.compile(src, false)` → `ProgramSource` | yes |
| **Migrations** | `registerMigrations` (`jsvm.go:233`) | `vm.RunScript(path, content)` **directly** | **no — bypasses `p.compile`** |

Therefore the transpile step must sit **earlier than `p.compile`**, at the single
chokepoint both paths share: **`filesContent` (`jsvm.go:557`)**, which reads files
from disk for both `registerHooks` and `registerMigrations`.

```go
// plugins/jsvm — applied to each file read by filesContent (both hooks & migrations)
func (p *plugin) transformSource(name string, content []byte) ([]byte, error) {
    if !isTypeScript(name) {              // .pb.ts / .ts by extension
        return content, nil               // .js passes through byte-for-byte
    }
    res := api.Transform(string(content), api.TransformOptions{
        Loader:    api.LoaderTS,
        Target:    api.ES2020,            // matches sobek's ES level
        Sourcemap: api.SourceMapInline,   // stack traces point at TS lines
    })
    if len(res.Errors) > 0 {
        return nil, fmt.Errorf("transpile %s: %s", name, formatEsbuildErrs(res.Errors))
    }
    return res.Code, nil
}
```

**Design properties:**

- **Extension-keyed, not content-sniffed.** Reuses the existing `.pb.ts`/`.ts`
  signal. A `.js` file is untouched — zero overhead, zero behavior change for
  existing JS packages (preserves the thin-fork guarantee).
- **Runs before `p.compile`.** By the time source reaches
  `ProgramSource.Compile(name, src, strict)`, `src` is already JS — so the
  cross-org compiled-program cache still keys on the JS source and still dedups one
  `*Program` across orgs. The memory-sharing win is preserved.
- **Transpilation is not cached by `ProgramSource`** (that caches compiled
  programs, not transpile output). Identical `.ts` across N orgs ⇒ N transpiles at
  load. Acceptable because (a) load cost is explicitly not the priority — steady
  execution is — and (b) prod packages arrive pre-transpiled as `.js` (§5), making
  this a no-op in production.
- **No mode flag.** The file extension *is* the switch: prod `.js` ⇒ passthrough;
  dev `.ts` ⇒ transpile. Same code path.

**Edge case:** the `migrate(up, down)` global call and the hook-file `types.d.ts`
reference-directive are source text. Transpile runs before both — `migrate(...)` /
`routerAdd(...)` are plain calls that survive esbuild untouched; the `.d.ts`
reference comment is stripped as a comment (it was only ever an IDE hint).
Asserted in tests (§6).

## 4. Architecture — the goja → sobek swap

**Measured scope:** 6 non-test files, 111 `goja.` reference sites, confined to
`plugins/jsvm/` and `tools/types/`. Wide but mechanical, with one non-mechanical
piece (the nodejs modules).

**Category 1 — engine import + type references (~106 sites, mechanical).**
`github.com/dop251/goja` → `github.com/grafana/sobek`; `goja.Runtime`/`goja.Value`/
`goja.New()`/`goja.Compile`/`goja.Object`/… → `sobek.*`. The `ProgramSource` seam
changes one type:

```go
// program_source.go — the one seam type change
type ProgramSource interface {
    Compile(name, src string, strict bool) (*sobek.Program, error) // was *goja.Program
}
```

This ripples to exactly one place in the router: the multi-org
`internal/progcache` `SharedProgramCache`, whose `map[string]*goja.Program`
becomes `*sobek.Program`. **That is the only router change the swap forces** — the
reason the seam keeps this contained.

**Category 2 — the nodejs modules (the real work + the risk).**
`goja_nodejs/{require,console,process,buffer}` (`jsvm.go:28-31`) →
`ohayocorp/sobek_nodejs/{…}`. Call sites (`registry.Enable(vm)`,
`console.Enable(vm)`, …) should map 1:1. **Risk:** `ohayocorp/sobek_nodejs` is a
single-maintainer, ~2025 third-party module, not grafana-official.

- **Vetting gate (during implementation):** read its source, confirm it covers the
  exact modules PocketBase uses, confirm it tracks a recent sobek.
- **Fallback if vetting fails:** vendor it into the fork under
  `plugins/jsvm/internal/sobeknodejs/` — removes the supply-chain dependency at
  the cost of carrying the code.
- **Escape hatch:** if the port is unusable *and* vendoring is heavier than
  expected, fall back to **goja + TS-only** (keep D1/D3, drop D2). This still
  delivers the TS goal; only the ES6/ESM wish is deferred. Surfaced at the vetting
  checkpoint, not pushed through.

**Category 3 — `tools/types` (tygoja `.d.ts` generation).**
`pocketbase/tygoja` is goja-specific. Resolved during implementation once coupling
is seen: if tygoja reads only goja reflection, the generated `.d.ts` is
engine-agnostic and only imports swap; if tightly goja-typed, keep goja *solely as
a codegen-time dep here* (never touches runtime) or port the thin part. Isolated
from the runtime path either way.

**Unchanged:** all `Bind*` functions (`BindCore`, `BindDbx`, …) — written against
the Runtime API sobek preserves; only referenced type names change.

**Rebase posture:** this diverges the fork from upstream (goja) more than the TS
seam alone — the accepted cost of D2. Divergence is concentrated in the
import/type layer + one dependency, not logic, so future rebases conflict on
imports (easy), not behavior (hard).

## 5. Architecture — publish-pipeline pre-transpile

**Principle:** the store holds **plain `.js`** for production. TS is an authoring
format; by the time a version lands in the immutable, version-addressed store, its
`server/*.pb.ts` and `pb-migrations/*.ts` have become `.js`. Prod org loads are
then pure-JS passthrough (the §3 seam is a no-op), and the runtime never depends on
when/how transpilation happened.

**Hook point:** `Provisioner.PublishPackage(name, version, files, kind)` receives a
`files map[string][]byte`. Transpile that map before `store.Publish`:

```
files (authored: server/main.pb.ts, pb-migrations/001_x.ts, client/dist/**)
   │
   ▼  transpileForStore(files)          ← NEW
   │     server/main.pb.ts      → server/main.pb.js     (esbuild api.Transform)
   │     pb-migrations/001_x.ts → pb-migrations/001_x.js
   │     .js / client/** / other → untouched
   ▼
store.Publish(name, version, transpiledFiles)   (immutable, content-addressed)
```

- **Same esbuild helper as the fork** (same loader, same ES2020 target) — one
  transpile implementation, one place to keep the target synced with sobek's ES
  level. The store stays dumb (writes arbitrary relative paths — no store change).
- **Filename-key rewrite is required:** `server/main.pb.ts` → `server/main.pb.js`
  and `pb-migrations/001_x.ts` → `…​.js`, because `materialize.linkServerHooks`
  symlinks `server/` into the tenant's `pb_hooks/`, and the tenant should see
  already-transpiled `.js` (not re-trip the load-time seam). The
  `migrate(...)`/`routerAdd(...)` bodies survive transpile intact (§3).
- **Subsumes HANDOFF finding #1:** the publish path is being touched anyway, so
  **add the missing `POST /api/store/packages` HTTP route** here (it was specced
  but never bound — `PublishPackage` is Go-only today) so operators can publish
  transpiled packages over the API.

**Division of labor (decided): transpile at publish, in Go.** The pnpm workspace
ships raw `.ts`; `PublishPackage` transpiles. Single source of truth for transpile
config; works regardless of who publishes. (Alternative — transpile in the pnpm
build — was rejected: splits config across two toolchains and forces every
publisher to run the build. A future CI type-check gate is where TS 7 `tsgo` could
slot in: type-check in CI, transpile in Go.)

**Net for prod:** every tenant materializes `.js`; the load-time seam does nothing;
`ProgramSource` shares compiled programs across orgs; TS never runs at request
time — satisfying the steady-state-speed priority.

## 6. Testing & verification

**Fork unit tests (`plugins/jsvm/`, under sobek):**

- `transformSource` on `.ts`: type annotations, `enum`, `interface`, `as`,
  optional chaining transpile to runnable JS; `.js` passes through byte-identical.
- `.pb.ts` **hook** end-to-end: typed handler registers + serves a route (via
  `filesContent` → transform → `p.compile` → `ProgramSource`).
- `.ts` **migration** end-to-end: `migrate((app) => {...})` runs + creates a
  collection — proving the seam covers the path that bypasses `p.compile`.
- `migrate` / `routerAdd` / `types.d.ts` reference survive transpile (§3 edge case).
- Transpile error surfacing: a `.ts` syntax error yields a clear `transpile <file>:
  …` message, not an opaque engine parse failure.
- ES2020/ESM feature: a feature goja lacked but sobek+ES2020 supports runs
  (proves the swap delivered the "more ES6" goal).
- **Isolation invariant (carried over):** existing `ProgramSource` cross-org
  isolation tests pass under sobek — a shared `*sobek.Program` holds no per-org
  state.

**`sobek_nodejs` vetting gate:** a hook exercising `require()`, `console.log`,
`process`, `Buffer` — the concrete pass/fail for whether `ohayocorp/sobek_nodejs`
is a real drop-in. Failure ⇒ vendor-or-fallback before going deeper.

**Multi-org router tests (`~/code/tinycld/multi-org/`):**

- `transpileForStore`: `PublishPackage` given `server/x.pb.ts` stores
  `server/x.pb.js` with transpiled content; `.js`/`client/**` untouched; keys
  rewritten.
- `progcache` under sobek: `SharedProgramCache`'s `*sobek.Program` map still dedups
  identical sources across orgs.
- **Extend `TestIntegration_CreateOrgToLoadWithSchema`** with a TS variant: publish
  a package with a `.pb.ts` hook + `.ts` migration → `CreateOrg` → tenant DB has
  the collection **and** serves the hook route. Full-stack proof.
- Publish HTTP route: `POST /api/store/packages` accepts a package that becomes
  provisionable (closes finding #1).

**Live smoke test:** boot `serve-multi` (proxy mode), publish a **TypeScript**
package via the new route, `POST /api/orgs`, hit the TS-authored hook route,
confirm the TS migration's collection exists in the tenant DB.

**Full green bar in BOTH modules** (the seam type change spans both — verify
together):
```sh
go build ./... && go vet ./... && go test ./... -count=1 && go test -race ./...
```

## 7. Dropped / out of scope

- **On-disk bytecode caching** — infeasible on goja/sobek (`*Program` not
  serializable). Would require V8/v8go (CGO + binding rewrite). Shared in-memory
  `ProgramSource` cache is the substitute.
- **V8/v8go engine** — rejected (abandons thin-fork posture); recorded as the
  alternative if bytecode/JIT ever becomes a hard requirement.
- **TS 7 (`typescript-go`) as the transpiler** — no importable Go API yet
  (binary-only). Revisit for CI type-checking when its 7.1 API ships.
- **Runtime type-checking** — esbuild transpiles, it does not type-check. Type
  safety is an authoring/CI concern (future `tsgo` gate), not a runtime one.

## 8. Delivery order (for the plan)

1. Fork: add esbuild dep + `transformSource` seam at `filesContent` (still goja) —
   TS hooks & migrations work; smallest independently-testable slice.
2. Fork: goja → sobek swap (Categories 1–3) + `sobek_nodejs` vetting gate.
3. Router: `progcache` `*sobek.Program` type change; rebuild green.
4. Router: `transpileForStore` in `PublishPackage` + the `POST /api/store/packages`
   route.
5. Router: extend the integration test + live smoke test.
