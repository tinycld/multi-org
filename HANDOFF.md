# Handoff — Multi-Org PocketBase Router

**Updated:** 2026-07-25
**Goal:** one PocketBase process hosts many organizations — each org its own
SQLite DB, client bundle, and server-side JS — sharing versioned code where
identical, isolated where not.

This is a working brief, not a changelog. Full narrative history is in the git
log of this file and of the four repos.

---

## 1. Where things stand

**The router works.** A booted `serve-multi` (proxy mode) bootstraps a superuser
from env, provisions an org via `POST /api/orgs`, brings that tenant up with its
application collections, and serves its hooks at `<slug>.<domain>` — proven live
and in tests. It runs **TypeScript** hooks/migrations (esbuild transpile at
publish time, **sobek** engine) and serves **CardDAV per-org host-side**.

**The tinycld app is single-org.** `orgs`/`user_org` are gone; `role` lives on
`users`; routes are bare (`/mail`, not `/a/<org>/mail`). The router owns
multiplexing. Core assumes one org = one DB.

**Packages ship their own Go** (this reversed an earlier "no feature Go"
decision). Core provides reusable libraries — `carddav`, `fts`, `audit`,
`mailer`, `notify`, `thumbnails`, `textextract`, **`mailproto`** — and a package's
`server/` drives them with its own config. Single copy, no duplication: the
router imports the same core libraries.

**Two features are migrated: `contacts` (simplest template) and `mail` (richest).**

### The blocking security finding

Tenant JS runs with full host capabilities. In-process hardening shipped
(jsvm `Sandboxed` mode: deny-by-default bindings, no `$os`/`$http`/`$filesystem`,
scrubbed `process.env`, no file `require()`), **but it is not containment.** A
demonstrated, still-open exploit: `$app.db()` exposes raw SQL, and a sandboxed
hook running `ATTACH DATABASE '<other-org>/data.db'` reads another org's
secrets. modernc SQLite has no authorizer API to stop it in-process. Four
successive audits each found another capability re-entering through a *kept*
surface.

**OS-level per-process isolation is the required next boundary**, not optional
hardening. Until it lands: **do not treat tenant authors as untrusted in
production.** Brief: `docs/superpowers/specs/FOLLOWUP-os-process-isolation.md`.
The `GetOrg → http.Handler` seam in `frontrouter` is the drop-in point.

---

## 2. Repo & branch map

| Repo | Branch | Notes |
|---|---|---|
| `~/code/tinycld/pocketbase` | `feat/multitenant-fork` | **Must stay checked out here** — the router's `replace ../pocketbase` needs both seams + sobek |
| `~/code/tinycld/multi-org` | `multi-org` | The router. No remote. |
| `~/code/tinycld/tinycld` | `multi-org` | App shell + `@tinycld/core` (nested at `tinycld/core`) |
| `~/code/tinycld/{contacts,mail}` | `multi-org` | Migrated features |

Nothing is pushed. `multi-org/` and `pocketbase/` are gitignored by the parent
and are **not** pnpm members.

**Workspace is LEAN: core + contacts + mail.** The other five features
(`calendar drive text calc google-takeout-import`) plus `share-stub`/
`shortcut-stub` are parked at `~/code/tinycld/.parked/` and removed from
`pnpm-workspace.yaml`. To bring one back: move the dir to the workspace root,
add it to `pnpm-workspace.yaml`, `pnpm install`. Its git repo travels with it,
and **the generator emits its `server/go.work` with the fork replace for free**.

---

## 3. Converting the remaining features

`calendar`, `drive`, `calc`, `text` are un-migrated. Use **contacts** as the
template for a simple feature, **mail** for one with a Go server. Order matters.

### 3.1 Unpark and get a real baseline

Unpark (§2), `pnpm install`, then `cd <member>/server && go build ./...`. The
compile errors ARE the checklist. Expect at minimum:
- `audit.CollectionConfig` has **only `ExtractLabel`** — every `ResolveOrg` fails.
- `notify.NotifyParams` has **no `OrgID`**.
- `orgs`, `user_org`, `org_provisioning` collections no longer exist.

### 3.2 Rename the lying fields FIRST — highest-value step

The earlier de-org pass repointed `user_org` FKs to `users` **but kept the field
names**. A field called `user_org` now holds a *users* id. Nothing type-errors;
queries just silently match zero rows.

