# Scope — linking feature Go into the tenant process

**Status:** **CLOSED** 2026-07-27 — implemented as D1 + D2 with step 5 resolved
as option (b). See "How it was closed" at the end. **SUPERSEDED 2026-07-31** by
`DESIGN-org-package-agency.md` §7 step 5: the pinned menu (`serve-org` +
`internal/tenantpkgs`) is deleted. Each org now runs its OWN per-recipe artifact
binary — the app shell's dual-mode `main` — which links exactly the org's
package set and registers its feature Go unconditionally (the artifact is the
gate, replacing the runtime slug filter this doc scoped). The composition-gap
concern below is answered by per-org builds rather than a hand-pinned menu. This
document is retained as the record of why option (b) was chosen at the time.
**Motivates:** REMEDIATION-PLAN P1-5 (calendar member authz), and the whole
class of findings shaped "enforcement lives in Go a tenant never runs".

---

> **Read `FINDING-tenant-composition-gap.md` first.** This document scopes
> linking FEATURE Go into tenants. That is not the first problem: CORE's own
> guards are missing from tenants too, which no feature-linking decision
> addresses. The framing below is still valid on its own terms, but it is the
> second question, not the first.

## The problem, stated precisely

`multi-org/cmd/serve-org` links no feature package. Measured, not assumed:

| binary | `packages/calendar` symbols | size |
|---|---|---|
| `tinycld/server` (single-tenant app) | 78 | 66.2 MiB |
| `multi-org/cmd/serve-org` (tenant) | **0** | 47.6 MiB |

The app gets them because the generator (`generate.ts:455`, `emitGoWiring`)
writes `package_extensions.go` into `SERVER_DIR` — which is `APP_DIR/server` —
importing each feature and calling its `Register(app)`. Nothing writes an
equivalent for `serve-org`, and `multi-org` has neither a `go.work` nor any
`replace` for the feature modules, so it could not resolve them today.

**This is build wiring, not an architectural impossibility.** Verified: a
standalone `main` importing `tinycld.org/packages/calendar` and referencing
`calendar.Register` compiles cleanly given the right `replace` directives.
Feature Go is ordinary Go.

The consequence is that any authorization a feature keeps solely in its request
hooks is absent from a tenant. Calendar is the live instance: the owner-only
member create, the self-promote/repoint guard and the last-owner guard are all
in `calendar/server/register.go`, so a tenant serves stock PB REST with a
`@request.auth.id != ""` create rule — any signed-in tenant user can take any
calendar. It is not the only instance, just the one with a proven exploit.

## The rule this does NOT violate

> A service that must **bind a port** moves into core so the multi-org router
> can open the port. Performance-sensitive work stays in Go; TS/JS hooks are
> the customization seam.

CardDAV/CalDAV/WebDAV are core libraries because they *listen*, and the router
owns every listening socket (a tenant serves on a unix socket handed down to
it). Calendar's authorization hooks bind no port. Linking them into the tenant
is orthogonal to that rule.

---

## Design

### D1 — one binary, runtime gating (recommended)

`serve-org` links **every** known feature package and decides at boot which to
register, from the org's resolved package set.

```go
// generated, tenant-side
func registerPackageExtensions(app *pocketbase.PocketBase, enabled map[string]bool) {
    if enabled["calendar"] { calendar.Register(app) }
    if enabled["drive"]    { drive.Register(app) }
    // …
}
```

**Why gate rather than register everything:** an org that has not installed
calendar must not get calendar's hooks, its scheduler goroutines, or its
collections' behaviour. Registration is not free — `Register` binds record
hooks and several features start background goroutines (calendar starts a
subscription poller and a reminder scheduler on `OnServe`).

**Why one binary rather than per-org builds:** a per-package-set binary means
invoking the Go toolchain on the provisioning path, a build cache keyed by
package set, and a rebuild whenever any package version changes. That is a
build service, not a feature. One binary keeps `Deploy` to what it is now.

**Cost:** the tenant binary grows toward the app's size. Today's delta is
~19 MiB (47.6 → 66.2). Every tenant process pays that in RSS, and the
per-process model runs one process per active org — this is the real cost of
D1 and the number to argue about.

### D2 — the package set has to reach the tenant

`SpawnRequest.Slug` is documented as "identification and logging only", and
`serve-org` receives no package list. The host already resolves the lockfile
(`manager.load` → `lf.Resolve`) to materialize hooks and DAV config, so the
data exists host-side; it needs a channel to the child.

Follow the existing pattern rather than inventing one: the host already writes
`<orgDir>/.runtime/{carddav,caldav,webdav,quota}.json` and passes each path as
a flag. Add `.runtime/packages.json` (the resolved slugs) and a
`--packages-config` flag. It inherits the existing materialize/confine story
for free.

### D3 — what happens to the declarative DAV config

