# Handoff — Multi-Org PocketBase Router

**Updated:** 2026-07-23 (was 2026-07-22)
**Goal:** Make one PocketBase process host many organizations — each org its own
SQLite DB, client JS bundle, and server-side JS handlers — sharing versioned code
where identical, isolated where not.

This documents everything built, where it lives, what's pushed vs. local, and
what remains.

> **⚠️ Paths changed 2026-07-23.** The router moved
> `~/code/multitenant` → **`~/code/tinycld/multi-org`** and its Go module was
> renamed `tinycld.org/multitenant` → **`tinycld.org/multi-org`**. The PocketBase
> fork moved `~/code/vendor/pocketbase` → **`~/code/tinycld/pocketbase`**, and the
> router's `go.mod replace` is now the relative **`../pocketbase`**. Both are
> nested independent git repos inside the tinycld workspace (gitignored by the
> parent; not pnpm members).

---

## TL;DR — current state

The router is **operator-runnable and proven end-to-end live**, not just in a
test. A booted `serve-multi` (proxy mode) creates a control-plane superuser from
env, provisions an org via `POST /api/orgs`, brings that tenant up **with its
application collections** (materialized from the package's `pb-migrations`), and
serves the tenant's custom hook route at `<slug>.<domain>`. All three former
prerequisites are closed.

**As of 2026-07-23 the system also runs TypeScript.** Package authors write
`.pb.ts` / `.ts` hooks and migrations; they are transpiled TS→JS (esbuild) and run
on the **sobek** JS engine (the fork was swapped goja→sobek for ESM/ES2020+, with
`sobek_nodejs` vendored in). Transpilation happens at **publish time** (the store
holds `.js`) with a **load-time** fallback for raw `.ts`. Proven live: a TypeScript
package published over HTTP → stored as `.js` → provisioned → its TS-authored hook
served and its TS migration's collection present in the tenant DB. See §9.

Four bodies of work:

1. **PocketBase fork seams + TypeScript engine** (`~/code/tinycld/pocketbase`) — the
   two additive embed seams (§1) *plus* the TypeScript transpile seam and the
   goja→sobek swap (§9). All on `feat/multitenant-fork`.
2. **Workspace version bump** — PocketBase `v0.38.1 → v0.39.8` across all 8 tinycld
   Go modules. Local commits, unpushed.
3. **The multi-org router** (`~/code/tinycld/multi-org`) — a private Go module,
   branch `feat/operator-runnable`, 8 packages + TypeScript publish path, all green
   + race-clean. Local-only, no remote.
4. **TypeScript support** (§9) — spans both repos; the fork transpiles + runs TS on
   sobek, the router transpiles at publish.

Design spec + implementation plans:
- Fork seams + router design (2026-07-22) live in the fork's git history at
  `docs/superpowers/{specs,plans}/2026-07-22-*.md` (on the branches that committed
  them; not in the `feat/multitenant-fork` working tree).
- Operator-runnable plan: `~/.claude/plans/streamed-orbiting-pretzel.md`.
- **TypeScript** spec + plan (committed in this repo):
  `docs/superpowers/specs/2026-07-23-typescript-hooks-design.md` and
  `docs/superpowers/plans/2026-07-23-typescript-hooks.md`.

---

## 1. PocketBase fork seams

**Repo:** `~/code/tinycld/pocketbase` (a clone of `pocketbase/pocketbase`;
`origin` = upstream, `fork` = `git@github.com:nathanstitt/pocketbase.git`).

Two narrow, backward-compatible, nil-default extension points were added so a
separate (closed) router can embed and multiplex PocketBase apps. Both are
**opt-in — existing single-app behavior is byte-for-byte unchanged.**

### Seam A — `jsvm.ProgramSource` (PR #1, pushed)

An optional hook on `jsvm.Config` letting an embedder share compiled programs
across plugin instances. **As of the goja→sobek swap (§9) the interface returns
`*sobek.Program`** (it was `*goja.Program` when the seam was first authored /
pushed as PR #1):

```go
type ProgramSource interface {
    Compile(name, src string, strict bool) (*sobek.Program, error)
}
```

Nil (default) → compile directly with the engine. Set → route all hook-file +
callback compilation through it. Hook files compile sloppy (matching `RunScript`);
callbacks compile strict (matching the prior `MustCompile(..., true)` sites).

> ⚠️ The **pushed PR #1 branch** (`feat/jsvm-programsource`) still uses
> `*goja.Program` — the sobek swap lives only on the integration branch
> `feat/multitenant-fork` (§9). If PR #1 is upstreamed as-is it stays goja; the
> sobek swap is a downstream-only divergence unless separately upstreamed.

- **Branch:** `feat/jsvm-programsource` (commit `6644c6af`, based on the released
  tag **`v0.39.8`** + one commit). **Pushed to `fork`** — clean, PR-ready.
- **Files:** `plugins/jsvm/{program_source.go, jsvm.go, binds.go}` + tests.
- The PR body is ready — see [Loose ends](#6-loose-ends--next-actions) for where it is.

### Seam B — `apis.BuildServeMux` (PR #2, staged)

Builds an app's `http.Handler` (router + CORS + admin UI + `OnServe`) **without**
starting a server, so an embedder that owns its own `http.Server` can build one mux
per app. `apis.Serve` was refactored to share the base-router construction; its
behavior is unchanged.

- Lives on `feat/jsvm-programsource-buildservemux` (older, v0.38.1-era base — also
  pushed to `fork`, but **stale/bloated**; do not PR as-is).
- **Rebuild it clean off `v0.39.8`** for the real PR #2 (same approach as PR #1:
  branch off `cc4e8570`/`v0.39.8`, apply only the two `apis/serve.go` +
  `build_serve_mux_test.go` changes, no docs).

### Integration branch (what the router actually builds against)

- **Branch:** `feat/multitenant-fork` (commit `2c4bcc31`) = `feat/jsvm-programsource`
  + the two `BuildServeMux` commits cherry-picked. **This is the currently
  checked-out branch**, and the router's `go.mod replace` (`../pocketbase`) points
  at this working tree. Local-only; not for upstream.

**Fork delivery plan (spec D12):** submit both seams as upstream PRs. If accepted,
the downstream `replace` is dropped and everything becomes a plain library import.
Until then, the router builds against the local fork via `replace`.

---

## 2. Workspace version bump: v0.38.1 → v0.39.8

**Why:** the fork seams are based on `v0.39.8`; the tinycld workspace pinned
`v0.38.1`. Bumping aligns everything so the router's `replace` has no version skew.

**Scope:** all 8 Go modules — `tinycld/core/server` + `tinycld/server` (app shell,
same repo) and the 6 feature siblings (`mail/calendar/contacts/drive/text/calc`,
each its own repo).

**Result:** clean bump — **zero source changes**, only `go.mod`/`go.sum`. Every
module builds, tests pass, `go vet` clean; the assembled workspace builds.

- **Each repo is on branch `chore/bump-pocketbase-v0.39.8`** with one commit
  (`tinycld` HEAD `8fff4e4`). **None pushed.**
- **Tooling gotcha (important for future bumps):** feature-sibling modules can't be
  `go mod tidy`'d standalone — they resolve `tinycld.org/core` only via `go.work`
  (untracked, per-developer). Recipe used: `go mod edit -require ...@v0.39.8` →
  temp-add `replace tinycld.org/core => ../../tinycld/core/server` → `GOWORK=off go
  mod tidy` → drop the temp replace. The app shell (`tinycld/server`) additionally
  can't be `GOWORK=off` tidied (it imports generated `tinycld.org/packages/*`); its
  go.mod require was edited directly and validated by the assembled build. Its
  committed `replace` path is `../core/server` (do not let a bump tool rewrite it to
  `../../tinycld/core/server`).

**To ship:** push each repo's bump branch (7 pushes / PRs), or fold into a
coordinated release (see the `release` skill). These are independent of the
router work and can land anytime.

---

## 3. The multi-org router (`~/code/tinycld/multi-org`)

Private Go module `tinycld.org/multi-org`, **22 commits, HEAD `fb4c6b0`, branch
`feat/operator-runnable`, no remote, clean tree.** ~1150 LOC production Go across
8 packages, all tests green and race-clean.

Imports the fork via `go.mod`:
`replace github.com/pocketbase/pocketbase => ../pocketbase`
(the fork must be on `feat/multitenant-fork` — it currently is).

### What it does

One process hosts N orgs. A fronting server dispatches by subdomain to a
control-plane PocketBase app (registry + provisioning) or a lazily-loaded tenant
app. Each tenant is stock PocketBase with `pb_hooks`/`pb_public`/`pb_migrations`
materialized as symlink farms from a version-addressed package store; compiled JS
hook programs are shared across orgs via a process-wide cache implementing the
fork's `ProgramSource`.

### Package map

| Package | What |
|---|---|
| `internal/store` | Immutable version-addressed package store. |
| `internal/lockfile` | Per-org `{name:version}` lockfile; parse + resolve vs. store. |
| `internal/materialize` | Symlink-farm `pb_hooks` (from `server/`), `pb_public` (from `client/dist/`), **and `pb_migrations` (from `pb-migrations/`)**. |
| `internal/progcache` | `SharedProgramCache` → the fork's `jsvm.ProgramSource`. |
| `internal/controlplane` | Control-plane app: `orgs/packages/deployments` schema, `Provisioner`, HTTP routes, `OrgLookup`. |
| `internal/orgmanager` | Lazy per-org app loader: materialize → bootstrap PB+jsvm(shared cache) → `BuildServeMux`; singleflight, `Evict`, idle sweeper. |
| `internal/frontrouter` | `Host` → subdomain dispatch. |
| `internal/server` | Single `http.Server`; TLS mode = proxy / file / autocert; graceful shutdown. |
| `cmd/serve-multi` | Wires it all together; env-driven superuser bootstrap. |

### Proven working

- **Cross-org program sharing** (`internal/orgmanager/e2e_test.go`): two orgs boot
  independently, each serves `/api/health` **and** its own custom `/whoami` JS-hook
  route, and loading a second org with identical hooks **adds zero new compiled
  programs** (`TestE2E_SecondOrgAddsNoNewPrograms`) — the whole reason for Seam A,
  verified across the fork boundary.
- **Full provisioning chain** (`internal/controlplane/integration_test.go`,
  `TestIntegration_CreateOrgToLoadWithSchema`): the real
  `CreateOrg → OrgLookup → orgmanager.load` path (no stub lookup). A package
  carrying a `pb-migrations` schema is published, `CreateOrg` runs, the tenant DB
  gains the collection, and the manager serves the tenant's hook route.
- **Live smoke test (2026-07-23):** booted `serve-multi` in proxy mode; superuser
  bootstrapped from env; `POST /api/orgs` → `active`; `acme.<domain>/whoami` served;
  the package's `widgets` collection confirmed present in acme's own `data.db`
  (materialized via the `pb_migrations` symlink); reserved slug `admin` rejected 400.

---

## 4. Operator-runnable work — DONE (Track A, 2026-07-23)

Commit `207ef4b` (`feat: make the router operator-runnable`) closed the three
former prerequisites. Approved plan: `~/.claude/plans/streamed-orbiting-pretzel.md`.

1. **Control-plane superuser bootstrap** — `cmd/serve-multi/main.go`
   `ensureSuperuser` upserts a superuser from `MT_SUPERUSER_EMAIL` /
   `MT_SUPERUSER_PASSWORD` after migrations (idempotent; logs and no-ops when
   unset). The provisioning API is now usable on a fresh `MT_ROOT`.
2. **Tenant application schema** — `materialize` gained a third step
   (`linkMigrations`) that symlinks each package's `pb-migrations/*.js` into the
   org's `pb_migrations`, **erroring on cross-package filename collisions**
   (mirroring tinycld's single-tenant generator guarantee). `bootstrapTenantOnce`
   now registers jsvm so those JS migrations actually run at first provision.
   Provisioned tenants boot with their application collections.
3. **TLS modes** — `internal/server/serve.go` honors `MT_TLS_MODE`: **`proxy`**
   (plain HTTP behind a TLS-terminating LB/proxy — the default and simplest real
   deploy), **`file`** (pre-issued `*.<domain>` wildcard cert+key via
   `MT_TLS_CERT`/`MT_TLS_KEY`), and **`autocert`** (retained; still needs a DNS-01
   solver for real wildcards).

Also folded in: **reserved-subdomain rejection** (`validSlug` now rejects
`admin`/`www`, which the front router can't reach) and the **dedicated integration
test** above.

---

## 5. Remaining gaps / findings (surfaced 2026-07-23, NOT yet done)

Gaps found while making the router runnable:

1. ~~**No HTTP route to publish packages.**~~ **CLOSED (2026-07-23, §9).** The
   `POST /api/store/packages` route (superuser-guarded, base64 file payloads) was
   added as part of the TypeScript work and is proven live.
2. **Control-plane collections leak into every tenant.** `orgs`/`packages`/
   `deployments` are registered into the process-global `core.AppMigrations`, so
   every tenant DB also runs them (observed `orgs`, `packages`, `deployments` in
   acme's `data.db`). The spec is explicit that **no org/membership data belongs in
   a tenant** — a tenant having an `orgs` table contradicts the isolation model.
   Fix likely means registering the control-plane schema only on the control-plane
   app (e.g. app-scoped migration registration) rather than globally.
3. **Tenant JS runs with full host capabilities → PARTIALLY mitigated in-process;
   NOT contained.** Tenant hooks + migrations now run under the fork's jsvm
   **Sandboxed** mode (`jsvm.Config{Sandboxed: true}`): a deny-by-default allowlist
   that withholds `$os`/`$http`/`$filesystem`/`$filepath` from **both** the hook and
   migration runtimes, scrubs `process.env`/`process.argv` (so `MT_SUPERUSER_PASSWORD`
   et al. are unreachable), **withholds `$apis.static`**, **denies file-based
   `require()`** (native modules only), and **restricts `$template` to `loadString`**.
   Enabled at both untrusted-code call sites — runtime `orgmanager.load` and
   provision-time `controlplane.bootstrapTenantOnce` — each using `jsvm.Register`
   (returning the error) not `MustRegister`, so a load-time throw fails **closed** for
   that one org instead of panicking the shared process. Spec + plan:
   `docs/superpowers/specs/2026-07-23-tenant-jsvm-sandbox-design.md` and
   `docs/superpowers/plans/2026-07-23-tenant-jsvm-sandbox.md`. See also the README's
   "Tenant JS security boundary" section.

   **⚠️ This is blast-radius reduction, NOT containment — and there is a DEMONSTRATED,
   still-open in-process bypass.** `$app.db()` gives tenant JS a raw SQL surface over
   the shared connection; a sandboxed hook running `ATTACH DATABASE '<other-org>/data.db'`
   inside a transaction reads another org's secrets and can create arbitrary `.db`
   files (arbitrary host-file read/write). Our SQLite driver (**modernc**) exposes **no
   authorizer API**, so this class can't be cleanly contained in-process, and `$app`
   exposes further host-reaching surface (`newFilesystem`, `createBackup`/`restoreBackup`).
   Successive adversarial audits each found another capability re-entering through a
   *kept* surface (`$apis.static` → `require` → `$template` → `$app.db()` raw SQL) —
   the conclusion (2026-07-23) is that **allowlisting the full stock `$app`/DB API
   against a hostile author in one shared process is the wrong altitude.** The
   in-process hardening stands as defense-in-depth; it does **not** make tenant authors
   safe to treat as untrusted.

**The required next deliverable is OS-level per-process isolation** (no longer
"optional hardening" — it is THE security boundary). Each org's app runs in its own
process confined to its own directory: per-uid + a filesystem namespace/chroot so
`ATTACH '<abs path>'` (and any host-path open) physically fails at the OS layer,
cgroup CPU/memory/pids limits, and a scrubbed environment. That confines filesystem,
DB, resource, **and unknown-future** vectors uniformly — which in-process allowlisting
provably cannot. The `GetOrg → http.Handler` seam in `frontrouter` is the drop-in
point for a reverse-proxy-to-subprocess model. **Until it lands, do NOT treat tenant
authors as untrusted in production.** Still also unaddressed in-process: sobek engine
escapes, and CPU/memory/wall-clock DoS (no resource limits).

**Cleanup (non-blocking, still open):** store "content-addressed" naming is
vestigial (`ContentHash`/`content_hash`/`manifest` unused — either wire or drop);
`lockfile.Resolve` doesn't run the `peerVersions` solver yet (spec §7 follow-on).

**The Track-B follow-on — tinycld de-org-ing (spec §11):** remove `orgs`/`user_org`
collections + FKs from the tinycld app, collapse `useOrgLiveQuery`/`OrgScope` to
auth-based rules, org switcher via the parent-domain hint cookie. This is the
larger app-facing effort that makes orgs independent inside the tinycld codebase.
It's now unblocked (the router can produce a live tenant to verify against) but
needs its own plan.

---

## 6. Loose ends / next actions

Pick up any of these independently:

- **OS-level per-process tenant isolation (the required security boundary — see §5
  finding #3).** Scoped follow-up brief with the constraints, existing anchors, and
  the open decisions is at
  `docs/superpowers/specs/FOLLOWUP-os-process-isolation.md`. Run it through
  brainstorming (deployment-target/isolation-primitive decision first) → spec → plan.
- **PR #1 (ProgramSource):** open it from `nathanstitt/pocketbase:feat/jsvm-programsource`
  against `pocketbase/pocketbase` (short summary of Seam A + the sloppy-mode note +
  backward-compat statement).
- **PR #2 (BuildServeMux):** rebuild a clean branch off `v0.39.8` (the pushed
  `-buildservemux` branch is stale), then PR.
- **Ship the version bump:** push the 7 `chore/bump-pocketbase-v0.39.8` branches
  (or fold into a coordinated release).
- **Close finding #2 (tenant schema leak)** from §5. (Finding #1, the publish
  route, is now closed — §9.)
- **Delete the stale fork branch** `feat/jsvm-programsource-buildservemux` from the
  remote once PR #2's clean branch exists.
- **Give the multi-org module a remote** if it should be shared/CI'd.
- **Plan Track B** (tinycld de-org-ing, spec §11).
- **TypeScript follow-ons (§9):** the goja→sobek swap is a downstream-only
  divergence — decide whether to upstream it or keep it fork-local; and the two
  esbuild transpile call sites (fork `transformSource`, router `transpileForStore`)
  are duplicated across repos, kept in sync by a golden test rather than a shared
  helper.

---

## 7. Git state (what's where)

| Repo | Branch | HEAD | Pushed? |
|---|---|---|---|
| `~/code/tinycld/pocketbase` | `feat/jsvm-programsource` | `6644c6af` | **Yes** (`fork`) — clean PR #1 (goja-era) |
| `~/code/tinycld/pocketbase` | `feat/jsvm-programsource-buildservemux` | (older) | Yes (`fork`) — **stale, don't PR** |
| `~/code/tinycld/pocketbase` | `feat/multitenant-fork` | `0da4c670` | No — local integration (checked out); +8 TS/sobek commits since `2c4bcc31` (§9) |
| `~/code/tinycld/multi-org` | `feat/operator-runnable` | `da08173` | **No remote**; +TS commits since `fb4c6b0` (§9) |
| `~/code/tinycld` (core+shell) | `chore/bump-pocketbase-v0.39.8` | `8fff4e4` | No |
| `mail/calendar/contacts/drive/text/calc` | `chore/bump-pocketbase-v0.39.8` | (each) | No |

⚠️ The router builds against the fork's **working tree**, which must stay on
`feat/multitenant-fork` for the `../pocketbase` replace to see both seams **and the
sobek engine**. If you check out another fork branch, the router won't compile
(missing `BuildServeMux`, and/or `*goja.Program` vs `*sobek.Program` mismatch).

Both `multi-org/` and `pocketbase/` are gitignored by the parent `~/code/tinycld`
repo (entries just below the `link-members.ts` auto-managed block) and are not
pnpm workspace members, so workspace tooling ignores them.

---

## 8. Verify the current state

```sh
# Router builds + all tests green + race-clean (fork must be on feat/multitenant-fork):
cd ~/code/tinycld/multi-org && go build ./... && go vet ./... && go test ./... -count=1 && go test -race ./...

# The cross-org sharing promise + the full provisioning chain (JS and TS variants):
go test ./internal/orgmanager/ -run TestE2E -v
go test ./internal/controlplane/ -run TestIntegration_CreateOrgToLoadWithSchema -v
go test ./internal/controlplane/ -run TestIntegration_CreateOrgToLoadWithTSSchema -v

# TypeScript on the fork (transpile seam + sobek engine, incl. vendored node modules):
cd ~/code/tinycld/pocketbase && go test ./plugins/jsvm/... -count=1 && go test -race ./plugins/jsvm/ -count=1

# Live operator flow (proxy mode, no TLS needed for the smoke test):
#   MT_ROOT=/tmp/mt_smoke MT_BASE_DOMAIN=tinycld.test MT_TLS_MODE=proxy MT_ADDR=127.0.0.1:8543 \
#     MT_SUPERUSER_EMAIL=admin@tinycld.test MT_SUPERUSER_PASSWORD='<pw>' ./serve-multi
#   Then, as superuser on admin.<domain>: POST /api/store/packages a package whose files
#   are base64-encoded .pb.ts/.ts source (transpiled to .js in the store), POST /api/orgs,
#   then GET <slug>.<domain>/<hook route>. (A package can also be seeded on disk under
#   <MT_ROOT>/packages/<name>/<version>/ using already-.js files.)

# Workspace bump holds:
cd ~/code/tinycld/tinycld/core/server && go test ./...
```

---

## 9. TypeScript hooks & migrations + goja→sobek (done 2026-07-23)

Package authors write `.pb.ts` / `.ts` hooks and migrations in real TypeScript
(types, `interface`, `enum`, `as`, optional chaining). Spec + plan:
`docs/superpowers/{specs,plans}/2026-07-23-typescript-hooks*.md`.

**Decisions (from the brainstorm):** esbuild-Go for transpile (pure Go, no CGO,
`Loader: LoaderTS`, `Target: ES2020`, inline sourcemaps); engine goja→**sobek**
(`github.com/grafana/sobek`) for ESM/ES2020+; **on-disk bytecode caching dropped**
(neither goja nor sobek can serialize `*Program`; the in-memory `ProgramSource`
cache covers load speed). TS 7's native Go compiler was evaluated and rejected as
the transpiler (no importable Go API yet — binary-only).

### In the fork (`~/code/tinycld/pocketbase`, `feat/multitenant-fork`)

- **Transpile seam** — `plugins/jsvm/transform.go`: `transformSource(name, content)`
  transpiles `.ts` via esbuild, passes `.js` through byte-for-byte, and guards
  empty input (so an empty `.pb.ts` stays 0 bytes and `registerHooks` still fires
  its `types.d.ts` bootstrap). Wired into `filesContent` (`jsvm.go`) — the single
  chokepoint **both** `registerHooks` and `registerMigrations` use. This placement
  is load-bearing: migrations run via `vm.RunScript` and bypass `p.compile`, so a
  seam at `p.compile` alone would miss `.ts` migrations.
- **Engine swap goja→sobek** — every `goja.` → `sobek.` across `plugins/jsvm/` +
  `tools/types/`; `ProgramSource.Compile` now returns `*sobek.Program`.
- **Vendored `sobek_nodejs`** — the require/console/process/buffer modules (+
  transitive util/goutil/errors) are copied into
  `plugins/jsvm/internal/nodejs/**` (MIT, provenance in that dir's README) rather
  than depended on as the 1-star `ohayocorp/sobek_nodejs` module. The fork is
  self-contained; no external node-compat dep.

### In the router (`~/code/tinycld/multi-org`, `feat/operator-runnable`)

- **`progcache`** — `SharedProgramCache` now holds `*sobek.Program` (follows the
  seam). Only forced router change from the engine swap.
- **Publish-time transpile** — `internal/controlplane/transpile.go`
  `transpileForStore(files)` converts `.pb.ts`→`.pb.js` / `.ts`→`.js` (keys +
  content) before `store.Publish`, so the store holds `.js` and production tenants
  materialize pure JS (the fork's load-time seam then no-ops). `.d.ts` files pass
  through untranspiled. Called from `PublishPackage`.
- **`POST /api/store/packages`** — the publish route (superuser-guarded, files as
  base64) — closes §5 finding #1.

### Proven

- Fork: `plugins/jsvm/transform_test.go` — unit (transpile / `.js` passthrough /
  empty guard / error surfacing), e2e `.pb.ts` hook + `.ts` migration through the
  seam, node-compat (Buffer/process/console/require) and ES2020 (`?.`/`??`) on
  sobek. Vendored packages' own suites pass.
- Router: `transpile_test.go` (key-rewrite, `.d.ts` passthrough, ES2020-stability
  golden test), and `integration_test.go`
  `TestIntegration_CreateOrgToLoadWithTSSchema` — the full stack: TS published via
  `PublishPackage` → transpiled → materialized → tenant collection present + hook
  route serves on sobek. A separate reviewer empirically confirmed this test FAILS
  if transpile is neutered.
- **Live:** a TypeScript package `POST`ed to `/api/store/packages` was stored as
  `.js`, provisioned, its hook served `{"ok":true}`, and its migration's collection
  was present in the tenant DB.

### Consequences / watch-outs

- The **sobek swap is downstream-only.** The pushed PR #1 branch is still goja
  (§1). Upstreaming sobek is a separate decision.
- The **two esbuild call sites are duplicated** across repos (fork
  `transformSource`, router `transpileForStore`) — same options by convention, not
  a shared helper (they're in different modules). A golden output-stability test on
  the router side guards against drift; keep them in sync if you change the target.