Rename `user_org` → `user` in the migrations (edit **in place** — no back-compat,
fresh DBs re-migrate), rebuild indexes/views/RLS, and mirror in `types.ts` /
`collections.ts` / `seed.ts`. This converts silent breakage into loud errors
before you touch the Go.

> **This bug class is invisible to tests.** Reproduced live on mail: with the old
> `ts.user_org` column, `GET /api/mail/search?q=…&folder=inbox` returns
> `{"total":0}` with HTTP 200 — and **the entire Go unit suite still passes**.
> Only the live smoke test (§4) catches it. Budget for it.

### 3.3 De-org the Go

Mechanical once §3.2 is done. Recurring shapes:
- Collapse org helpers → a membership check on `user` + `verifyAdmin(auth)`
  reading `users.role` (no DB lookup — role is on the auth record).
- Drop `ResolveOrg` from audit configs; drop `OrgID` from notify params.
- Rebind `user_org`/`org_provisioning` hooks to `users`.
- Collapse per-org settings/provider lookups to deployment-wide `system_settings`.
- Notification deep links: `/a/<slug>/<pkg>` → `/<pkg>`.
- Per-org fan-out loops collapse to one query (mail's IMAP Login and search each
  shed an N+1 for free).

### 3.4 Widen `*pocketbase.PocketBase` → `core.App`

Do this for every feature. Nearly free, and it unlocks everything else.

Verify first (mail and carddav both passed): every method called on `app` —
`Save`, `Delete`, `FindRecordsByFilter`, `Logger`, `DB`, `OnServe`,
`NewFilesystem` — is on the `core.App` interface, and `Start`/`Execute`/
`RootCmd` appear nowhere. The concrete type is incidental. (carddav's comment
claiming it needed the concrete app "for record Save/Delete" was simply wrong.)

Keep the concrete type in exactly one place: `func Register(app
*pocketbase.PocketBase)` — the generator's contract and core's `audit` API.

Widening is what makes protocol code host-agnostic: the single-tenant app, a
multi-org tenant, or a future per-org subprocess can all drive it.

### 3.5 Only then consider lifting code into core

The rule, learned from carddav and mailproto: **lift the transport, keep the
sessions.**

- **Generic** = protocol/transport plumbing with no feature schema in it (TLS,
  listener lifecycle, the DAV handler). Lift it.
- **Not generic** = anything saturated with the feature's data model. Mail's IMAP
  session speaks threads, folders, UIDs, flags, mailbox membership;
  config-driving it would mean reimplementing the schema as configuration. Leave
  it in the package and **inject** it (`NewIMAPSession`, `smtp.Backend`).

Measure before deciding — `grep -o "<pkg>_[a-z_]*"` per file cleanly separated
mail's 721 generic lines from its schema-bound ones.

Expose lifted state as a **type, not a package global** (`mailproto.IdleNotifier`),
so a multi-org host can hold one per tenant.

### 3.6 Tests

- Rewrite org fixtures against `users.role`.
- **Check RLS tests assert the rules the migrations actually ship.** Mail's
  `guest_rls_test.go` passed while asserting self-defined `user_org_via_org`
  strings that had already diverged from the shipped migrations — it validated
  its own fixture. After rewriting, confirm the test *fails* when you neuter the
  guard.
- **Scope e2e assertions to the package's own UI.** Contacts' specs used bare
  `getByText('Alice')` and broke the moment mail was installed — mail's sidebar
  renders "Hey Alice, I submitted a PR…". Use a row testID
  (`[data-testid^="contact-row-"]`). Expect this as more packages return.
- Virtualized lists (FlashList) mount only visible rows — search to bring a row
  into the window rather than asserting a row that never mounts.

---

## 4. Verify

```sh
# Member Go — plain go, NOT GOWORK=off (core resolves only via go.work)
cd <member>/server && go build ./... && go vet ./... && go test ./...

# Core + assembled app shell
cd tinycld/core/server && go build ./... && go test ./mailproto/ ./carddav/ ./fts/ ./audit/ ./coreserver/
cd tinycld/server && go build -o /tmp/tinycld-server .

# Router (must stay green — it imports the same core libs)
cd multi-org && go build ./... && go vet ./... && go test ./... -count=1
go test ./internal/controlplane/ -run TestIntegration_MultiOrgCardDAV -v

# TS
cd <member> && pnpm exec tinycld-pkg check
cd tinycld && pnpm run checks

# e2e (kill stray dev servers first — see §5)
cd <member> && pnpm exec tinycld-pkg test:e2e
```

