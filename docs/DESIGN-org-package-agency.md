# Design — org package agency

**Status:** IN PROGRESS. Proposed 2026-07-30; §7 step 1 (the `pkgbuild`
extraction + `RecipeHash` with the cross-repo golden) landed 2026-07-30 —
see `tinycld.org/core/pkgbuild` and `internal/recipeparity/` here. Steps 2–5
are not started.
**Motivates:** letting a tenant's own admin manage that org's packages —
install, uninstall, upgrade, including third-party packages the operator has
never heard of — while a new org can still be spun up from a default set in
seconds.
**Supersedes (in part):** `SCOPE-tenant-feature-go.md`'s option (b) — the
pinned tenant feature menu — and the "hosted third-party packages are
TS-only" trade recorded there. Both were correct for a shared prebuilt tenant
binary; this design removes that premise. The pinned menu stays until this
ships.

---

## 1. Goals

1. **Fast spin-up.** A new org instantiates from a default package list the
   router owns. Seconds, not minutes.
2. **Full tenant freedom.** An org's admin can customize their package set
   with complete autonomy — versions, uninstalls, and third-party packages
   unknown to the operator — with the same UX the single-tenant app already
   has.
3. **One package system.** Today there are two (§2). The end state must not
   have a "hosted package format" that is a second, weaker thing packages must
   also target.

## 2. Where things stand: two package systems

**World A — the single-tenant app** (`tinycld/core/server/coreserver/
rebuild*.go`, `pkg_*.go`). Install is a *build*: `npm pack` fetch →
out-of-tree workspace assemble → `pnpm install` → `go build` → `expo export`
→ DB backup → symlink activate → exit-75 relaunch, with `entrypoint.sh` as
the supervisor that health-checks and promotes or reverts. Source of truth is
the `pkg_registry` collection; third-party npm/git specs are first-class; the
peerVersions solver is enforced authoritatively server-side
(`pkg_compat.go`), including the post-assemble on-disk re-check
(`verifyTargetPeerVersions`). Trust model, stated in code
(`rebuild_pipeline.go`): installing a package is running its author's code
on the host.