Nothing, initially. CardDAV/CalDAV/WebDAV keep being driven by materialized
config, because they still listen and still belong to core. Do **not** fold
this into the same change — that is a second, larger question (whether a
feature's own `Register` should own its DAV mount in a tenant) and mixing them
makes both unreviewable.

### D4 — confinement is unchanged, but must be re-proven

Feature Go would run inside the tenant's existing uid/mount/PID namespace
sandbox, so the OS boundary is identical. But the boundary is now protecting
*more* code, and `TestConfinement_*` should be re-run with features linked
before this is called done. Cheap to check, expensive to assume.

---

## Work breakdown

1. **Module resolution** — `multi-org/go.work` (or `replace` set) covering each
   feature `server/` module + core. Generator-emitted, gitignored, same shape
   as the per-member `go.work` `emitGoWiring` already writes.
2. **Generator** — teach `emitGoWiring` to emit a second extensions file for
   the router, keyed by slug, with the gating signature above. It already has
   `serverPkgs` (slug + module + relpath); this is a second template, not new
   discovery.
3. **`packages.json`** — host writes the resolved slugs; `serve-org` grows
   `--packages-config` and parses it.
4. **`serve-org` wiring** — call `registerPackageExtensions(app, enabled)`
   before `app.Bootstrap()`. Mind ordering against `jsvm.Register`: the app
   server registers features *before* jsvm (see `coreserver/server.go:128`,
   which calls the order load-bearing), so mirror that.
5. **Bootstrap assembly — THE OPEN QUESTION. Resolve before writing code.**

   A tenant binary that links every feature needs every feature's source
   present at build time. But the workspace is assembled per-developer:
   `npx @tinycld/bootstrap --assemble-only --with mail --with contacts` clones
   only what you asked for, and `pnpm-workspace.yaml` lists exactly the members
   that developer chose. "The set of installed features is exactly the set of
   present member dirs containing a manifest.ts" (CLAUDE.md). There is no
   canonical full checkout.

   That leaves three shapes, and they are not equivalent:

   **(a) Generator emits only the present features.** Trivial to build — it is
   what `emitGoWiring` already does for the app. But the tenant binary then
   depends on the assembly that produced it: a router built in a mail-only
   workspace can never host an org that installs calendar, and the failure is
   silent (the org boots, calendar's rules apply, its Go guards are absent —
   exactly today's bug, now harder to see). **This reintroduces per-set builds
   through the back door and I would not ship it.**

   **(b) The router pins a canonical feature set** — a manifest in `multi-org`
   naming the packages a hosted deployment supports, with its own clone/build
   step independent of the developer's workspace. Honest about what a hosted
   product is: a fixed menu. Costs a second assembly path and makes
   "third-party packages are first-class" (CLAUDE.md) untrue for hosted orgs
   with Go — they would be TS/rules-only.

   **(c) Build the tenant binary per package-set on the provisioning path.**
   Keeps arbitrary packages first-class, but puts the Go toolchain in the
   serving path plus a build cache keyed by resolved set. That is a build
   service. Out of scope here; noting it so the option is not lost.

   **(b) is the recommendation** — it matches what the router already is (it
   materializes from a package *store* it controls) and keeps the door open for
   (c) later. But it is a product decision about whether hosted third-party
   packages may ship Go, and that is not mine to make.
6. **Tests** — the tenant configuration is currently untested for feature
   behaviour. `rlstest.ApplyWithHooks` covers migrations+hooks; this needs the
   equivalent for linked Go. At minimum: calendar's owner guard denying a
   takeover in a tenant-shaped app, and a package NOT in the org's set
   contributing nothing.
7. **Re-run `TestConfinement_*`** with features linked (D4).

## Estimate

Steps 1–4 are a day or so of mechanical work. Step 5 is the one that decides
the shape and could invalidate D1 — resolve it first. Step 6 is a day. Call it
**3–4 days** with the open question closed, more if step 5 forces per-set
builds.

## What this buys

- P1-5 dissolves: calendar's existing Go hook works in a tenant unchanged, and
  the migration keeps the relaxed create rule with no bootstrap problem and no
  pb-hook.
- The whole finding class dissolves with it. Any future "the Go guard doesn't
  run in a tenant" stops being a security finding.
- Features stop needing to express enforcement twice (Go for the app, rules for
  the tenant), which is where the drive/commentor divergence came from.

## What it costs

- ~19 MiB per tenant process, times every resident org.
- The tenant binary's dependency surface grows to everything the features
  import. The existing comment at `serve-org/main.go:124` resists exactly this
  for `coreserver` (Sentry, webpush, postmark, go-message) — that instinct
  applies here and deserves an explicit answer rather than a silent reversal.
- One more generated artifact, and a build that breaks if a feature's `server/`
  does not compile.

## Recommendation

Worth doing, but **not inside the Phase 0–3 merge gate**. It is a design change
with an unresolved question (step 5) and a real per-process memory cost;
rushing it into a security remediation is how the next review's findings get
written. For P1-5 now, take the cheap proven path (pb-hook, ~30 lines, already
tested green in the tenant configuration and falsifiable), and schedule this
separately — at which point the pb-hook becomes redundant and can be deleted.

---

## How it was closed (2026-07-27)

Implemented as **D1 (one binary, runtime gating) + D2 (packages.json)**, with
step 5 resolved as **option (b)** — the pinned canonical menu — and two design
additions the proposal did not anticipate, both found by reading the feature
`Register()`s before wiring them in.

**Step 5, decided: option (b).** The menu is hand-pinned in
`multi-org/internal/tenantpkgs` (slug → tenant entry) plus six `replace
tinycld.org/packages/<slug> => ../<slug>/server` directives in `go.mod` — the
same sibling-checkout shape as the existing core replace, and **never
generator-emitted**, so the tenant binary cannot silently depend on a
developer's workspace assembly (option (a)'s failure mode). The recorded
trade: hosted packages outside the menu are TS-hooks/rules-only.
`TestMenuIsThePinnedSet` pins the contents so a menu change is a named,
deliberate edit.

**Addition 1 — features needed a host/tenant seam of their own.** Calling a
feature's `Register(app)` in a tenant was never viable as-is: mail's
`Register` starts IMAP/SMTP/inbound-SMTP TCP listeners on `OnServe` (and in
production a failed listener aborts the boot), and drive/calendar/contacts
mount their own DAV servers — which a tenant already mounts from materialized
config, so linking them naively double-binds the routes. And a hand-rolled
"tenant subset" per feature would recreate exactly the drift
`FINDING-tenant-composition-gap.md` closed. So each Go-bearing feature got the
same structure coreserver got: one `registerShared` (single source of truth),
`Register(app)` = shared + an explicit host-only tail with reasons (mail's
listeners; the DAV mounts), `RegisterTenant(app)` = shared only, and a
composition-parity test per split feature
(`rlstest.HookHandlerCounts`/`AssertCompositionDiff`, verified red). text and
calc have no host-only tail; their two entries are identical by construction.

**Addition 2 — mail is IN the menu, listeners host-only.** The proposal's
implicit assumption that a feature is all-or-nothing was wrong for mail: a
tenant-hosted mail org previously had no `/api/mail/send|search|draft`, no
webhooks, no FTS sync, no mailbox lifecycle at all. With the split, all of
that runs in a tenant; only the port-binding listeners stay host-only
(per-tenant IMAP/SMTP still needs injected listeners — HANDOFF §6, mailproto).

**D2 as proposed.** `orgmanager` writes `<orgDir>/.runtime/packages.json`
(resolved manifest slugs, host-side via `controlplane.PackageSlugs` — the
child must not walk the store) and passes `--packages-config`; like
quota.json it is always written when the hook is wired, so "no packages" and
"config missing" are indistinguishable. `serve-org` feeds the slugs to
`tenantpkgs.Register` through `coreserver.TenantOptions.RegisterExtras` — a
new seam mirroring the host's `Options.RegisterExtras`, called before quota
(feature record hooks must precede the enforcement hook) and before jsvm
(feature `$`-bindings must exist when the org's hook files run).

**D3 as proposed: unchanged.** The DAV protocol servers keep coming from the
materialized source lists; the features' own mounts are host-only. The WebDAV
`BeforeOverwrite` version snapshot therefore remains single-org-only
(history, not safety).

**D4 / step 7.** `GOOS=linux go build ./...` and
`GOOS=linux go test -c ./internal/orgmanager/` both pass with features
linked. (Linking drive briefly broke the full cross-compile — goheif's cgo
dav1d cannot build with CGO_ENABLED=0 — but goheif was dropped the same day:
core/thumbnails now renders HEIC/HEIF through doctaculous's pure-Go decoder,
removing the last cgo dep from the tenant graph.) Re-running
`TestConfinement_*` with features linked still requires a Linux host, as
before.

**Step 6, tests.** `TestTenant_FeatureGoIsGatedByPackageSet` spawns the real
serve-org binary twice: an org WITH contacts in its lockfile observes the
`$contacts` jsvm binding, an org WITHOUT does not — one test, both polarities,
through the real store → lockfile → packages.json → `--packages-config` path.
`TestRegisterGatesBySlug` covers the gate at unit level;
`packages_config_test.go` covers the materialization; the per-feature parity
tests cover the split.

**What this closed elsewhere.** P1-5's interim pb-hook (the tenant
owner-membership bootstrap in `calendar/pb-hooks/calendar.pb.ts`) is
**deleted** — calendar's Go bootstrap hook
(`registerOwnerMembershipBootstrap`, split out for testability) now runs in
tenants, and `tenant_hooks_bootstrap_test.go` binds it the way a tenant runs
it. The FINDING's "account-delete cannot settle feature-owned rows in a
tenant" item also closes for menu packages: `userorg.RegisterReassignable`
runs from each feature's shared set, so the reassignable registry is populated
in a tenant.

**What stays open.** Per-tenant IMAP/SMTP (injected listeners); the WebDAV
version-snapshot-on-overwrite in tenants (D3's deferred second question);
`TestConfinement_*` still need a Linux host; and hosted third-party packages
with Go remain a product question — option (c) is the recorded escape hatch
if that answer ever changes.
