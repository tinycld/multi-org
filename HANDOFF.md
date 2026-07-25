# Handoff — Multi-Org PocketBase Router

**Updated:** 2026-07-25 (was 2026-07-24)
**Goal:** Make one PocketBase process host many organizations — each org its own
SQLite DB, client JS bundle, and server-side JS handlers — sharing versioned code
where identical, isolated where not.

> **2026-07-25:** **The §11 "no feature Go" decision was REVERSED — packages ship
> Go again.** The contacts pilot was re-Go'd: its CardDAV/FTS/audit now run in the
> package's own Go server, driven by config it builds and passes to core's reusable
> `carddav`/`fts` libraries (single copy — no duplication). The multi-org router
> keeps serving CardDAV **host-side** for stock-PB tenants (they still have no
> feature Go), importing the same `tinycld.org/core/carddav`. The tail of Track-B
> de-org was finished so the contacts app + e2e run green. See **§13**.

> **2026-07-24:** **Track B (tinycld de-org-ing) is done** for core + app shell +
> all TS client + core Go server + the contacts feature — the tinycld codebase now
> assumes a single org (the router owns multiplexing). See **§12**. Feature Go
> servers (mail/drive/calendar/text/calc) remain, blocked on §11 fork adoption.

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

Six bodies of work:

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
5. **Tenant JS sandbox** (§10) — in-process capability hardening for hostile tenant
   authors (jsvm `Sandboxed` mode). Shipped as defense-in-depth, but a demonstrated
   `$app.db()` `ATTACH` cross-org exploit proves in-process allowlisting is not
   containment — **OS-level per-process isolation is now the required boundary** (§5).
6. **Host-side capabilities + multi-org CardDAV** (§11) — the Go-into-core /
   config-driven-capability model: packages contribute protocol servers (CardDAV),
   FTS, and audit as **manifest config**, and core runs them host-side (no feature
   Go in tenants). Pilot: contacts de-Go'd; CardDAV served per-org in BOTH the
   single-tenant app (`sharedDBScope`) and multi-org tenants (`singleOrgScope`),
   the latter wired through `orgmanager` over each tenant's app. Closed the
   control-plane-leak (§5 #2) along the way. Spans the tinycld repo (core
   capabilities) and this router. All green.
7. **Track B — tinycld de-org-ing (§12, done 2026-07-24).** `orgs`/`user_org`
   collections DELETED from the tinycld app; `role` moved onto `users`; every
   feature/core FK repointed `user_org → users`; `useOrgLiveQuery` scope collapsed
   to `{ userId }`; RLS rewritten to `@request.auth.role`; `app/a/[orgSlug]/` route
   segment collapsed to `app/(app)/`; the two admin surfaces merged (`/admin` =
   in-shell console, `/setup` = bootstrap door). Verified end-to-end against a fresh
   boot of the forked server (member/guest RLS, contacts owner-isolation). Committed
   on `feat/de-org` across 8 repos. **Now single-org: the router owns org
   multiplexing; core assumes one org = one DB.** The `sharedDBScope` mentioned in
   item 6 above is gone — carddav is `singleOrgScope`-only now.

Design spec + implementation plans:
- Fork seams + router design (2026-07-22) live in the fork's git history at
  `docs/superpowers/{specs,plans}/2026-07-22-*.md` (on the branches that committed
  them; not in the `feat/multitenant-fork` working tree).
- Operator-runnable plan: `~/.claude/plans/streamed-orbiting-pretzel.md`.
- **TypeScript** spec + plan (committed in this repo):
  `docs/superpowers/specs/2026-07-23-typescript-hooks-design.md` and
  `docs/superpowers/plans/2026-07-23-typescript-hooks.md`.