**World B — this router.** Install is a *materialization*: a global,
immutable, superuser-publish-only store (`<root>/packages/<name>/<version>/`),
a flat `{name: version}` lockfile on the org row, symlink farms
(`internal/materialize`), evict-to-restart. There is no default set
(`CreateOrg` takes the lockfile as an argument; absent means zero packages),
no tenant-driven mutation path (the package endpoints are deliberately
host-only in `coreserver.RegisterTenant` — "a self-restarting tenant escapes
supervision"), no publisher tooling at all, and no client/static story for
tenants. Feature Go is a hand-pinned menu (`internal/tenantpkgs`) compiled
into one shared `serve-org` binary; packages off the menu are TS/rules-only.

The stated goals land exactly in the seam: World B has the supervision and
isolation, World A has the package agency, and neither has both.

## 3. The decision chain

Each decision below was reached by knocking down the previous design's
weakest assumption. Recording the chain matters more than the endpoint,
because several earlier shapes looked shippable until one question broke
them (§8 records them as rejected alternatives).

### D1 — The tenant proposes; the router supervises

Package-change *authority* belongs to the org admin; package-change
*execution* (restart, revert) stays with the router. The reason the tenant
package endpoints were made host-only remains valid — a self-restarting
tenant escapes supervision — but it only requires the router to own the
restart, not the whole workflow.

The tenant runs the existing Packages UI and pipeline against its own state,
then **proposes** a deploy to the router and waits to be restarted. The
channel is a **per-org control socket** the router binds in the org's run
dir at spawn time: only that org's uid can reach it, so identity is by
construction — no tokens, no control-plane user database, no public-path
exposure through the reverse proxy. Authorization inside the tenant is the
same owner/admin check single-tenant uses; a compromised tenant can only
ever propose changes to *itself*, which is precisely the authority being
delegated.

### D2 — Per-org builds: the tenant binary and client bundle are NOT shared

`SCOPE-tenant-feature-go.md` rejected per-package-set builds because the
build would have run on the router's serving path. D1 inverts that premise:
the build runs off the serving path while the org keeps serving its old
build. With that objection gone, per-org builds are strictly better:

- **The pinned menu dies.** A binary built from the org's own package set
  has no composition gap by construction — it links exactly the org's
  packages via the same generator-emitted wiring the app uses.
  Third-party **Go becomes first-class for hosted orgs.**
- **The client problem evaporates.** The org's `expo export` contains its
  exact package set. No fat-bundle-with-runtime-gating trick, no
  second-class "standalone web surface" tier for third-party client UI.
- **One package format.** A hosted package is an npm/git package, full stop.
  The store/transpile/materialize format stops being a parallel target.

### D3 — A trusted builder, because artifacts shared by hash must be trustworthy

Common package lists will repeat across orgs, so build artifacts should be
shared by hash. That requirement decides *who builds*:

- An artifact built inside org A's confinement is attacker-controlled
  output. Org B must never execute it.
- Honesty cannot be verified by hashing outputs: the only way to know the
  correct hash is to rebuild, and Metro/Expo output is not bit-reproducible
  in practice, so verification collapses into doing the build yourself.

Therefore builds run in a **trusted builder** the router owns — confined
per job (uid/namespace/cgroup via the existing spawner machinery, no
secrets, no org data), because `pnpm` lifecycle scripts and Metro execute
package-author code. This also deletes two costs of pure org-side builds:
tenants need no toolchain in their mount namespace, and no org carries a
workspace, `node_modules`, or Go caches — those live once, in the builder.

**Blast radius of a shared artifact is sound:** a malicious package can only
taint artifacts whose recipe includes it, and every org resolving that
recipe chose to run that package in-process anyway. The cache adds no
cross-org escalation the package didn't already have through the org's own
choice.

### D4 — Content-addressed artifact cache; the default set is just a warm entry

- **Recipe hash** = sha256 over the sorted resolved member set
  (`name@version` + tarball integrity for every member, third-party
  included) plus toolchain versions (go/node/pnpm). Defined in exactly one
  place (D5). Two orgs with the same list hit the same key by construction.
- **Artifact store**: `<root>/builds/<recipe-hash>/` holds only the runtime
  tree — server binary, `pb_hooks`, `pb_migrations`, `pb_public`, OTA
  bundles (~100–200 MB). Orgs hold a pointer, bind-mounted read-only exactly
  like today's package store.
- **The default set needs no special machinery**: it is the cache entry for
  the default lockfile, built once. Provisioning = point a new org at it +
  migrate an empty DB. Any *popular* customization gets the same treatment
  for free: the second org to choose a set pays nothing.
- **Second-level dedup** (optional, later): store output files in a flat CAS
  by content hash and hardlink into build trees, so near-identical sets
  share most of `pb_public`.
- **GC by refcount**: a build is live while any org's lockfile resolves to
  it, plus each org's previous build (revert target). Sweep the rest after a
  grace period. This finally gives the store a principled deletion story;
  today it has none.

### D5 — The build pipeline is one shared Go package

The composition-gap finding's lesson applies to the build pipeline before a
second copy of it exists: one shared implementation, host-specific tails,
and a parity test. Extraction is feasible because World A's orchestrator is
already injectable (`rebuildDeps` — the seam `verifyTargetPeerVersions`'s
wiring test uses).

**`core/server/pkgbuild`** (new; imported by `coreserver` and by the
multi-org builder):

- spec validation, fetch (`npm pack` + untar), manifest parse/validate
  (from `pkg_validate.go`);
- the peerVersions solver + post-assemble on-disk re-check
  (from `pkg_compat.go`);
- workspace assemble: scaffold, `package-versions.json` → pnpm-overrides
  transcription, member placement (from `rebuild_assemble.go`);
- the pipeline steps: `pnpm install`, `go build`, `expo export` web/native,
  stage-runtime-tree (from `rebuild_pipeline.go`);
- **`RecipeHash(...)`** — the cache key's single definition.

Host-specific tails:

- **coreserver (single-tenant):** member set from `pkg_registry`,
  copy-unchanged-members-from-current, DB backup/recover, symlink activate,
  `commitRegistry`, SSE + `pkg_install_log`, exit-75 restart.
- **multi-org builder:** job intake, per-job confinement wrapper, CAS write,
  refcount/GC. No DB, no activation — it only produces artifacts.

Seams: `MemberSource` (fetch vs copy-from-current), `ProgressSink`
(SSE/install-log vs job log), sandbox wrapper (in-process exec vs jailed
child).

**The library is a driver, not the toolchain.** The pipeline delegates the
real work to the *fetched workspace's own* scripts (postinstall runs the
build dir's generator, not the host's) — already true in World A. Keep it
deliberate: it is what lets one builder build workspaces pinned to
different core versions.