**Live smoke test — do not skip.** The only thing that catches silent zero-row
matches:

```sh
/tmp/tinycld-server serve --dir /tmp/pbX \
  --migrationsDir <app>/server/pb_migrations --hooksDir <app>/server/pb_hooks --http 127.0.0.1:8899
```
1. Assert the schema: **no** `orgs`/`user_org`/`org_provisioning`; FKs →
   `_pb_users_auth_`; `users.role` present.
2. Exercise the feature's **filtered/scoped** read paths (the ones joining
   through the renamed field) — not just an unfiltered list.
3. Check an RLS boundary live (a `guest` denied where a `member` is not).
4. Drive any protocol server end-to-end.

---

## 5. Gotchas

1. **`GOWORK=off go test` does not work for a member** — core resolves only via
   `go.work`. Use plain `go test` from `<member>/server`.
2. **Regenerate after changing migrations**: `cd tinycld && pnpm run
   packages:generate`. `pbSchema.ts` is generated from the on-disk migrations; a
   stale copy produces a wall of type errors in *core and other members* that
   looks far worse than it is. Regeneration is deterministic.
3. **Kill stray dev servers before e2e.** A leftover process holding `:1993` made
   all 8 mail IMAP specs fail with `ECONNREFUSED :1193` — the harness sets
   `IMAP_ADDR` but not `IMAPS_ADDR`, so the dev implicit-TLS listener collides
   and aborts IMAP startup entirely.
4. **The fork must stay on `feat/multitenant-fork`** or the router won't compile
   (missing `BuildServeMux`, goja↔sobek mismatch).
5. **`.pb.ts` hooks:** a top-level `const`/`function` is **not in scope inside a
   hook callback** after the TS→JS transpile wrap — keep everything inline. An
   all-comment `.pb.ts` fails to load ("sourcemap: mappings are empty"), so
   include at least one live statement. Plain `.pb.js` is unaffected.
6. The two esbuild call sites (fork `transformSource`, router `transpileForStore`)
   are duplicated across repos, kept in sync by a golden test.

---

## 6. Open work

**Blocking / security**
- **OS-level per-process tenant isolation** (§1). The required next deliverable.
  Post-isolation, CardDAV/IMAP become **proxies to per-org backends** rather than
  host-side handlers — which is why `mailproto` is deliberately router-ready but
  **unwired**, and why package Go inside tenants stays deferred.

**Feature migration**
- `calendar`, `drive`, `calc`, `text` — un-migrated (§3).

**Upstreaming / release**
- PR #1 `jsvm.ProgramSource`: push from
  `nathanstitt/pocketbase:feat/jsvm-programsource` (still goja-era; the sobek
  swap is downstream-only — a separate decision).
- PR #2 `apis.BuildServeMux`: rebuild a clean branch off `v0.39.8` (the pushed
  `-buildservemux` branch is stale; delete it after).
- Push the 7 `chore/bump-pocketbase-v0.39.8` branches, or fold into a release.
- Give `multi-org` a remote if it should be shared/CI'd.

**Cleanup (non-blocking)**
- Store "content-addressed" naming is vestigial (`ContentHash`/`manifest` unused).
- `lockfile.Resolve` doesn't run the `peerVersions` solver yet.
- Org switcher: `OrganizationsTab` is stubbed pending the router setting a
  parent-domain cookie listing accessible orgs.

---

## 7. Design docs

- **Specs/plans** (this repo, `docs/superpowers/`): TypeScript hooks
  (`2026-07-23-typescript-hooks*`), tenant sandbox
  (`2026-07-23-tenant-jsvm-sandbox*`), multi-org CardDAV
  (`2026-07-23-carddav-multi-org.md`), process isolation
  (`FOLLOWUP-os-process-isolation.md`).
- **Fork seams + router design** (2026-07-22): in the fork's git history.
- **Core capability/hook reference**: `tinycld/docs/hooks.md`,
  `tinycld/docs/packages.md`.
- Approved plans: `~/.claude/plans/{streamed-orbiting-pretzel,rustling-popping-babbage,rosy-weaving-blossom}.md`.