- **Multi-org CardDAV** spec (committed in this repo):
  `docs/superpowers/specs/2026-07-23-carddav-multi-org.md`. Core capability-hook
  reference lives in the tinycld repo at `tinycld/docs/hooks.md`.

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
2. ~~**Control-plane collections leak into every tenant.**~~ **CLOSED
   (2026-07-23, §11).** `orgs`/`packages`/`deployments` were registered into the
   process-global `core.AppMigrations`, so every tenant `RunAllMigrations()` also
   created them — a tenant having an `orgs` table (the registry of every other org)
   contradicts the isolation model, and it collided with any package wanting those
   collection names. Fixed by making the control-plane schema **app-scoped**:
   `controlplane.RunSchema(app)` runs a local `MigrationsList` against the
   control-plane app only (via `NewMigrationsRunner`), and `ControlPlane.Init()`
   sequences Bootstrap → RunSystemMigrations → RunSchema. `core.AppMigrations` is no
   longer touched, so tenants never inherit the registry. Guarded by
   `TestIntegration_TenantHasNoControlPlaneCollections` (asserts a provisioned
   tenant DB has none of the three, while the control-plane still has all three).
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

~~**The Track-B follow-on — tinycld de-org-ing (spec §11):** remove `orgs`/`user_org`
collections + FKs from the tinycld app, collapse `useOrgLiveQuery`/`OrgScope` to
auth-based rules, org switcher via the parent-domain hint cookie.~~ **DONE
2026-07-24 (§12)** for core + app shell + all TS client + core Go server + the
contacts feature. The org switcher (parent-domain cookie) is stubbed pending the
router setting the cookie. **Remaining:** the 5 feature Go servers
(mail/drive/calendar/text/calc) still contain org/user_org logic AND are blocked on
§11 fork adoption (they lack the `replace => ../../pocketbase` sobek fork replace,
so they can't build against the forked core). See §12 "Remaining".

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
- ~~**Close finding #2 (tenant schema leak)** from §5.~~ **DONE (§11)** — the
  control-plane schema is app-scoped now. (Finding #1, the publish route, closed in
  §9.)
- **Commit the §11 CardDAV/capabilities work** across the three repos (`multi-org`,
  tinycld core+shell, `contacts`) — currently uncommitted working-tree changes.
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
| `~/code/tinycld/pocketbase` | `feat/multitenant-fork` | `fb868ca4` | No — local integration (checked out); +8 TS/sobek commits, **+8 jsvm Sandboxed-mode commits (§10)** since `0da4c670` |
| `~/code/tinycld/multi-org` | `feat/operator-runnable` | `1694c1e` | **No remote**; **+ jsvm-sandbox wiring + docs commits (§10)** since `da08173`; **+ UNCOMMITTED CardDAV/capabilities + control-plane-leak fix (§11)** in the working tree |
| `~/code/tinycld` (core+shell) | **`feat/de-org`** | `1f6691f` | No; §11 committed as `6f8ead3`, then **Track-B de-org** stacked in 3 commits (`a4d577a` schema → `aed30e8` client → `1f6691f` server+app). Branched off `chore/bump-pocketbase-v0.39.8` (`8fff4e4`). |
| `~/code/tinycld/contacts` | **`feat/de-org`** | `9ccd0ac` | No; §11 de-Go committed (`ea752ad`), then de-org schema/client + the `contacts.pb.ts` inline-uuid fix (§12) |
| `mail/calendar/drive/text/calc` | **`feat/de-org`** | (each) | No; **client** de-org committed; **feature Go servers NOT de-orged** (see §12 Remaining) |
| `~/code/tinycld/google-takeout-import` | **`feat/de-org`** | `1781eb0` | No; client de-org committed |

⚠️ **The §11 CardDAV work is now COMMITTED** (`6f8ead3` in the tinycld repo;
contacts `ea752ad`) — the Track-B de-org branch (`feat/de-org`) is stacked on top of
it. The router imports `tinycld.org/core` via a `replace => ../tinycld/core/server`
in `multi-org/go.mod`, so a router build needs the tinycld core working tree present
with its capability packages.

⚠️ **Workspace is currently running LEAN (core + contacts only).** The other 6
feature dirs (`mail calendar drive text calc google-takeout-import`) plus the two
test stubs (`share-stub`, `shortcut-stub`) are PARKED at `~/code/tinycld/.parked/`
and removed from `pnpm-workspace.yaml`, so `getPackages()` doesn't scan them and the
app boots as a lean core+contacts shell. To restore the full workspace: move the
parked dirs back to the workspace root, re-add them to `pnpm-workspace.yaml`, then
`pnpm install`. Their git repos (all on `feat/de-org`) travel with the dirs.

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

# Multi-org CardDAV multiplex + control-plane-leak fix (§11):
go test ./internal/controlplane/ -run TestIntegration_MultiOrgCardDAV -v
go test ./internal/controlplane/ -run TestIntegration_TenantHasNoControlPlaneCollections -v
# Core capability packages (tinycld repo, standalone against the fork):
cd ~/code/tinycld/tinycld/core/server && GOWORK=off go test ./carddav/ ./fts/ ./audit/ ./coreserver/

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

# --- Track B — de-org (§12), on feat/de-org across the tinycld repos ---
# Core TS (typecheck + biome + vitest) and app-shell checks:
cd ~/code/tinycld/tinycld/core && pnpm exec tinycld-pkg check
cd ~/code/tinycld/tinycld && pnpm run checks
# Contacts (re-linked feature):
cd ~/code/tinycld/contacts && pnpm exec tinycld-pkg check
# Core Go server on the fork (all suites; rewritten to single-org fixtures):
cd ~/code/tinycld/tinycld/core/server && GOWORK=off go build ./... && GOWORK=off go vet ./... && GOWORK=off go test ./...
# Live single-org schema + RLS (fresh boot of the app-shell server binary):
#   Build: cd ~/code/tinycld/tinycld/server && go build -o /tmp/tinycld-server .
#   /tmp/tinycld-server superuser create admin@x.test 'Passw0rd123!' --dir /tmp/pbv
#   /tmp/tinycld-server serve --dir /tmp/pbv \
#     --migrationsDir ~/code/tinycld/tinycld/server/pb_migrations \
#     --hooksDir ~/code/tinycld/tinycld/server/pb_hooks --http 127.0.0.1:8899
#   Assert: sqlite3 /tmp/pbv/data.db has NO orgs/user_org, users has a `role` field;
#   a member lists all users, a guest sees only itself and 0 labels; a member's
#   contact (owner = their user id) is invisible to another user.
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

---

## 10. Tenant JS sandbox — in-process hardening (done 2026-07-23) + isolation escalation

Hardened the untrusted tenant server-side JS boundary. Trust model: **fully hostile**
tenant authors sharing one process. Spec + plan (this repo):
`docs/superpowers/specs/2026-07-23-tenant-jsvm-sandbox-design.md`,
`docs/superpowers/plans/2026-07-23-tenant-jsvm-sandbox.md`.

### Shipped (defense-in-depth; stands, but is NOT containment)

Fork `feat/multitenant-fork` (`614189f1..fb868ca4`) added a nil-default
`jsvm.Config{Sandboxed}` mode; router `feat/operator-runnable`
(`fc2efb4`, `3c782d9`, `b783697`) turns it on for tenant code:

- **Deny-by-default binding allowlist** for both hook and migration runtimes:
  withholds `$os` (exec/env/raw FS), `$http` (outbound), `$filesystem`, `$filepath`;
  scrubs `process.env`/`process.argv`; **withholds `$apis.static`**; **denies
  file-based `require()`** (native modules only); **restricts `$template` to
  `loadString`**.
- **Enabled at both untrusted-code call sites** — runtime `orgmanager.load` and
  provision-time `controlplane.bootstrapTenantOnce` — each via `jsvm.Register`
  (returns the error) not `MustRegister`, so a **load-time throw fails closed** for
  that one org instead of panicking the shared process (a DoS otherwise; singleflight
  would poison every caller of that org).
- All TDD'd; fork + router suites green incl. `-race`; cross-org program sharing and
  benign JS/TS provisioning still pass (§8 proof tests).

### The escalation (why in-process tops out here)

A final adversarial review demonstrated a **live cross-org exploit** the allowlist
cannot close: `$app.db()` exposes raw SQL, and a sandboxed hook running
`ATTACH DATABASE '<other-org>/data.db'` reads another org's secrets (and writes
arbitrary `.db` files). modernc SQLite has **no authorizer API** to contain it
in-process. This was the 4th capability an audit found re-entering through a *kept*
surface (`$apis.static` → `require` → `$template` → `$app.db()`). **Conclusion:
allowlisting the full stock `$app`/DB API against a hostile author in one shared
process is the wrong altitude.**