**Enforcement:** a golden test asserting both hosts compute the same recipe
hash for the same inputs — same spirit as the paired esbuild goldens and
`composition_parity_test.go`.

**Build target:** prefer a **dual-mode binary** — one `main` that runs as
host (`coreserver.Register`) or tenant (`coreserver.RegisterTenant`,
`--socket`, `--ready-fd`) by flag — over a separate generated tenant main.
One artifact then serves a self-hosted single-tenant deployment *and* a
hosted tenant; the recipe hash needs no target dimension; the existing
composition-parity tests already police the two modes. `serve-org`'s
current main is mostly transport + confinement glue, which folds in.

### D6 — The deploy protocol (restart + revert)

The lifecycle is World A's, re-hosted: downs in the old process, snapshot
before mutation, ups on the post-swap boot, restore on failed activation.
The router plays the role `entrypoint.sh` plays today.

1. Tenant admin drives the existing Packages UI: fetch spec metadata,
   advisory compat solve, drop-report + typed-slug confirm for downgrades
   (`dryRevertNamedMigrations` runs in-tenant — the running process has the
   outgoing migrations registered, since jsvm loaded them at boot).
2. Tenant snapshots its own DB (`VACUUM INTO <orgDir>/.deploy/backup.db` —
   the in-tenant `backupDB`), runs any down migrations via
   `revertNamedMigrations` (aux+main transaction nesting: a failed down
   aborts atomically), then sends `{next lockfile}` over the control socket
   and waits to be killed.
3. Router: authoritative `CheckPeerVersions` on `next` → compute recipe
   hash → cache hit or enqueue build (singleflight per hash,
   concurrency-capped) → record proposed lockfile → evict → repoint the
   org's build reference → spawn.
4. Readiness OK → commit the org row, drop the snapshot. The new boot's
   `RunAllMigrations` applies the ups — World A's "the freshly-built binary
   applies them on its post-swap boot", unchanged.
5. Readiness fails → the child's reason travels back through the existing
   pipe → router restores `backup.db`, repoints the previous build, spawns,
   and writes the failure into `.runtime/deploy-result.json` for the
   recovered tenant to surface (the hosted analog of the durable
   `pkg_install_log` row that survives exit-75). Because the snapshot
   precedes the downs, every failure point — down error, peer refusal,
   build failure, boot failure — converges on the same restore path.

Migration attribution needs no owner table here: the router can ask which
files each resolved version contributes, and the delta between two resolved
sets is the down/up list. (World A needs `pb_migrations_owner.json` only
because its history interleaves in one mutable workspace.)

Per-org deploys are serialized (mutex) and rate-limited — tenant-triggered
churn is now possible and the load singleflight does not cover it.

### D7 — Default set: a control-plane template

A `default_lockfile` JSON on a control-plane settings record, editable from
the admin surface. `CreateOrg` with no explicit lockfile copies it. Editing
the template affects **new** orgs only — an org owns its lockfile from the
moment it is copied, which is the fork semantics we want. The append-only
`deployments` table already provides per-org history.

