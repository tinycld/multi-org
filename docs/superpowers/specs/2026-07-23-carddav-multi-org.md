# Spec — Multi-Org CardDAV (per-org, org-from-hostname)

**Date:** 2026-07-23
**Status:** IMPLEMENTED + verified (integration test
`TestIntegration_MultiOrgCardDAV` proves two orgs serve their own contacts
independently through `inst.Mux()`).
**Depends on:** the single-tenant CardDAV pilot (core `carddav`/`fts`/`audit`
capabilities, `contacts` de-Go'd — see the tinycld plan
`~/.claude/plans/purrfect-zooming-fog.md`).

## Context

The single-tenant pilot serves CardDAV from the core `carddav` capability,
driven by the `contacts` package's `carddav` manifest block, over the one app DB.
That path does not run in a multi-org **tenant**: tenants are **stock
PocketBase** (`apis.BuildServeMux`) with only JS/TS hooks + migrations — no
feature Go, so `coreserver.Register` (which mounts `/carddav`) never executes for
a tenant.

**Model decided:** *per-org, org-from-hostname.* `acme.tinycld.org/carddav`
authenticates against **acme's tenant `users`** and serves **acme's contacts**; a
user in K orgs configures K CardDAV accounts. This fits the current architecture
with **no new identity model**.

**Why not nested/aggregated** ("one login → a book per org"): it requires a
**cross-org user identity**, which does not exist. The control-plane authenticates
only an **operator superuser** (`_superusers`, from `MT_SUPERUSER_*`) to guard the
provisioning API; it holds `orgs`/`packages`/`deployments` and **no end-user or
membership data** (by design — `controlplane.go`: "no tenant/user data").
End-users authenticate **per-tenant** against each org's own `users` collection.
"alice" in acme's DB and "alice" in globex's DB are independent auth records.
Nesting is therefore blocked on a global-identity + membership + tenant-SSO
feature (overlapping Track-B de-org-ing) — out of scope here.

## Where the org comes from

Dispatch already resolves the org **before any handler runs**:
`frontrouter.Subdomain(r.Host, base)` → slug → `orgmanager.GetOrg(slug)` →
`inst` (the org's `*pocketbase.PocketBase` + stock mux). By the time a `/carddav`
request reaches a tenant, **there is exactly one org's DB present** (`inst.app`).
So CardDAV multiplexing = the subdomain; the handler never resolves org itself.

## Design

### 1. Serve `/carddav` from the host, bound to `inst.app`

`orgmanager.load` currently builds `inst.mux = apis.BuildServeMux(inst.app)`.
Compose a CardDAV handler (trusted host Go, `go-webdav`) **in front** of the
stock mux, bound to that org's app:

```
inst.mux = prefixRouter{
    "/carddav":         carddav.HandlerFor(inst.app, sources, orgScope),
    "/.well-known/carddav": redirect,
    "/":                stockMux,
}
```

- The front router is **unchanged** — it still calls `inst.Mux()`.
- The router module imports `tinycld.org/core/carddav` (new dep edge; the router
  already `replace`s the fork, so importing core is the same kind of local dep —
  add `require tinycld.org/core` + a `replace => ../tinycld/core/server`).
- Multiplexing is free: each org's handler only ever sees its own DB.

### 2. A per-org `OrgScope` for the backend (simplifies the pilot)

The pilot's `Backend` resolves org **by slug across a shared DB**
(`ListAddressBooks` walks `user_org`→`orgs`→slug; `findUserOrg(userID, slug)`).
In a tenant the whole DB **is** the org, so introduce a scope strategy:

```go
type OrgScope interface {
    // Books returns the address books visible to the authed user. Single-org
    // returns exactly one book (this DB); shared-DB returns one per membership.
    Books(app core.App, user *core.Record) ([]Book, error)
    // OwnerFilter returns the PB filter binding for a book's objects.
    OwnerFilter(app core.App, user *core.Record, book Book) (dbx.Params, error)
}
```

- **`singleOrgScope`** (multi-org tenant): one book at `/carddav/u/ab/default/`,
  objects filtered to the authed user's own contacts (`owner`-via-`user_org` for
  *this* user, or simply the user's records — no cross-org slug). Path carries no
  meaningful org segment.
- **`sharedDBScope`** (single-tenant, today's behavior): keep the current
  per-org-book logic verbatim.

The protocol mechanics (PROPFIND/REPORT, ETags, vCard codec, Basic-Auth) and the
`Source`/`VCardMap` config are **unchanged** — only org resolution is pluggable.
`Backend` gains an `OrgScope` field; `HandlerFor(app, sources, scope)` selects
`singleOrgScope`; the single-tenant `coreserver` path selects `sharedDBScope`.

### 3. Surface the `carddav` config to the tenant host

The `carddav` block lives in `manifest.ts`. Two router gaps to close:

- **`ResolvedPackage` carries only `{Name, Version, Dir}`** — no manifest. The host
  reads `<pkg.Dir>/manifest.*` (or a materialized `manifest.json`) for the
  `carddav` block. Prefer materializing a parsed `manifest.json` into the store at
  publish time (the publish path already transpiles TS→JS) so the router never
  needs a TS loader — mirror how the single-tenant generator emits
  `bundled-packages.json`.
- **Hook source dir**: `materialize.linkServerHooks` reads `pkg.Dir/server`, but
  the pilot moved contacts' hooks to `pb-hooks/` (no `server/` dir remains). The
  router must materialize `pb-hooks/*.pb.ts` into the tenant `pb_hooks` too (the
  fork jsvm accepts `.pb.ts`; publish-time transpile already covers multi-org).
  This is the router-side analog of the tinycld generator's `hooks.directory`
  symlinking that the pilot relies on.

### 4. Auth

Basic-Auth against the tenant's `users` (the pilot's `authenticateRequest` is
already feature-agnostic and DB-scoped). No control-plane involvement. The org is
the tenant process; the user is authenticated within it. No SNI needed for
correctness (org = subdomain = tenant); SNI/Host only identify the service.

## Work items

1. **core/carddav**: extract `OrgScope`; add `singleOrgScope`; keep existing logic
   as `sharedDBScope`; `HandlerFor(app, sources, scope)` constructor. (tinycld
   repo, `tinycld/core/server/carddav/`.)
2. **router/orgmanager**: import core/carddav; in `load`, build the CardDAV handler
   over `inst.app` and prefix-compose it with the stock mux into `inst.mux`.
3. **router/materialize**: also materialize `pb-hooks/*` into tenant `pb_hooks`
   (match the pilot's convention), and surface each package's `carddav` config
   (parsed manifest) to the host.
4. **router go.mod**: `require tinycld.org/core` + `replace => ../tinycld/core/server`.
5. **Verify**: provision a tenant with the contacts package; `PROPFIND`/`REPORT`
   against `<slug>.<domain>/carddav/` with a tenant user's Basic-Auth returns that
   org's contacts; `PUT`/`DELETE` round-trip; a second org's tenant serves its own
   contacts independently (multiplex proof).

## Out of scope / follow-ons

- **Nested/aggregated CardDAV** — needs cross-org identity (control-plane users +
  membership + tenant SSO). Separate spec; revisit after that lands.
- **IMAP/SMTP** — same host-Go-over-`inst.app` shape, but stateful sessions;
  deferred.
- **Per-process isolation** — if orgs become isolated processes, the host can no
  longer read `inst.app` directly and CardDAV/IMAP must proxy to per-org backends.
  Keep the backend's data access behind `OrgScope`/an interface so that swap
  doesn't touch protocol code.