**OS-level per-process isolation is now the required next boundary** (§5 finding #3).
Follow-up brief: `docs/superpowers/specs/FOLLOWUP-os-process-isolation.md`.
**Until it lands, do not treat tenant authors as untrusted in production.**

---

## 11. Host-side capabilities + multi-org CardDAV (done 2026-07-23)

The model: functionality that must be Go (protocol servers, FTS5, OOXML, …) lives
in **core** and runs **host-side**; packages contribute it as **manifest config**,
not feature Go. A tenant stays stock PocketBase + JS/TS hooks. Spec:
`docs/superpowers/specs/2026-07-23-carddav-multi-org.md`; capability-hook reference:
`tinycld/docs/hooks.md` (tinycld repo). **Uncommitted** across `multi-org`, tinycld
core+shell, and `contacts` (see §7).

### In the tinycld repo (`~/code/tinycld/tinycld`)

- **Fork adoption**: `server/go.mod` + `core/server/go.mod` gain
  `replace github.com/pocketbase/pocketbase => …/pocketbase`, so the single-tenant
  app links the fork jsvm (sobek + `OnInit`). Verified: full core + all 6 feature
  Go suites green on the fork.
- **`OnInit` `$`-binding seam** — `core/server/coreserver/jsvm_binds.go`:
  `RegisterJSVMBinder`/`buildJsvmOnInit`/`NewBindNamespace`. Minimal by design; the
  pilot needs **zero** custom bindings (data-plane work is core Go record hooks
  driven by config, not bindings — keeps the untrusted-TS surface tiny).
- **Core capability packages**: `core/server/{carddav,fts,audit}/`, all
  config-driven and wired from `coreserver/pkg_capabilities.go` (reads each
  package's manifest block from `bundled-packages.json`; **fails loud** on a
  malformed block). CardDAV uses an `OrgScope` interface: `sharedDBScope`
  (single-tenant, org-by-slug across one DB) vs `singleOrgScope` (multi-org, the DB
  IS the org). FTS5 sync/search generalized from contacts; audit moved off the
  `audit.RegisterCollection` Go call to config.
- **Manifest**: `carddav`/`fts`/`audit` block types added to
  `scripts/load-manifest.ts`. **contacts de-Go'd**: `contacts/server/` deleted; a
  single `contacts/pb-hooks/contacts.pb.ts` (vcard_uid autogen) remains; the
  manifest declares `hooks`/`carddav`/`fts`/`audit`. The generator drops contacts
  from the Go wiring and symlinks its `pb-hooks` into the app's `pb_hooks`.

### In this router (`~/code/tinycld/multi-org`)

- **Imports `tinycld.org/core`** (`replace => ../tinycld/core/server`).
- **`orgmanager.load` composes CardDAV** over each tenant's app: `composeMux`
  prefix-routes `/carddav*` to `carddav.HandlerFor(inst.app, sources)`
  (`singleOrgScope`), else the stock mux. `inst.App()` accessor added.
- **Manifest reaches the host**: `PublishPackage` now emits a parsed `manifest.json`
  into the store (`controlplane/manifest.go`: transpile `manifest.ts` via esbuild →
  eval in a throwaway sobek VM → JSON), and `controlplane.CardDAVSources` reads the
  `carddav` block from it, fed to `orgmanager.Config.CardDAVSources`.
- **`materialize`** now links `pb-hooks/*` into the tenant `pb_hooks` (the tinycld
  convention), alongside the legacy `server/` link.
- **Control-plane-leak fix (§5 #2, CLOSED)**: control-plane schema is now
  **app-scoped** (`controlplane.RunSchema` / `ControlPlane.Init`), off the
  process-global `core.AppMigrations`, so tenants no longer inherit
  `orgs`/`packages`/`deployments`.

### Proven

- Core: `carddav` (codec, path helpers, both scopes DB-backed), `fts`
  (sanitize/coerce), `audit` (descriptor translation), binding seam — all green;
  single-tenant `/carddav` served a live 401 challenge from config.
- Router: `TestIntegration_MultiOrgCardDAV` — publish contacts → provision **acme +
  globex** → each serves **only its own** contact via `inst.Mux()` (the multiplex
  proof: acme has Alice not Bob, globex the reverse); `…_Challenges401` (route
  mounted from config, not 404); `TestIntegration_TenantHasNoControlPlaneCollections`
  (leak fix). `emitManifestJSON` + `CardDAVSources` unit-tested. Full router suite
  green + vet clean.

### Multi-org CardDAV model (decided)

**Per-org, org-from-hostname.** `acme.<domain>/carddav` authenticates against
acme's tenant `users` and serves acme's contacts; a user in K orgs configures K
CardDAV accounts. **Nested/aggregated** ("one login → a book per org") was rejected:
it needs a cross-org identity that doesn't exist — the control-plane authenticates
only the operator superuser and holds no end-user/membership data; end-users
authenticate per-tenant. A global-identity + membership + tenant-SSO feature
(overlaps Track-B de-org-ing) is a prerequisite and out of scope.

### Follow-ons

- **Other protocols** (CalDAV/CardDAV-analog, WebDAV, IMAP/SMTP) follow the same
  host-Go-over-tenant-app shape; IMAP/SMTP add stateful long-lived sessions and one
  host listener (SNI selects the org at connect, TLS terminated by the host).
- **Post-isolation**: once orgs are isolated processes, the host can't read
  `inst.app` directly — CardDAV/IMAP then **proxy** to per-org backends. The
  `OrgScope`/data-access seam is where that swap lands without touching protocol
  code.

---

## 12. Track B — tinycld de-org-ing (done 2026-07-24)

The app-facing counterpart to the router: with the router owning org multiplexing,
**core now assumes a single org (one org = one PocketBase DB)**. All multi-org
concepts were removed from the tinycld codebase. Plan:
`~/.claude/plans/rustling-popping-babbage.md`. Committed on `feat/de-org` across all
8 repos (see §7 for hashes).

### Decisions (locked with the user)

- `orgs` + `user_org` collections **deleted**. The `role` enum
  (`owner`/`admin`/`member`/`guest`) moved onto the `users` auth record. Every
  feature/core FK that pointed at `user_org` was **repointed to `users`** (value is
  now a users id): `contacts.owner`, `calendar_members.user`,
  `calendar_events.created_by`, `drive_items.created_by`, `drive_shares.user_org`
  (name kept), `mail_*.user_org` (name kept), core `labels.user` /
  `label_assignments.user` / `org_pkg_access.user`, `comment_mentions.mentioned_user_org`.
- Direct `org` FK fields **dropped** everywhere + their composite indexes rebuilt.
- **No backwards compat / no data preservation**: migrations edited IN PLACE (fresh
  DBs re-migrate; no add→backfill→drop). Org-only patch migrations deleted.
- Roles + member management **survive** (invites, members list, package access,
  role-gated UI) — only cross-org concerns (switcher, org-create console, cross-org
  impersonation, org-slug routing) go away.
- Route segment `app/a/[orgSlug]/` **collapsed to `app/(app)/`** (host identifies the
  org). `useOrgHref`/`useOrgSlug` kept as slug-free shims to avoid churning ~200 nav
  sites.
- **The two admin surfaces merged**: `/admin` = the single in-shell console
  (packages/builds/orgs/super-admins); `/setup` (was top-level `/admin`) = the
  pre-auth bootstrap door only (first-run `?token=` wizard + `_superusers` recovery).
  The Go first-run URL now prints `/setup?token=`.

### What shipped

- **Schema** (all repos): migrations edited in place; RLS rewritten from
  `org.user_org_via_org…` predicates to `@request.auth.id`/`@request.auth.role`
  (verified: once `role` is a real `users` field, `@request.auth.role` resolves in
  the PB rule engine).
- **Client** (core + all 7 features): `useOrgLiveQuery` scope → `{ userId }`;
  `useCurrentRole` reads `users.role`; org-branding hooks stubbed; auth store dropped
  `primaryOrgSlug`/org-expand; `OrganizationsTab` stubbed to an empty state (org list
  is a router-cookie concern, not built); members roster + settings screens
  re-sourced from `users`.
- **Go server (core)**: `carddav` collapsed to `singleOrgScope` (owner = the authed
  user id; `sharedDBScope`/`findUserOrgBySlug` deleted); `userorg` reduced to account
  offboarding (leave-org endpoints removed); invites/demo set `users.role` instead of
  creating junction rows; `notify`/`fts`/`audit` de-org-scoped; cross-org
  impersonation (`org_admin.go`) deleted. All core Go tests rewritten to single-org
  fixtures and passing.

### Verified end-to-end (fresh boot of the forked app-shell server)

- Migrations replay clean into a fresh DB: **no** `orgs`/`user_org`/`org_provisioning`;
  `users.role` = `select[owner,admin,member,guest]`; FKs → `_pb_users_auth_`; no `org`
  fields.
- RLS live: a **member** lists all users; a **guest** sees only itself and gets 0
  labels (`@request.auth.role != "guest"` + self carve-out both hold).
- **Contacts** (re-linked, no Go server): member creates a contact owned by their
  user id → 200; owner-scoped RLS isolates it from other users; the `contacts.pb.ts`
  hook auto-generates `vcard_uid`.
- TS: core typecheck + biome + 477 unit tests green; app-shell checks green; contacts
  check green.

### Remaining

1. **The 5 feature Go servers** (mail/drive/calendar/text/calc) still contain
   org/user_org logic AND are **blocked on §11 fork adoption** — they lack the
   `replace github.com/pocketbase/pocketbase => ../../pocketbase` the app shell has,
   so they can't build against the sobek-forked core (goja↔sobek mismatch). Their
   *client* code is already de-orged. Next: add the fork replace to each, then de-org
   the server (same pattern core used). Contacts (already de-Go'd) is the proven
   template.
2. **§11 TypeScript-hooks transpile bug (found during Track-B verification).** A
   `.pb.ts` hook cannot reference a top-level module binding — a `function` OR a
   `const` arrow, declared before or after the hook — from inside the hook callback:
   it throws `ReferenceError: X is not defined` at request time. The fork's TS→JS
   hook-wrapping seam loses module scope. Worked around in `contacts.pb.ts` by
   inlining the UUID logic into the callback; the fork seam should be fixed so hooks
   can factor out helpers. (Plain `.pb.js` hooks are unaffected — they load without
   the esbuild wrap.)
3. **Org switcher parent-domain cookie**: `OrganizationsTab` is stubbed; wire it to
   the cookie once the router sets it (`.<domain>` cookie listing accessible orgs,
   rows linking to `<slug>.<domain>`).
4. **Restore the full workspace** when the feature servers are ready (see §7 warning).

---

## 13. Packages ship Go again — §11 reversal + carddav de-dup (done 2026-07-25)

The §11 model — "functionality that must be Go lives in core, packages contribute
only config, **no feature Go runs**" — was **reversed**. Packages must keep the
ability to ship their own Go. The contacts pilot was re-Go'd, with core's protocol
code kept as **one shared copy** (not duplicated per consumer). Committed on branch
`multi-org` across three repos (tinycld, contacts, this router).

### Decisions (locked with the user)

- **Packages own their Go** via the manifest `server: { package, module }` +
  `Register(app)` seam (the "Era-1" model, still used by parked mail/drive/…).
- **Core keeps `carddav` + `fts` as generic, config-driven LIBRARIES**, not
  boot-time wiring. A package's Go builds a `carddav.Source` / `fts.Config` and
  calls `carddav.Register` / `fts.Register` / `audit.RegisterCollection`. **Single
  copy** — the router imports the same `tinycld.org/core/carddav`, so there is no
  duplication (an earlier attempt vendored a copy into the router; that was undone
  once the decision was to keep core's copy).
- **Single-tenant only** for package Go for now; running package Go inside multi-org
  *tenants* stays deferred to the OS-process-isolation follow-on (§5 #3). Tenants
  remain stock PocketBase; the router serves CardDAV host-side (`orgmanager.load` →
  `composeMux` → `carddav.HandlerFor(inst.app, sources)`), unchanged.

### In the tinycld repo (`~/code/tinycld/tinycld`, branch `multi-org`)

- `core/server/coreserver`: removed the manifest-config capability wiring
  (`pkg_capabilities.go` deleted; the `fts.Register`/`carddav.Register`/
  `audit.RegisterFromDescriptors` calls dropped from `server.go`). Kept the
  `$`-binding seam and the `/carddav` CORS bypass. `audit`'s config-driven
  `Descriptor` path deleted (imperative `RegisterCollection` stays).
- `core/server/{carddav,fts}` **kept** as the single shared libraries.
- `scripts/gen-server.ts`: `buildMemberGoWork` now emits the fork replace so a
  package server builds standalone against the sobek fork.
- **Track-B de-org tail finished** so the contacts app runs: the seed harness
  (`scripts/seed-db.ts`, `SeedContext`, `reset-demo.ts`) de-org'd (user `role`, no
  `orgs`/`user_org`, dead `seedSecondOrg` removed); the shared e2e helpers
  de-org'd (`login()` gates on the nav rail, bare `/<pkg>` routes); and the
  **post-login React #185 loop** fixed — `app/index.tsx`'s `navigateToOrg()`
  (`router.push('/')`) pushed the current route onto itself under the collapsed
  single-org path, so it's now a declarative `<Redirect>` to the first accessible
  package; `useOrgHref` returns a stable string href.

### In the contacts repo (`~/code/tinycld/contacts`, branch `multi-org`)

- `server/` restored (recovered from `b8d2d2b`, reconciled to single-org): a
  `Register(app)` that builds a contacts `carddav.Source` + `fts.Config`, calls
  core's capabilities, adds the `vcard_uid` create hook, and exposes a
  `$contacts.search` JS binding. `manifest.ts` re-adds `server`, drops the
  `carddav`/`fts`/`audit` config blocks; `contacts.pb.ts` is a TS-extension example.
- `seed.ts` de-org'd (owner/labels by user id).

### In this router (`~/code/tinycld/multi-org`, branch `multi-org`)

- **No source change from the de-dup** beyond confirming it still imports
  `tinycld.org/core/carddav` and that `TestIntegration_MultiOrgCardDAV` is green.
  The router's CardDAV multiplex is unchanged. (The pre-existing §11 working-tree
  changes — `controlplane.go`, `provisioning.go`, `materialize.go`, `manifest.go`,
  `capabilities.go`, `carddav.go`, the integration tests — are committed onto this
  branch alongside.)

### Verified

- Go: contacts server + core (`carddav`/`fts`) + the app-shell binary (with
  `contacts.Register` wired) all build/test; router builds + vets +
  `TestIntegration_MultiOrgCardDAV` green.
- TS: app checks + core (477 tests) + contacts (20 tests) green.
- **Live single-tenant smoke**: vcard_uid autogen, `/api/contacts/search` (incl.
  partial-email), CardDAV 401 → REPORT vCard → soft-delete, and an `audit_logs` row.
- **Contacts Playwright e2e: 5/5 pass** (boots, renders seeded data, search
  clear/restore, partial-email FTS, create-shows-in-list).

### Remaining / follow-ons

- **Package Go inside multi-org tenants** — still deferred to OS-process isolation
  (§5 #3). Until then the router serves protocol Go host-side.
- **The other feature servers** (mail/drive/calendar/calc/text) are still Era-1 and
  blocked on §11 fork adoption (§12 Remaining) — unchanged by this work.
- The `manifest.ts` version mismatch was fixed for contacts (`0.1.2`).