## 4. Security consequences

- **The tenant process still contains untrusted code** — not because the
  org admin can tamper with the binary (they can't; the builder is trusted
  and core's guards + quota hooks are always compiled in), but because
  third-party package Go the org chose runs in-process with full process
  privileges. The router↔child contract (readiness-pipe parsing, socket
  lifecycle, drain behavior) must be audited under hostile-child
  assumptions. The env-allowlist and namespace-before-exec work already
  point the right way.
- **Quota**: app-layer enforcement survives (trusted builder ⇒ the org
  admin cannot strip `core/quota`, preserving "can't raise the plan they
  were sold" — the reason quota.json is host-materialized today). Add
  **kernel filesystem quotas** on the org's uid/orgDir as the hard backstop
  against hostile package Go bypassing `app.Save`. This is the same lesson
  the isolation work already recorded: the boundary is the OS.
- **Builder confinement**: per-job uid/namespace/cgroup, no secrets, no org
  data, allowlist env — reusing the spawner. Build jobs execute
  package-author code by design (pnpm lifecycle scripts, Metro).
- **Migrations become append-only for hosted orgs.** "Migrations may be
  edited in place; deployments are provisioned fresh" stops holding the
  moment tenants upgrade over time — an edited file with the same name
  silently never re-applies to a DB that already ran it. Once this ships, a
  released package version's migrations are frozen. This changes developer
  workflow and must land in CLAUDE.md / CONTRIBUTING when it does.

## 5. What this deletes or demotes

| Today | After |
|---|---|
| `internal/tenantpkgs` pinned menu + go.mod feature replaces | deleted — the org's binary links its own set |
| shared `serve-org` binary | per-recipe artifact; `serve-org` main folds into the dual-mode binary |
| `internal/store` (package store) + `internal/materialize` symlink farms | demoted to the build-artifact cache (`builds/<hash>/`, bind-mounted read-only); per-file CAS optional later |
| `transpileForStore` / publish-time manifest eval / `POST /api/store/packages` | deleted — packages are fetched and validated by `pkgbuild` in the builder; the paired esbuild goldens with the fork lose the router-side member |
| `.runtime/{carddav,caldav,webdav}.json` materialized DAV config | candidate for deletion — a per-org build always contains exactly the org's features, so tenant mode can mount DAV from feature registration like the host does (port listeners stay router-owned via injected sockets). Decide during implementation, not by drift. |
| `.runtime/quota.json`, `packages.json`, `app.json` | quota stays host-authoritative; `packages.json` becomes redundant with the built-in set (the artifact *is* the gate) |
| `orgs.lockfile` flat map | stays, plus per-entry integrity/provenance and the org's current recipe hash |

The mail router (SNI demux, per-org mail sockets), confinement, admission,
idle sweep, and the readiness protocol are untouched.

## 6. What stays open (named, not hidden)

- **Router↔tenant ABI.** Orgs will run binaries built from different core
  versions; the socket/flags/ready-fd/`.runtime` contract becomes a
  versioned interface with explicit stability guarantees, not an internal
  detail.
- **Build latency UX.** Novel sets pay minutes; surfaced through the same
  job-progress UI World A has, but the org keeps serving its old build —
  confirm the UI communicates "building, not down".
- **Native OTA per org.** One store app must serve per-org JS bundles;
  runtime-version matching already constrains this in World A — the hosted
  update endpoint needs the same policy per org.
- **Builder capacity.** One host initially (concurrency-capped); the queue
  is the seam if it ever needs to be a fleet.
- **Multiple templates** (plans/tiers): D7 trivially extends to named
  templates if product wants it.
- **Kernel quota mechanism** (project quotas vs per-uid) — pick during
  implementation.

## 7. Sequencing

1. **`pkgbuild` extraction** in core (behind the existing `rebuildDeps`
   seam; World A keeps working unchanged). Recipe-hash golden test.
   **DONE 2026-07-30** — `tinycld/core/server/pkgbuild` (validation, compat
   solver + post-assemble verify, assemble behind `MemberSource`, pipeline as
   a per-instance `Pipeline` struct, native export). Tarball integrity is
   captured at fetch (`members.lock.json`); `RecipeHash`/`RecipeHashForBuild`
   + `DetectToolchain` are the single key definition, enforced cross-repo by
   the paired goldens (`pkgbuild/recipehash_test.go` ↔
   `internal/recipeparity/recipehash_parity_test.go`). The single-tenant
   verify step logs the hash as a breadcrumb. Note for step 2: a FromCurrent
   member copied from a pre-lock active build has integrity "" and RecipeHash
   refuses — the first fetched build repopulates it; the builder always
   fetches, so this affects only World A's breadcrumb.
2. **Builder worker** in multi-org: job queue, per-job confinement, CAS
   write, refcount/GC. Build the default set through it; verify a tenant
   boots from the artifact (dual-mode binary lands here).
3. **Router deploy protocol**: control socket, snapshot/revert, per-org
   serialization, `deploy-result.json`. `CreateOrg` from
   `default_lockfile` → default artifact.
4. **Tenant-side**: hosted-mode wiring for the existing Packages UI
   (propose instead of exit-75), in-tenant downs against the control-socket
   flow, `pkg_registry` reconciliation from the built-in set.
   **Acceptance test:** the single-tenant install suite
   (`tinycld/tests/install/todo-install.spec.ts`) run against an org
   subdomain — same spec, both hosts, per goal 2's "same UX". Until then,
   `tinycld/tests/install/multiorg-deploy.spec.ts` (runner
   `run-multiorg-deploy.sh`) covers the hosted flow that exists today —
   superuser publish → create org → deploy → respawned tenant serves the
   new version — over real HTTP, the only over-the-wire coverage of the
   provisioning routes.
5. **Deletions** (§5) + hostile-child audit + kernel quotas + docs
   (append-only migration rule).

Each step lands green on its own; the pinned menu and materialize path keep
working until step 5 removes them.

## 8. Rejected alternatives — and the question that broke each

1. **Shared prebuilt tenant binary + "fat" client bundle + runtime gating**
   (official packages toggled per-org without builds; third-party packages
   as a TS-only hosted format with standalone web surfaces). Broke on:
   *why does the binary/bundle have to be shared at all?* It doesn't — the
   premise came from builds running on the router's serving path, which D1
   had already removed. Permanently second-classed third-party packages.
2. **Router-executed installs with delegated auth** (router claims
   `/api/org-admin/packages/*` on org subdomains, authenticates by
   subrequest to the tenant, executes lockfile edits via `Deploy`). Broke
   on: duplicated World A's entire pipeline router-side, needed a
   proxy-path guard + auth relay, and put third-party fetch/eval on the
   host instead of inside a confinement built for exactly that.
3. **Org-side builds** (tenant runs the full rebuild pipeline itself,
   proposes only the restart). Broke on: *shared-by-hash artifacts.*
   Org-built output is attacker-controlled and unverifiable
   (Metro is not reproducible), so nothing could ever be shared; every org
   would carry a toolchain, a workspace, and multi-GB caches; and in-tenant
   quota enforcement would have become voluntary (org admin compiles the
   binary), forcing all plan enforcement to the kernel.
4. **Provenance-keyed shared store for third-party ingestion**
   (router fetches specs on demand into the global store, keys non-npm
   provenance by commit sha to prevent cross-org poisoning). Subsumed:
   with a trusted builder and per-org artifacts, third-party bytes never
   enter a shared package namespace at all — the poisoning problem is
   dissolved rather than mitigated, and the store's successor (the build
   cache) shares only *outputs* keyed by full-recipe inputs.
5. **Soft uninstall** (leave migrations applied, gate access only). Was a
   hedge against down-migrations being impossible in tenants; they aren't —
   `revertNamedMigrations` needs only the outgoing files to be registered
   in the running process, which the old tenant satisfies by construction.
   Real uninstall/downgrade ship in v1 (D6).
