# Handoff — Multi-Org PocketBase Router

**Updated:** 2026-07-26
**Goal:** one router hosts many organizations — each org its own **OS process**,
SQLite DB, client bundle, and server-side JS, sharing versioned code on disk but
isolated at the kernel boundary.

This is a working brief, not a changelog. Full narrative history is in the git
log of this file and of the four repos.

> **A full pre-merge review ran on 2026-07-26** across the router, core (Go +
> TS), the app shell and all seven features. Every build/vet/test/lint gate is
> **green in every repo** — and the review still found a critical socket-hijack
> hole, four HIGH security findings and a live-broken feature, none of which any
> suite catches. **Read §7 before merging.** The pattern worth internalizing:
> the green suite is not evidence, because in nearly every case the test that
> should have failed was asserting its own fixture.
> The remediation is planned in **`REMEDIATION-PLAN.md`** (64 items, 7 phases;
> phases 0–3 gate the merge).

---

## 1. Where things stand

> **✅ RESOLVED (2026-07-27) — `docs/FINDING-tenant-composition-gap.md`.**
> A tenant used to hand-roll a subset of `coreserver.Register` (jsvm + quota
> only), missing the users field guard, the disabled-user guard, notify,
> realtime, userorg and the DAV CORS bypass — so `users.updateRule` alone let a
> plain member PATCH their own role to `owner`. Fixed: `coreserver` now has a
> shared composition set (`registerSharedEarly`/`registerSharedCore`) and a
> `RegisterTenant` entry point that `serve-org` calls instead of hand-listing
> registrations. A reflection-based parity test
> (`coreserver/composition_parity_test.go`) fails if the two compositions ever
> diverge without a recorded host-only reason, and `coreserver/tenant_test.go`
> is the permanent regression coverage for both proven holes. The tenant now
> also wires the jsvm `$`-binding / hook-point seams (see §6).
>
> **✅ Feature-package Go now links into tenants too (2026-07-27) —
> `docs/SCOPE-tenant-feature-go.md` is CLOSED** as its option (b): serve-org
> links a hand-pinned menu (`internal/tenantpkgs` + go.mod replaces — never
> generator-emitted), gated per org by `.runtime/packages.json` /
> `--packages-config`. Every Go-bearing feature split into
> `registerShared` + host-only tail with `Register` (host) / `RegisterTenant`
> (tenant) entries and its own composition-parity test: mail's IMAP/SMTP
> listeners and the drive/calendar/contacts DAV mounts are host-only (a tenant
> mounts DAV from materialized config, and the router owns every listening
> socket); everything else — record hooks, endpoints, guards, FTS, workers —
> runs in a tenant. `TestTenant_FeatureGoIsGatedByPackageSet` proves the gate
> through the real binary in both polarities. This deleted calendar's P1-5
> pb-hook and populated the tenant's `userorg` reassignable registry.

**The router works, and tenants now run in their own OS processes.** A booted
`serve-multi` (proxy mode) bootstraps a superuser from env, provisions an org via
`POST /api/orgs`, **spawns a `serve-org` child** for that tenant, and reverse-
proxies `<slug>.<domain>` to it over a per-org unix socket — proven live and in
tests. It runs **TypeScript** hooks/migrations (esbuild transpile at publish
time, **sobek** engine) and serves **CardDAV inside each tenant process**.

The router holds no tenant app object. `orgmanager` is now a process supervisor:
spawn → readiness handshake over a pipe → proxy; plus crash detection with
exponential backoff, drain-then-TERM-then-KILL eviction, and the idle sweeper.

**The tinycld app is single-org.** `orgs`/`user_org` are gone; `role` lives on
`users`; routes are bare (`/mail`, not `/a/<org>/mail`). The router owns
multiplexing. Core assumes one org = one DB.

**Packages ship their own Go** (this reversed an earlier "no feature Go"
decision). Core provides reusable libraries — `carddav`, **`caldav`**, `fts`,
`audit`, `mailer`, `notify`, `thumbnails`, `textextract`, **`mailproto`**,
**`driveshare`** — and a package's `server/` drives them with its own config. Single copy, no
duplication. `driveshare` is the newest: the one definition of "may this user
read / write / delete this drive_item", shared by drive, text and calc, and the
Go mirror of the `drive_items` collection rules.

**The rule that decides where code goes is about PORTS, not about Go.**

> **A service that must BIND A PORT moves into core, so the multi-org router
> can open the port.** Everything else is a normal engineering choice:
> performance-sensitive work stays in Go, and TS/JS hooks are the customization
> seam on top.

A tenant serves on a unix socket the router hands it (`serve-org --socket`),
and the router owns the listening sockets for every org. A feature that wanted
its own listener would need a port the router did not open and cannot route —
which is why CardDAV, CalDAV and WebDAV became core libraries fed by
declarative config the router materializes. It is not that their Go was
untrusted; it is that they listen.

Feature Go is not forbidden in a tenant — and since 2026-07-27 it RUNS there:
`serve-org` links the pinned feature menu (`internal/tenantpkgs`, gated by the
org's resolved package set), calling each feature's `RegisterTenant` — the
shared composition minus port listeners and DAV mounts. Enforcement in a
feature's request hooks therefore covers tenants for menu packages; collection
rules remain the layer that travels with the schema and covers packages
outside the menu. See `docs/SCOPE-tenant-feature-go.md` for the decisions.

**All seven features are migrated: `contacts` (simplest template), `mail`
(richest), `drive` (the protocol lift + the Go→TS hook seam), `text` + `calc`
(the realtime/Yjs pair, which also produced `core/driveshare`), `calendar` (which
produced `core/caldav`), and `google-takeout-import` (client-only, but the
sharpest instance of mirrored-schema drift).** The feature migration is done.

Core now also has a **Go→TS hook-point seam** (`coreserver/ts_hooks.go`, plus
`jsvm.Config.OnLoaderInit` in the fork). It lets host-owned Go call registered
package TS at defined points and use what it returns — the escape valve for
behaviour declarative config can't express. The fast path never touches a VM: a
point nobody registered costs one atomic load, which matters because jsvm's pool
builds a whole new runtime once every executor is busy. WebDAV is the first
consumer (five points via `webdavHook`). Full reference: `tinycld/docs/hooks.md`.

### The security boundary (was: the blocking finding)

**Resolved in design and code; unverified by CI.** The blocking finding was that
`$app.db()` exposes raw SQL, so a sandboxed hook running
`ATTACH DATABASE '<other-org>/data.db'` read another org's secrets — and modernc
SQLite has no authorizer API to stop it in-process. Four successive audits each
found another capability re-entering through a *kept* surface, which is what
made in-process allowlisting the wrong altitude.

**The boundary is now the OS process.** On Linux each tenant gets its own uid,
mount + PID namespaces, and cgroup; the package store is bind-mounted read-only
at its real absolute path (a `chroot` to the org dir would break every
`materialize` symlink). jsvm `Sandboxed` mode is retained inside each tenant as
defence in depth, and children are spawned with an **allowlist-constructed**
environment so no `MT_*` secret can leak — by construction, not by filtering.

**Three caveats — the first is CLOSED (2026-07-27):**
1. ~~**`TestConfinement_*` require Linux + root and there is no CI**~~ **CLOSED**
   — the `confinement` workflow (`.github/workflows/confinement.yml`) runs the
   full suite plus `TestConfinement_*` as root on every push/PR; first green
   run 30318853658, all five confinement tests executed and passed. Its first
   run also caught P4-10 live (non-root spawns EPERM'd), now fixed. On darwin
   `-run TestConfinement` prints an explicit SKIP stub.
2. **macOS is not a boundary.** The darwin spawner is a plain subprocess and logs
   a warning saying so. Don't host untrusted tenants on it.
3. **Provisioning is still in-process.** `bootstrapTenantOnce` runs untrusted
   tenant migration JS inside the control-plane process (sandboxed, not
   OS-isolated). Moving it to a one-shot isolated subprocess was scoped out.

Original brief: `docs/superpowers/specs/FOLLOWUP-os-process-isolation.md`
(decisions 1–5 are now answered; 6 resource limits and 7 control-plane remain).

---

## 2. Repo & branch map

| Repo | Branch | Notes |
|---|---|---|
| `~/code/tinycld/pocketbase` | `feat/multitenant-fork` | The fork's own checkout. **The router no longer builds it** — `go.mod:11` replaces to `../tinycld/third_party/pocketbase` (a vendored copy, commit `a37c8ac`; Go sources byte-identical, `ui/` stripped). Keep this checkout for fork development and upstreaming; edits here reach the router only once vendored. |
| `~/code/tinycld/multi-org` | `multi-org` | The router. `origin` = `github.com/tinycld/multi-org`, pushed. |
| `~/code/tinycld/tinycld` | `multi-org` | App shell + `@tinycld/core` (nested at `tinycld/core`) |
| `~/code/tinycld/{contacts,mail,drive,text,calc,calendar,google-takeout-import}` | `multi-org` | Migrated features (all seven) |

**All nine repos have remotes under `github.com/tinycld/`, and `multi-org` is
pushed** (`origin/multi-org`, 0 ahead / 0 behind as of 2026-07-26) — an earlier
revision of this file said "no remote, nothing is pushed", which is no longer
true and had been read as blocking Linux CI. `multi-org/` and `pocketbase/` are
gitignored by the parent and are **not** pnpm members.

**Workspace is core + all seven features** (contacts, mail, drive, text, calc,
calendar, google-takeout-import). Only `share-stub`/`shortcut-stub` remain parked
at `~/code/tinycld/.parked/` and out of `pnpm-workspace.yaml`. To bring one back:
move the dir to the workspace root, add it to `pnpm-workspace.yaml`,
`pnpm install`. Its git repo travels with it, and **the generator emits its
`server/go.work` with the fork replace for free**.

---

## 3. Converting a feature

Every feature that shipped is migrated, so this section is now a **playbook for
the next one** (a new package, or a downstream fork catching up) rather than a
to-do list. Use **contacts** as the template for a simple feature, **mail** for
one with a Go server, **drive** for one whose Go includes a protocol server worth
lifting, **text**/**calc** for one that drives a realtime room, and **calendar**
for one whose protocol server needs both a config lift and a TS customization
seam. Order matters.

> **What the text/calc pass added (2026-07-26).** Both had stopped at commit 3 of
> drive's 12 (pb bump → schema → client), so they carried the schema/client
> de-org but none of the Go, rename, or e2e work. The result was worse than
> stale: every share lookup read `drive_items.org`, a column their own step-2
> migration had deleted, so it returned `""`, tripped a fail-closed guard, and
> denied **everyone** from every room and render — while building clean and
> passing every test. Their PB rules still walked `drive_shares.user_org`
> (renamed to `user` by drive's `321e29a`), which PB now rejects outright at
> migration-apply time: `pnpm install` fails with `unknown field "user_org"`.
> That is the loudest signal in the whole migration — use it as the baseline.
>
> Three things generalize:
> - **The triple-duplicated share predicate is now `core/driveshare`.** calc's
>   own comments admitted it "mirrors the canonical filter in
>   text/server/authorize.go". Lifting it exposed a real divergence: drive
>   honors a `created_by` disjunct (a creator reaches their own item without a
>   share row) and text/calc never did. Core makes it unconditional, which
>   slightly widens drive's write/delete to match the rules the REST API
>   already enforces.
> - **Never delete a "cross-org staleness" test by adapting it.** Rewriting it
>   for single-org silently inverts its meaning (it grants a real editor share
>   and then expects denial). Delete it and note that `userorg.OffboardUser`
>   owns that property now.
> - **A guard test you have not seen fail is not a guard.** Both RLS suites were
>   verified by neutering the rule and confirming the deny-side tests go red.

### 3.1 Unpark and get a real baseline

Unpark (§2), `pnpm install`, then `cd <member>/server && go build ./...`. The
compile errors ARE the checklist. Expect at minimum:
- `audit.CollectionConfig` has **only `ExtractLabel`** — every `ResolveOrg` fails.
- `notify.NotifyParams` has **no `OrgID`**.
- `orgs`, `user_org`, `org_provisioning` collections no longer exist.

> **The compile error count lies about the work.** Drive produced THREE compile
> errors and 44 runtime references to `orgs`/`user_org`/`org` that the compiler
> is structurally blind to, because PB filters are strings. Grep for
> `"user_org"`, `"orgs"`, `Set("org"`, `GetString("org")`, `org = {:org}` before
> believing a green build. `userorg.RegisterReassignable` is the opposite trap:
> it looks like dead multi-org machinery but was reworked for single-org account
> offboarding — keep the registrations or account deletion silently breaks.

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

Widening is what makes protocol code host-agnostic: the single-tenant app or a
per-org tenant process can both drive it. This has already paid off — widening
`carddav.HandlerFor` to `core.App` is the only reason CardDAV could move into
`serve-org` with no signature change when isolation landed.

### 3.5 Only then consider lifting code into core

The rule was **lift the transport, keep the sessions** — carddav lifted as pure
data, mailproto lifted only the socket and injected the schema-bound session.
WebDAV (drive) landed between the two and turns that binary into a spectrum:

| Lift shape | When | Example |
|---|---|---|
| **Pure data** | the feature's contribution is a flat field map | `carddav.Source` |
| **Data + Go callbacks** | protocol is generic, but a few *non-access* decisions are schema-bound | `webdav.Source` + `Hooks` |
| **Transport only** | the session itself speaks the schema | `mailproto` + `NewIMAPSession` |

A file tree is *data* as far as the protocol cares (name, size, parent, blob),
but its authorization, quota and versioning are not. Encoding those as config
would have meant reimplementing drive's model as configuration — the objection
mailproto raises against config-driving sessions. So `Source` carries the field
map and `Hooks` carries five Go callbacks (`CanRead`, `CanWrite`, `CanDelete`,
`CheckQuota`, `BeforeOverwrite`). Drive's ~900 lines of protocol code became a
40-line config literal.

**The cost of that middle shape, and the lesson:** `Hooks` are Go func values,
so they cannot cross a process boundary — a tenant gets nil hooks, and a nil
hook means unrestricted. Authorization originally rode there, which silently
made a tenant-served tree readable by any member of the org.

The fix generalizes: **never put an enforcement boundary in a lifted hook.** A
Go closure cannot cross a process boundary, so anything a tenant must enforce
has to be expressible as data or as something core owns.

Both of drive's boundaries moved out as a result:
- **Authorization** → the collection's own PB rules, via `app.CanAccessRecord`.
  A rule is a string, travels in the schema, and is the same definition the REST
  API and web UI use, so the two cannot drift.
- **Quota** → `core/quota`, bound as a record hook. Every write goes through
  `app.Save`, so REST, WebDAV and IMAP are covered by construction rather than
  each protocol remembering to check.

What is left in `Hooks` is the version snapshot — not a boundary, so losing it
in a tenant costs history rather than safety.

Measure before deciding — `grep -o "<pkg>_[a-z_]*"` per file cleanly separated
mail's 721 generic lines from its schema-bound ones.

Expose lifted state as a **type, not a package global** (`mailproto.IdleNotifier`).
Under per-process isolation each tenant has its own address space, so a global
would no longer alias across orgs — but a type still beats a global for testing,
and it keeps the single-tenant app (where they DO share a process) correct.

Note for IMAP/SMTP specifically: `mailproto` binds fixed TCP ports
(`imap.go`, `smtp.go`). Following CardDAV into the tenant process needs an
injected listener first, or every org after the first fails to bind.

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

### 3.7 What the calendar + takeout pass added (2026-07-26)

**`calendar` was half-migrated, not un-migrated** — its `multi-org` branch already
carried de-orged migrations and client (commits `a4ad691`, `d9fddbd`), but the
entire 1,900-line Go server, `seed.ts`, both Go RLS suites and two e2e specs were
untouched. Same shape as the text/calc trap: schema and client de-orged, Go left
reading columns that no longer exist (`lifecycle.go` wrote `cal.Set("org", …)`
against a dropped field). **Check the member's branch log before believing "un-migrated".**

**`google-takeout-import` owns nothing** — no migrations, no Go, no collections,
no routes. It writes into **nine collections belonging to four other packages**,
so all of its breakage was *mirrored foreign schema*: 8 sites still wrote
`user_org`. Its whole suite was green, 22/22, because `pb` is mocked and the
filter was never checked against a real collection. **A package that mirrors
another's schema needs a test that asserts the FIELD NAME**, or its suite
certifies the bug — one was added.

**CalDAV lifted to `core/caldav`, WebDAV-shaped.** Authorization moved to the
collections' own PB rules via `app.CanAccessRecord`, which subsumed BOTH
`resolveCalendarMembership` AND `requireEditorRole` — the event rules already
encoded the viewer/editor split. The `orEqualsFilter` fan-out and
`calendarDisplayName` (org-name suffixing) deleted outright; `server/auth.go` was
a duplicate of `core/davauth`.

Four things generalize:

- **A lift can silently drop a non-obvious concern.** `caldav_sentry.go` was the
  only place CalDAV errors reached Sentry (go-webdav turns a backend error into an
  `http.Error` and returns nil, so request middleware never sees it). It became
  `Source.OnError` + a decorator that wraps the *interface*, so adding a protocol
  method breaks the build rather than going unreported — and it filters the
  not-found sentinel so routine 404s don't bury real faults.
- **`CanAccessRecord` cannot authorize a CREATE.** It evaluates the rule as a
  query filtered to `id = record.Id`, so an unsaved record matches nothing and
  every create rule denies. Save inside `RunInTransaction`, evaluate, roll back on
  refusal — what PB's own create API does. This cost a live 500 and no unit test
  saw it.
- **A required field with no schema default breaks a minimal protocol write.**
  `busy_status`/`visibility` are required selects; a minimal VEVENT carries neither
  TRANSP nor CLASS, so the save was rejected. Fixed with `Source.Event.Defaults` —
  **data, not a Go callback**, so a tenant gets them too. Same rule as
  authorization: anything a tenant must do has to be expressible as data.
- **iCalendar value types are load-bearing.** A *decoded* RRULE has value type
  `RECUR`, and `Props.Text()` rejects non-TEXT — returning `""`, so every
  client-sent recurrence was silently dropped. Emitting one with `SetText()` stamps
  `VALUE=TEXT` and escapes the separators (`FREQ=WEEKLY\;BYDAY=TU`), which no
  client can parse. **Both directions were broken and both were invisible to
  in-memory tests**; only a round trip through real iCalendar *bytes* catches it.
  There are now five wire-format tests that do exactly that.

Also: denials must be wrapped in `webdav.NewHTTPError(404, …)`. A bare sentinel
becomes a 500 — the masking still holds, but a routine miss is misreported as a
server fault. `errors.Is` still matches through the wrapper.

And a caution about diagnosis: an early hypothesis here was that the static
catch-all (`Router.Any("/{path...}")`) was swallowing DAV GETs, and a guard was
added to `coreserver/static.go` before a probe test showed the literal route
already wins. The guard was reverted. **Prove which handler runs before changing
routing** — the 500 was the DAV handler correctly refusing a GET on a collection.

---

## 4. Verify

```sh
# Member Go — plain go, NOT GOWORK=off (core resolves only via go.work)
cd <member>/server && go build ./... && go vet ./... && go test ./...

# Core + assembled app shell
cd tinycld/core/server && go build ./... && go test ./mailproto/ ./carddav/ ./caldav/ ./webdav/ ./davauth/ ./fts/ ./audit/ ./coreserver/
cd tinycld/server && go build -o /tmp/tinycld-server .

# Router (must stay green — it imports the same core libs)
cd multi-org && go build ./... && go vet ./... && go test ./... -count=1
go test ./internal/controlplane/ -run TestIntegration_MultiOrgCardDAV -v

# Tenant-process tests build and spawn the real serve-org binary; -short skips them.
go test ./internal/orgmanager/ -run TestTenant -v

# The confinement tests — the ones that actually prove the boundary — are
# `//go:build linux`, so on darwin they are NOT COMPILED: `-run TestConfinement`
# prints "no tests to run" and exits 0. That is a vacuous pass, not a skip.
# Cross-compile at minimum (this works even with feature Go linked: dropping
# goheif for doctaculous removed the last cgo dep from the tenant graph):
GOOS=linux go build ./... && GOOS=linux go test -c -o /dev/null ./internal/orgmanager/
sudo go test ./internal/orgmanager/ -run TestConfinement -v   # on a Linux host

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
4. Drive any protocol server end-to-end. For WebDAV that means the full cycle
   (PROPFIND / PUT / GET / MKCOL / MOVE / DELETE); for CalDAV, PROPFIND / PUT /
   GET / DELETE **through real iCalendar bytes**, including a **minimal** VEVENT
   (no TRANSP/CLASS — that is what catches a required-field-with-no-default) and a
   complex `RRULE` (a decoded rule is RECUR-typed, so a Text()-based read drops it
   and a SetText()-based write mangles it — see §3.7). Plus:
   - `OPTIONS` must answer `Dav: 1, 2` — class 2 is what makes Finder mount
     read-write, and losing the `NewMemLS` lock system silently downgrades it.
   - Another user's path must return **404, not 403**. Not-found masking is what
     stops a probe confirming a path exists — and check the STATUS, not just the
     refusal: a bare error sentinel surfaces as 500, which masks correctly but
     misreports a routine miss as a server fault.
   - A **create** must be exercised, not just an update. `CanAccessRecord` filters
     on `id = record.Id`, so it cannot authorize an unsaved record (§3.7).
5. If the feature ships TS hook points, boot **with a handler registered and
   again without one**, and diff the behaviour. Both bugs in the drive migration
   (the jsvm ordering, the shorthand handler) were invisible to unit tests and
   showed up only here.

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
7. **Build BOTH binaries.** `serve-multi` spawns `serve-org` and resolves it as a
   sibling of its own executable. `go build ./cmd/serve-multi` alone yields a
   router that 503s every org with "spawn: no such file or directory".
8. **Never `log.Fatal`/`os.Exit` after a tenant can exist.** Both skip deferred
   functions, so `mgr.Shutdown()` never runs and every child is orphaned —
   surviving the router and holding its port. This actually happened; that is why
   `serve-multi` puts everything in `run() error` and keeps `log.Fatal` in `main`
   alone. New failure paths must `return err`.
9. **The spawn context must not own the child.** `exec.CommandContext` ties the
   process to the context, and the spawn context is cancelled the moment `load`
   returns — which killed every tenant the instant it became ready, presenting as
   a uniform 502. Use `exec.Command`; the context bounds the readiness handshake
   only, and the manager owns the lifetime via `Evict`/the supervisor.
10. **`jsvm.Register` runs the hook files SYNCHRONOUSLY.** `registerHooks()` is
    a direct call inside it — only `refreshTypesFile` defers to `OnBootstrap`.
    So every `$`-binding and loader binding a package contributes must be
    registered BEFORE it, which is why `coreserver.Register` calls
    `RegisterExtras` first. Get it wrong and the package's `.pb.ts` dies at boot
    with `<hook> is not defined`. Unit tests will NOT catch this: they register
    bindings before jsvm by construction. Only booting the real server does.
11. **A JS handler passed as method shorthand is not an expression.**
    `{ beforeWrite(e) {…} }` stringifies to `"beforeWrite(e) {…}"` — a method
    definition — and fails to compile standalone, while `function` and arrow
    forms are fine. `normalizeHandlerSource` prefixes `function `. Any new
    handler-taking binding needs the same treatment.
12. **Unix socket paths cap at ~104 bytes** (`sockaddr_un.sun_path`), and
    overrunning fails at `bind()` with a bare "invalid argument". A deep `MT_ROOT`
    trips this; `orgmanager.socketPath` falls back to a hashed dir under
    `os.TempDir()`.

---

## 6. Open work

**Blocking / security**
- **Linux CI for `TestConfinement_*`** (§1, REMEDIATION P5-1). Per-process
  isolation has shipped, but the tests that prove it need Linux + root and
  never run today. Until they do, the boundary is verified by construction
  only. ~~§7 found the construction itself is wrong (the shared socket dir)~~
  **fixed** — P0-1's per-org socket dirs shipped (verified 2026-07-27), so CI
  is the remaining gap and now the top item. Two of these tests are still
  vacuous even on Linux (§7.4 / P3-4), so repair them as part of standing CI
  up — and CI is also the only way to re-run confinement with feature Go
  linked (SCOPE-tenant-feature-go D4).
- **Provision-time migrations still run in the control-plane process**
  (`bootstrapTenantOnce`). Move to a one-shot isolated `serve-org` invocation.
- **Resource limits.** `MT_CGROUP_ROOT` creates a per-tenant cgroup but writes no
  limits; a runaway tenant can still starve the host. (Brief decision #6.)

**Protocol servers**
- ~~A tenant-served WebDAV tree is broader than the single-tenant one.~~
  **CLOSED.** Core evaluates the collection's own PocketBase rules
  (`app.CanAccessRecord`: ListRule for listings, ViewRule for Stat/GET,
  UpdateRule for PUT-over/MOVE, DeleteRule for DELETE) instead of taking Go
  permission callbacks. A rule is a string and travels in the schema, so a
  tenant enforces exactly what the single-org app does, from the one definition
  in the migration.
- ~~A tenant-served write skips quota accounting.~~ **CLOSED.** `core/quota`
  enforces both ceilings as record hooks, so every write path is covered
  including inside a tenant. The org ceiling comes from `orgs.storage_limit_bytes`
  on the control plane, materialized to `<orgDir>/.runtime/quota.json` — NOT from
  the tenant's own `settings`, where its superusers could raise the plan they were
  sold. Sources ride the same file from each package's `quota` manifest block, so
  the total spans drive and mail.
  **Remaining gap, small:** `BeforeOverwrite` is still a Go hook, so a
  tenant-served overwrite does not archive the previous version. That loses
  history; it does not let anyone exceed a limit.
- ~~**Tenant VMs still get no `$` bindings and no hook points.**~~ **CLOSED
  (2026-07-27).** The import objection dissolved when `serve-org` moved onto
  `coreserver.RegisterTenant` (composition-gap fix, §1): the linker prunes the
  host-only functions, and the heavy deps belong to subsystems a tenant should
  run anyway. `RegisterTenant` passes `buildJsvmOnInit`/`buildJsvmOnLoaderInit`,
  so a tenant's VMs carry core's `$`-bindings and register Go→TS hook handlers.
  ~~Only binders from core sub-packages exist~~ **CLOSED (2026-07-27):**
  feature Go now links via the pinned menu (`docs/SCOPE-tenant-feature-go.md`),
  so an enabled package's own binders (`$drive.*`, `$contacts.*`) exist in
  that org's VMs too — proven by `TestTenant_FeatureGoIsGatedByPackageSet`,
  which also proves an org WITHOUT the package gets none of them.
- CardDAV **and CalDAV** now run **inside** each tenant process (core libs,
  `core.App`-driven, fed by `<orgDir>/.runtime/carddav.json` and `caldav.json`),
  mounted by `RegisterTenant` via `webdav.Register`/`caldav.Register` — the same
  path the single-org app uses, with the host bindings wired.
  CalDAV followed the **WebDAV** shape, not CardDAV's: a `Source` field map plus
  four opt-in TS hook points (`beforeWrite`, `beforeDelete`, `canRead`,
  `filterList` — no `beforeMove`, since CalDAV has no cross-calendar move).
  IMAP/SMTP are the remaining case:
  `mailproto` is still **unwired**, and it binds fixed TCP ports, so it cannot
  run per-tenant as-is — it needs an injected listener before it can follow
  CardDAV into the tenant.

**Feature migration — DONE (2026-07-26).** All seven features are migrated; see
§3.7 for what the last pass added.

**Upstreaming / release**
- PR #1 `jsvm.ProgramSource`: **no longer has a consumer here** — per-process
  isolation ended cross-org program sharing (a `*sobek.Program` is a Go heap
  object and sobek has no bytecode serialization, so there is nothing to share
  across an address space). Still worth upstreaming for single-process
  embedders, but this repo no longer exercises it.
- PR #2 `apis.BuildServeMux`: rebuild a clean branch off `v0.39.8` (the pushed
  `-buildservemux` branch is stale; delete it after). **Still used** — for the
  control plane, which shares the router's process. Tenants use stock
  `apis.Serve` with `ServeEvent.Listener` (upstream, no fork needed).
- Push the 7 `chore/bump-pocketbase-v0.39.8` branches, or fold into a release.
- ~~Give `multi-org` a remote if it should be shared/CI'd.~~ **DONE** — `origin`
  is `github.com/tinycld/multi-org` and the branch is pushed. Linux CI (§7,
  P5-1) is unblocked.

**Cleanup (non-blocking)**

*Cross-tenant watch list (if a router ever shares ONE PocketBase instance).*
`google-takeout-import` dedups by name/uid with no owner predicate —
`batch-inserter.ts:145` (calendars by name), `:185` (`ical_uid`), `:349`
(`message_id`), `:86` (`vcard_uid`). Harmless today: the reads run under the
caller's credentials, so each collection's list rule narrows them, and one
deployment is one org. Under a shared instance they would match across tenants.
Flagged in a comment at the calendar site.

*Version-pin inconsistency, cosmetic.* `peerVersions['@tinycld/core']` disagrees
across members — drive and mail say `>=0.4.0 <0.5.0`, contacts and calendar say
`>=0.0.4 <0.1.0`, and `core/package.json` says `0.0.4`. Nothing enforces it yet
(`lockfile.Resolve` doesn't run the solver — see below), so it is latent, but the
next release should settle on one range.

*App-shell de-org — DONE (2026-07-26).* The follow-up pass fixed four more live
bugs of the same compiler-blind class, all found only by reading against the
shipped schema:
1. **`scripts/reset-demo.ts` reported success while wiping nothing.** It read the
   deleted `orgs` collection inside `try/catch { return null }`, so the nightly
   cron (`coreserver/demo_reset.go`, 04:00 daily when `DEMO_RESET_ENABLED`) logged
   "Demo reset complete" and exited 0 forever. Now wipes per-collection by owner
   FK — verified by running it twice and confirming steady-state counts.
2. **Guest share-link visitors silently fell back to anon.**
   `core/lib/editor/use-share-visitor-role.tsx` filtered on `user_org.user` and
   expanded `user_org` — a field drive renamed to `user`. The query threw, a bare
   `catch` swallowed it, and no visitor ever resolved to `guest`.
3. **Three install specs asserted `'No organizations yet.'`** after bootstrap. The
   dashboard now defaults to Packages and `OrganizationsTab` is a static
   explainer, so that text never renders.
4. **`scripts/test-server-api.ts`** expanded `user_org_via_user.org` on a deleted
   junction.
Also: 8 app-shell e2e specs de-slugged (24 dead `/a/` routes), `ORG_SLUG` deleted,
`admin-organizations.spec.ts` removed (it tested a removed feature and a
`/api/admin/orgs/{id}/impersonate` endpoint that has no implementation), the two
install specs rewritten around the surviving setup flow, `EditorIdentity.userOrgId`
deleted as redundant with `userId` (it had **4** feature-package consumers, not the
0 an audit reported — the compiler found them), the `[[@userId]]` mention contract
renamed through core + text + calc, and text's Yjs root-key literals repointed at
`realtime.RootClientAuthors` / `RootClientFirstSeen` / `RootEditEvents`.

*Deferred deliberately — both assessed and rejected, not forgotten:*
- **`core/render/httpetag` lift: DON'T.** The "mirrors calc verbatim" comment at
  `text/server/render_endpoint.go:28` is aspirational — `writeRenderedItem` and
  `RenderItemHTML` have already diverged. Only ~25 lines per package are genuinely
  shared (router glue, a sha256, three `Header().Set` calls), and lifting needs a
  3-method `Renderer` interface plus `url.Values` in the signature, erasing calc's
  typed `RenderOpts`. text/calc `server/` are separate Go modules; drive has no
  render endpoint, so there is no third consumer. *Cheap alternative:* lift only
  `ETag(itemID, updated, version)` into the existing `core/render`.
- **`core/authorship` lift: NOT YET.** The near-generic claim holds (493 of 652
  lines have no text coupling) but genericity is the precondition for a cheap
  lift, not the trigger — a second consumer is. calc registers no
  `OnDocUpdateContent` callback, ships no blame UI (all 19 authorship TS files are
  under `text/`), and neither calc nor drive has a TODO asking for it. Lifting now
  would also give core its first hard `y-crdt` dependency, which
  `realtime/docruntime.go` deliberately avoids. Lift order when it lands: probe →
  cache → edit_event_buffer (needs a clock param) → writers → stamper (behind a
  `StampTarget` interface).
- **`userOrgId` outside the editor** (~45 sites in `core/components/settings/`,
  `core/lib/leave-org.ts`) is wired to server contracts
  (`/api/invite-link/${userOrgId}`, `?user_org_id=…`) — renaming means moving the
  API paths too. **But first see the leave-org item below: part of that surface
  calls endpoints that no longer exist.**

*Account lifecycle — DONE (2026-07-26).* `LeaveOrgFlow` called
`/api/account/leave-org`, which has no Go implementation — a user clicking
through Settings → Personal got a 404. "Leaving the organization" is meaningless
when the deployment IS the org, so it was refactored into **disable** (reversible)
and **delete** (irreversible):

- **`users.disabled`** (core migration `1930000000`), enforced in Go because the
  schema cannot express it: PB rules can't constrain *which fields* a write
  touches, so `disabled` is admin-only in `users_guard.go` — otherwise a
  suspended user could clear their own flag with a plain pbtsdb update. There is
  no PB `authRule`, so refusing sign-in is a hook (`disabled_guard.go`), bound to
  **both** `OnRecordAuthWithPasswordRequest` *and* `OnRecordAuthRequest` —
  password-only would let a live refresh token renew forever.
- **Two gates, both required.** `driveshare.ResolveRoleForItem` denies disabled
  users (covers WebDAV, render, realtime), and drive migration `1782000000`
  appends `@request.auth.disabled != true` to the drive_items/shares/state rules.
  The REST API evaluates the rules *instead of* the Go, so gating only Go would
  leave `/api/collections/drive_items/records` wide open — and would have looked
  correct in every Go unit test. The RLS guard was verified by neutering the
  clause and confirming the deny-tests go red.
- **Endpoints:** `/api/account/disable` (self, confirm by own email, rotates the
  token key so the suspension is immediate), `/api/account/enable` (admin-only),
  `/api/account/delete` (now routes through `OffboardUser`, restoring the
  reassign / delete-my-data choice it previously ignored — an omitted plan still
  means "leave my content", the safe default), and `/api/admin/users/offboard`
  for admin removal, split out because `/api/account/*` is self-only by
  construction.
- **UI:** `core/lib/account.ts` replaces `leave-org.ts` + `account-delete.ts`;
  `DisableAccountSection` + `DisableAccountFlow` are new, the existing
  `DeleteAccountModal` gained the content picker, and `RemoveMemberFlow` replaces
  the admin half of `LeaveOrgFlow`.

Verified live: a disabled user with a live editor share is refused password
login (403), auth-refresh, the REST read (404) and list (0 rows), and WebDAV
listing + direct GET (404) — while an enabled owner gets 200 on the same file.
Admin re-enable restores all of it.

*Still open here:*
- **Disable rotates the token key, so re-enabling forces a fresh sign-in.**
  `handleAccountDisable` calls `RefreshTokenKey()` — without it a suspension
  would be advisory for hours, since the caller's existing JWT (and any other
  signed-in device) keeps working until it expires. The cost is that the
  session cannot be resumed after an admin re-enables: the user must log in
  again. That is the right trade for a suspension, but it is a deliberate
  choice, not an accident, and it is why the disable copy says "you'll be
  signed out everywhere".
- **Mail delivery to a disabled user's mailbox** is unaddressed — bounce,
  silently accept, or hold? The account can't sign in, but SMTP doesn't consult
  `disabled`.
- **Other packages' collection rules.** ⚠️ **This audit was incomplete — see
  §7.** It correctly found `contacts.listRule` and mail's message rules to be
  own-rows-only, and correctly flagged `mail_mailbox_aliases.listRule`. But it
  missed: **text and calc comments** (both list/view/**create**, on *shared*
  content — §7.4), **calendar entirely** (no rule anywhere carries the clause,
  and it owns shared content via memberships — §7.2), **drive_item_versions and
  drive_share_links** (§7.4), and, most importantly, that **DAV Basic-Auth never
  consults `disabled` at all** (§7.2), which is a third authentication path
  outside the "two gates" model this section describes.

- **`core/server/userorg/`** is still named for the junction it no longer uses.

- Store "content-addressed" naming is vestigial (`ContentHash`/`manifest` unused).
- `lockfile.Resolve` doesn't run the `peerVersions` solver yet.
- Org switcher: `OrganizationsTab` is stubbed pending the router setting a
  parent-domain cookie listing accessible orgs.
- **Org branding has no source.** `useOrgInfo()` returns `org: null`, so an org
  name/logo cannot render anywhere — `DocumentTitle` silently drops its org
  segment, and `getOrgLogoUrl` (`core/lib/use-org-info.ts:23-27`) is unreachable
  dead code. The router materializes `carddav.json`, `caldav.json` and
  `quota.json` into
  `<orgDir>/.runtime/` but nothing for branding. `document-title.spec.ts` had its
  org-segment assertions **deleted** rather than reworded, because there is
  currently no value for them to assert; restoring them means the router
  materializing branding and `useOrgInfo` reading it.

---

## 7. Final review (2026-07-26)

A pre-merge review of the router, core (Go + TS), the app shell, and all seven
features. Nine parallel reviewers each ran their scope's real gates and read
against the **shipped** migrations rather than the tests' idea of them.

**Every gate is green in every repo** (§7.7). That is the headline finding, not
a reassurance: a critical socket-hijack hole, four HIGH security findings and
several live-broken features all sit under a fully green suite. In nearly every
case the test that should have caught it **asserts a constant the test file
itself declares** — the self-validating-fixture trap §3.6 already names for
mail's `guest_rls_test.go`, now found in drive, text, calc, calendar, core and
the router. Drive's finding #1 below is the proof: a rule regressed, and the
RLS suite stayed green because it mirrors the pre-drift rule.

Severity is by **impact if merged as-is**. `[V]` = reviewer read/ran and
confirmed; `[S]` = strong code-reading inference, not executed. Items already in
§6 are not repeated unless this pass found them **inaccurate or worse**.

> **→ `REMEDIATION-PLAN.md`** turns everything below into 64 checkable items
> across 7 phases, with the three product decisions that block coding called out
> up front. Phases 0–3 are the merge gate. This section is the evidence; that
> file is the work.

### 7.1 🔴 Critical — do not merge before fixing

- **[V] Any tenant can hijack every other tenant's socket and receive its
  traffic.** `orgmanager/spawn_linux.go:115` chowns `filepath.Dir(req.SocketPath)`
  to the spawning tenant's uid — but that directory is **shared by all orgs**
  (`<root>/run`, or the hashed `/tmp/mt-<hash>` fallback; `manager.go:349,366`).
  After tenant B spawns, uid(B) owns the 0700 dir holding `acme.sock`, so it can
  `unlink` it and bind its own listener; the root router then proxies acme's
  traffic — `Authorization` headers and session tokens included — into tenant B.
  Nothing hides the dir: only `PackagesDir` is bind-mounted. *(Independently
  re-verified while writing this doc.)* **Fix:** per-org socket directory
  (`<root>/run/<slug>/`), chown only that.
  → This is the isolation model's core claim. It fails on the very host type
  §1's boundary argument is about.

### 7.2 🟠 High — security

- **[V] WebDAV `PUT`-of-a-new-file and `MKCOL` evaluate no rule at all.**
  `core/webdav/filesystem.go:263-345`, `file.go:186-205`. `ruleFor()` has only
  List/View/Update/Delete arms; `persistWrite` authorizes only when
  `f.existing != nil`; `Mkdir` goes straight to `NewRecord` + `Save`. This is the
  same "`CanAccessRecord` cannot authorize a CREATE" problem §3.7 records for
  CalDAV — **CalDAV solved it** with save-in-transaction-evaluate-rollback
  (`caldav/backend.go:345-367`); WebDAV was never updated, so it resolves the
  problem by skipping the check. Consequence: **a disabled user can still create
  folders and upload files over WebDAV**, because the disabled clause on create
  lives only in the `createRule` this path never reads. Reads/updates/deletes
  *are* gated, which is exactly why §6's live verification passed.
  **Test blind spot:** both fixtures in `filesystem_test.go:143-176` set a
  permissive `CreateRule`, and the comment claims the tests would go red — true
  for four of five rules.
- **[V] DAV Basic-Auth never checks `users.disabled` — a third auth path nobody
  audited.** `core/davauth/davauth.go:26-52` resolves the user and calls
  `ValidatePassword`, nothing more. `disabled_guard.go` binds PB's *token*
  hooks, which a Basic-Auth request never traverses, and token-key rotation is
  irrelevant to it. So a suspended account keeps **CardDAV** (no PB rules at
  all — only owner scoping) and **CalDAV** (calendar ships no disabled clause)
  access with email + password. §6's "two gates, both required" is therefore
  incomplete: there is a third door, and `davauth` **has no test file at all**.
  Two reviewers found this independently. **Fix here covers all three DAV
  protocols at once** — the highest-leverage single fix in this review.
- **[V] Drive's disabled-clause migration silently reopened the guest-create
  hole.** `drive/pb-migrations/1782000000_exclude_disabled_from_drive.js:27`
  *restates* `drive_items.createRule` as
  `@request.auth.id != "" && @request.auth.disabled != true`, dropping the
  `@request.auth.role != "guest"` clause that `1781300000` had installed as an
  explicit security fix. Every fresh DB reopens it; OTP guests hold real auth
  tokens and there is no Go backstop. Its own comment cites the wrong
  predecessor ("appended to 1716200001"). The RLS suite stayed green because its
  constants mirror the pre-drift rule — **the live proof of the fixture trap.**
- **[V] The `commentor` role diverges between the two halves of the "one
  definition", in opposite directions.** Added by `1781100000`, written for real
  by the OTP flow (`endpoints_share_otp_test.go:488`).
  - *PB rules over-grant:* `canEdit` uses `role ?!= "viewer"`, so a commentor can
    rename/replace a shared item via REST `PATCH` and WebDAV PUT-overwrite/MOVE
    — contradicting `1781100000`'s own contract ("can read and comment … cannot
    edit it"). Privilege escalation.
  - *Go under-grants:* `driveshare.rank()` has no `commentor`, returns 0, so
    `ResolveRoleForItem` → `ErrNoAccess` denies a commentor **even read** on
    every driveshare-gated path (download tokens, text/calc render + realtime
    admission). The pre-lift predicate admitted them, so commit `5307491`'s
    message ("aligns the Go with the collection rules") is false for this role —
    the lift **regressed** commentor read.
  - Net user-visible absurdity: **a signed-in commentor has strictly less access
    than an anonymous visitor on the same share link** (`authorizeAnonShare`
    accepts `sharelink.RoleCommentor`). Zero commentor test cases anywhere.

### 7.3 🟠 High — correctness / live-broken features

- **[V] An org is permanently 502'd after Evict-then-traffic (the Deploy path).**
  `orgmanager/instance.go:159` unconditionally `os.Remove(i.sockPath)` after
  drain(10s)+kill(5s). `socketPath(slug)` is deterministic, so a `Get` inside
  that window spawns a replacement that clears the stale file and binds the
  *same* path — and up to 15s later the old teardown deletes the **new** child's
  socket. Every dial then ENOENTs → 502, while the supervisor still considers the
  instance healthy, so nothing respawns it until the 30-minute idle sweep. No
  test covers traffic after an Evict. *(Independently re-verified.)*
- **[V] In-app bell notifications are dead app-wide.**
  `core/components/NotifyContextSync.tsx:13-20` gates on
  `if (!userId || !orgId) return`, but `useOrgInfo()` now returns `orgId: ''`
  unconditionally — so the context is never set, `bellChannel.dispatch` no-ops,
  **and it fires `captureException('notify.bell.no_context')` on every dispatch**
  (Sentry noise on a dead path). Takeout import completion/failure never produces
  a notification row. The unit suite certifies the bug: `bell.test.ts:22` calls
  `setNotifyContext` directly, bypassing the dead sync. This makes §6's
  "`useOrgInfo()` returns `org: null`" **worse than described** — it is not just
  missing branding, it silently killed a feature. Fix: drop `orgId` from
  `NotifyContext`, gate on `userId`.
- **[V] Share links never redirect signed-in members into the workspace.**
  `core/lib/anon-identity.ts:42,72` types `ShareSession.orgSlug` as `string`, but
  the server stopped sending `org_slug` (`drive/server/endpoints_share_session.go:60-71`,
  `endpoints_public_share.go:156-168` — their comments say "there is no slug").
  `drive/…/public-screens/share/[token].tsx:65-72` gates the member redirect on
  `data?.org_slug`, always falsy, so members fall through to the public preview;
  the redirect target is a dead `/a/` route anyway. Fix core (delete the field —
  its only consumer ignores it) and drive (gate on `item_id` → `/drive?file=…`).
- **[V] Every org's auth emails carry dead links.** Nothing sets a tenant's
  `Settings().Meta.AppURL` (`grep AppURL` in the router → zero hits), so PB's
  default `http://localhost:8090` interpolates into `{APP_URL}` for
  verification, password-reset and email-change templates. The router knows
  `MT_BASE_DOMAIN` and the slug but materializes only carddav/caldav/webdav/quota.
- **[V] Hard navigation to `/drive` hits the WebDAV mount, not the app.** The
  de-slugged SPA routes (`app/(app)/drive/…`) now collide with
  `webDAVSource.Prefix = "/drive"`, whose literal `Router.Any("/drive")` +
  `Any("/drive/{path...}")` beat the SPA catch-all. `davauth` accepts only Basic,
  so a reload or a pasted link yields 401 + a native Basic-Auth popup. The dev
  proxy collides independently (`scripts/dev.ts:555`; its comment at :545 still
  claims `/a` ownership). **Migration-created** — `/a/<org>/drive` never
  collided. Invisible to e2e because every spec navigates by SPA click.
- **[S] Non-root Linux hosts can't spawn any tenant at all.**
  `spawn_linux.go:101` sets `CLONE_NEWNS|CLONE_NEWPID` unconditionally; both need
  `CAP_SYS_ADMIN`. The uid/chown block *is* gated on `UIDBase > 0`, the namespace
  block is not — so `NewSpawner`'s "Tenants are NOT confined" warning promises a
  degraded-but-working mode that instead fails every spawn with
  `operation not permitted` → every org 503s.
- **[V] mail's "Add domain" writes a field that does not exist, and the type
  system endorses it.** `settings/provider.tsx:552` inserts `org: orgId` into
  `mail_domains` — no migration defines that field (all 24 checked; generated
  `pbSchema.ts:210-224` agrees), and `orgId` is always `''`. It compiles because
  the package-local mirror `types.ts:29` declares `org: string`, which also feeds
  `MailSchema`, so `org` looks filterable on every `mail_domains` query — the
  §3.2 compiler-blind class, now in a *mirrored client schema*. Go test fixtures
  carry the same phantom field (`imap_fetcher_test.go:148-149`,
  `aliases_test.go:40,205`), so a future `org`-filtered query would pass in tests
  and return zero rows live. Dead scaffolding to remove with it:
  `provider.tsx:38,50,86,124,530`.
- **[V] Commit `4d52992` (username-derived mailbox addresses) is unguarded and
  half-applied.** `deriveMailboxAddress` (`server/lifecycle.go:123-148`) — the
  sole producer of primary mailbox addresses — has **no unit test at all**, and
  its edge cases are live: the numeric-suffix loop caps at `i<=99` and returns
  `""`, which `handleUserCreated:32-36` turns into "log a warning and create no
  mailbox" (the 100th `bob` silently gets no mail); a unicode username sanitizes
  to `""` and does the same. Meanwhile `seed.ts:1663-1670,1729` still derives the
  address from the **email** local-part — the TS mirror of the exact bug the
  commit fixed in Go — so seeded dev/e2e users get a different address than the
  server would provision. Help contradicts the change too
  (`help/mailboxes.md:10` still says "derived from your account email").
- **[S] calendar's member-authz enforcement lives in Go that a tenant never
  runs.** `calendar_members` `createRule` is `@request.auth.id != ""` and
  `updateRule` admits `user = @request.auth.id`; the real gates (owner-only
  create, self-promote/repoint guard, last-owner guard) are request hooks in
  `server/register.go:257-362` — **feature Go, which `serve-org` does not link
  today**. A
  tenant serves stock PB REST with those rules, so any authenticated tenant user
  could `POST calendar_members {calendar: <any>, user: self, role: "owner"}` and
  take any calendar, including over the tenant's CalDAV. Single-tenant is safe
  (hooks run; proven by `calendar_members_authz_test.go`). The relaxation was
  forced by a real PB back-relation bug, but this is exactly the
  **"never put an enforcement boundary in Go"** rule §3.5 exists to prevent, and
  the tenant consequence is nowhere flagged.

### 7.4 🟡 Medium

*Isolation / router*
- **[V] `chownTree` never chmods, so one org's `pb_data` stays mode-readable by
  other tenant uids** (`spawn_linux.go:110`). Org dirs are 0755
  (`provisioning.go:65`), PB creates `pb_data` 0755 and SQLite files 0644; the
  only `Chmod` in the repo is the socket. That is the `ATTACH DATABASE` read the
  boundary claims to close. End-to-end exploit `[S]` (depends on WAL `-shm`
  access); the missing mode restriction is unambiguous.
- ~~**[V] A single failed spawn counts as two crashes**~~ **FIXED 2026-07-27
  (P4-2):** the supervisor waits for the instance's fate (`published`/`closed`)
  before accounting; interval asserted by test.
- ~~**[V] The child gets a 10s drain budget it can never use**~~ **FIXED
  2026-07-27 (P4-3):** `killTimeout = drainTimeout + 5s`, relationship pinned
  by test.
- **[V] `Deploy` re-materializes the *running* tenant's `pb_public`/`pb_hooks`
  before evicting it** (`provisioning.go:134`; `Materialize` does RemoveAll +
  recreate), so the live tenant 404s on static assets during that window and the
  whole drain. Evict-first, or materialize to a temp dir and rename.
- ~~**[V] A `webdav` manifest block with no `prefix` mounts a site-wide catch-all
  or panics**~~ **FIXED 2026-07-27 (P4-5):** defaults to the reserved
  `/dav/<slug>`; malformed and duplicate prefixes (WebDAV and CalDAV) fail the
  load naming the packages.
- ~~**[V] The proxy drops the client IP twice over.**~~ **FIXED 2026-07-27
  (P4-6):** rightmost-entry contract + `ForwardedConfig` from MT_TLS_MODE;
  TrustedProxy materialized via `.runtime/app.json` and adopted at tenant boot.
  Verified end-to-end through the real binary (`e.realIP()`).
- ~~**[V] `evalManifest` runs untrusted package JS with no interrupt or
  timeout**~~ **FIXED 2026-07-27 (P4-7):** `vm.Interrupt` on a 5s deadline.

*Authorization / data*
- **[V] Neither comments collection carries the disabled clause.**
  `text/pb-migrations/1720000000:~78-84` and `calc/…/1719000000:93-100` — a
  disabled user with surviving share rows can still list, view **and create**
  comments via REST; the Go gate never runs for `/api/collections/*_comments`.
  §6's audit concluded the only open rule was `mail_mailbox_aliases`; it missed
  both, and comment bodies are content, not metadata.
- **[V] Same two migrations omit the `created_by` disjunct** that drive_items and
  `driveshare` honor, so an item creator with no share row can open and edit the
  doc but sees zero comments and cannot post one. calc's migration comment claims
  "Mirror drive_items access" while omitting it.
- **[V] drive's disabled clause reached only three collections.**
  `drive_item_versions` (restorable **file content**; its viewRule gates blob
  access) and `drive_share_links` have none, and the versions rules also lack
  `created_by` and use `role ?!= "viewer"` (commentor can write versions — the
  same class as §7.2). Masked today only by token rotation, i.e. the single-gate
  reliance `1782000000`'s own comment forbids.
- **[V] WebDAV existence-masking is read-side only** (`filesystem.go:364,395,324,399`):
  `RemoveAll`/`Rename` return 403 and `Mkdir`/`Rename` return `ErrExist` for
  records the caller cannot see, so DELETE/MOVE/MKCOL probes confirm another
  user's paths exist while `Stat`/read carefully answer 404.
- **[V] `carddav.PutAddressObject` evaluates no PB rule on any path**
  (`backend.go:152-196`) — self-consistent for an owner-scoped collection today,
  but a future contacts rule change (say, the disabled clause) would silently not
  apply to CardDAV.
- **[S] calendar subscription sync swallows every error and can silently empty a
  second subscription** (`subscription.go:183,193,200`): `_ = app.SaveNoValidate(…)`
  throughout, while migration `1715100000`'s unique `ical_uid` index is **global**
  though the contract (`source.go:95`) is per-calendar — two calendars on one feed
  means the second's inserts are discarded and the sync still reports success. The
  create path also skips `Event.Defaults`, persisting `visibility=""` that a later
  validated save rejects as a 500.
- **[S] Setting a `subscription_url` on a populated calendar deletes all its
  events** (`subscription.go:197-202`) — sync deletes every event whose `ical_uid`
  isn't in the feed, and UI-created events all have generated UIDs. No
  confirmation, no error.
- **[S] calendar's member list/delete rules are self-only**
  (`1715000000:265-271`), so the "Shared with" UI cannot list or remove other
  members. Faithful translation of main's semantics, not a de-org regression —
  but the e2e conspicuously only ever asserts the owner's own row.
- **[V] The audit-log "Members" filter can never match** — it filters
  `resource_type = 'user_org'` (`app/(app)/settings/audit-log.tsx:29`) while the
  writer stamps `"users"` (`users_guard.go:82`).
- **[V] Accept-invite renders "Welcome to " with an empty name** — the client
  still expects `orgName`/`orgSlug` the Go handler no longer sends
  (`invite.go:278-280`). e2e passes on a loose `/Welcome to/i`.
- **[V] Admin-initiated disable never rotates the token key**
  (`account_delete.go:159-188`, `users_guard.go:41-47`), so an admin suspending a
  compromised account leaves every existing session live until JWT expiry —
  while *self*-disable is immediate. §6 documents the trade-off for self-disable
  only.
- **[V] Takeout counts dropped records as imported.**
  `batch-inserter.ts:185,348` — the `if (!calendarId) return` / `if (!mailboxId)
  return` skip paths don't compensate the `imported: 1` that `insertRecords:72`
  adds, so a failed parent calendar silently reports all its events as imported.
- **[V] The demo reset leaks `realtime_doc_updates` forever**
  (`scripts/reset-demo.ts:62-76`): the row has no FK (text `room_id`), nothing
  cascades, and per-room truncation never fires for a deleted room.
- **[V] A failed mail search is indistinguishable from an empty inbox — on both
  sides.** `endpoints_search.go:298-301,388-392` turns a SQL error into
  `HTTP 200 {"items":[],"total":0}` after a `Warn`; `useMailSearch.ts:125` sets a
  local `error` that **no consumer reads** and never captures. This is precisely
  the mechanism that let §3.2's `ts.user_org` bug present as a silent
  `{"total":0}`. The field itself is now correct (`ts.user`), but the swallow
  that hid it is untouched, and no test covers `buildFolderJoin`'s SQL against a
  real schema — so the same class of bug would hide again identically.
- ~~**[V] mail's five-query JS-stitched join.**~~ **FIXED 2026-07-27 (P4-12):**
  all three sites resolve their relations with joined live queries; mounted-hook
  tests execute the real joins (verified red by neutering).
- **[V] mail swallows several user-visible failures.** `EmailBody.tsx:41`
  (`.catch(() => setHtml(''))` — a failed body fetch renders as an empty email);
  `useSaveDraft.ts:52-54` (no `captureException`, unlike its `useSendEmail`
  sibling); `useAttachments.ts:53-62` (toasts, never captures);
  `useMailBulkActions.ts` (no `onError` at all, so a bulk action failing across
  N threads is entirely silent).
- **[V] IMAP multi-term BODY search silently ORs.** `imap_session.go:312-321` —
  both arms of the "intersect for subsequent terms" if/else are byte-identical,
  so `SEARCH BODY "a" BODY "b"` returns messages matching *either*, against the
  comment's promise of AND.

*Tests that cannot fail* — each `[V]`, and each one guards something this review
found broken or would need to catch.

> **Repaired 2026-07-27 (Phase 3), and the list below was INCOMPLETE.** Fixing
> these surfaced two more instances the review missed, both of which had been
> hiding a live bug: calendar's `member_create_rule_probe_test.go` (its
> "owner adds a member" case adds the owner to a calendar they already own,
> so it passed on a rule that made sharing impossible) and takeout's whole
> e2e spec (orphaned — no `playwright.config.ts`, so it had never run). The
> generalizable point: **a test written specifically to certify a security
> rule is not exempt from this class — it is a prime candidate for it**,
> because its author is thinking about the attack, not the feature. When
> repairing one of these, check that the positive control exercises the shape
> the FEATURE uses, not merely a shape the rule admits. Three of the denials
> written during this repair were themselves vacuous on first draft and only
> caught by neutering the guard.
- `webdav/filesystem_test.go:143-176` — permissive `CreateRule` in both fixtures
  (masks §7.2's create hole).
- drive `guest_rls_test.go` / `disabled_rls_test.go`, text/calc
  `comments_rls_test.go:273-291`, calendar `guest_rls_test.go:37` +
  `calendar_members_authz_test.go:38`, core `coreserver/guest_rls_test.go:33-37`,
  core `caldav/backend_test.go:53-58` — all assert rule strings **re-declared as
  constants in the test file**, never the shipped migration. All currently match
  byte-for-byte (each was diffed), so these are drift tripwires that don't trip —
  and drive's already didn't.
  **mail is the exception and the model to copy:** its `guest_rls_test.go`
  constants are byte-identical to the shipped migrations *and* every deny-test
  has a paired positive control, so neutering the guard turns the deny-tests red
  while the controls stay green. That is §3.6 applied correctly. Port that shape
  to the other six.
- router `tenant_e2e_test.go:201` (`TestTenant_DoesNotInheritHostSecrets`) — the
  hook reads `process.env`, which the fork's `scrubProcess` empties on every
  sandboxed VM; swapping `cmd.Env` for `os.Environ()` would leave it green. The
  real check is the never-run `TestConfinement_ChildEnvironmentHoldsNoHostSecrets`.
- router `confinement_linux_test.go:152-175` (`…PackageStoreIsReadOnly`) — writes
  to `/tmp`, not the package store, and `$os` is withheld by the sandbox anyway,
  so deleting `confinePackages` entirely wouldn't turn it red.
- router `manifest_test.go:203,469,488` — `reflect.DeepEqual` round-trips built
  from the same mirrors, so a field missing from **all three** definitions
  compares equal (zero == zero) and the tenant silently loses the config.
- router `carddav_integration_test.go:125,134` — the cross-org "leak" assertion
  cannot fail (separate DBs, separate processes, Bob only ever inserted into one).
- router `integration_test.go:193` — asserts only `err != nil`, so a filename
  typo keeps it green with `Sandboxed` removed.
- **mail `endpoints_inbound_test.go:129,189,200-209` — the sharpest instance in
  the review.** The shared inbound fixture declares
  `mail_mailbox_members.user_org` and `mail_thread_state.user_org`; production
  reads `member.GetString("user")` and writes `Set("user", userID)`, and the
  shipped migration names both fields `user`. So the tests build a schema that
  does not exist, and `TestHandleInbound_KnownRecipientStoresMessage` /
  `_IdempotentRetry` **pass while thread state is written keyed to `""`** — i.e.
  they would stay green through a total failure to deliver mail to the
  recipient. The reviewer ran both verbosely to confirm. The fixture is shared by
  `imap_fetcher_test.go` and `smtp_inbound_server_test.go`. This is §3.7's
  takeout lesson verbatim: *a package that mirrors a schema needs a test that
  asserts the field NAME, or its suite certifies the bug.*
- **mail's folder counts: 13 tests guard a function the app no longer calls.**
  `computeMailboxFolderCounts.test.ts` + `unifiedInbox.test.ts:67-120` cover a
  helper that survives only as a re-export; the real counts come from the
  `mail_folder_counts` view (`useMailboxFolderCounts.ts:23-52`), which has **zero
  coverage** — not its column names, not `eq(counts.user, userId)`, not the
  realtime bridge. The sidebar could break entirely with every test green.
  Structural cause: **no vitest file in mail mounts a hook**, so no live-query
  shape in the package is tested at all. Also `mailListHelpers.test.ts:132-168` —
  three `as any` stubs carry 5 of 8 fields, so a field rename stays green, and
  the cast is load-bearing.
- takeout — the mirrored-schema field-name guard covers **one** of nine-plus
  foreign collections; every other write is `pb`-mocked. (Current field names
  were hand-verified against the owning repos' migrations: no live drift.)
- `package-scripts/tests/*` (3 files, 12 tests) — **orphaned from every runner**;
  the workspace-root vitest globs point at paths that no longer exist, so a root
  run collects 1 file and reports green. They pass when forced.
- **`TestConfinement_*` do not skip on darwin — they don't exist.** The file is
  `//go:build linux`, so `-run TestConfinement` prints "no tests to run" and
  exits 0. README:164 and §4 both describe a `t.Skip` that never executes.

### 7.5 ⚪ Low / cleanup

- **Docs that now mislead.** `~/code/tinycld/CLAUDE.md` and
  `tinycld/CONTRIBUTING.md` still teach the multi-org contract: the `user_org`
  junction, `/a/<orgSlug>` routes, `getRoleForOrg`, and `OrgScope` as
  `{orgId, userOrgId, orgSlug}` (shipped: `{userId}`). CLAUDE.md:115 cites
  `OrganizationsTab.tsx` as the reference joined-query example — it is now a
  static stub with no query. `docs/packages.md` has the same drift.
- **This file.** §2/§5.4 say the fork "must stay checked out" at
  `~/code/tinycld/pocketbase` for the `replace ../pocketbase` — `go.mod:11`
  actually resolves `../tinycld/third_party/pocketbase` (commit `a37c8ac`); the
  router no longer builds that checkout at all. §5.6 claims the two esbuild call
  sites are "kept in sync by a golden test" — **no such test exists** (both sides
  assert properties independently, no shared fixture). They *are* in sync today
  (same loader/target/sourcemap, both pin esbuild v0.28.1). §4's confinement-skip
  claim is wrong per above.
- **Router README:** the diagram claims each tenant gets a "netns" — there is no
  network namespace anywhere (`CLONE_NEWNET` appears nowhere); reserved
  subdomains are listed as open but are fixed at `provisioning.go:41`;
  `davconfig`/`serve-org` are still described as CardDAV-only.
- **User-facing help still teaches multi-org.** `core/help/organizations.md` is
  entirely about the deleted org switcher and `/a/<slug>` paths, and names roles
  ("admin / clerical / workforce") that match neither the shipped
  `owner/admin/member/guest` nor anything else in the tree; it is linked from
  `getting-started.md:24`. `core/help/super-admins.md:11-13` advertises org
  management; `help/account-settings.md` has **no coverage of the new
  disable/delete flows**, so by the project's own standard the account-lifecycle
  feature isn't done. contacts' `help/carddav.md:26` documents a per-org book at
  `/carddav/u/ab/<orgSlug>/` (actual: `/carddav/u/ab/default/`). mail's
  `help/provider-setup.md:90,94-102` documents provider config stored at
  `(app='mail', org=<orgId>)` plus an entire "Per-org fallback" section — all
  deleted storage, contradicted by `register.go:59-61` ("the provider is
  deployment-wide") — and `:78` promises "one worker per org" against a single
  deployment-wide fetcher.
- **Silent-failure residue.** `use-share-visitor-role.tsx:92-95` — the filter fix
  landed, but the bare `catch { return null }` that *hid* the original bug is
  still there, so any error still resolves to "member". drive
  `ShareDialog.tsx:181` swallows a failed share-save and closes the dialog as if
  it succeeded. takeout's `DocumentPicker` promise has no `.catch`, and its dedup
  lookups treat any rejection as "not found" (a transient error creates
  duplicates).
- **Dead / lying code.** contacts `scripts/test-server-api.ts` is unrunnable
  (expands the deleted `user_org_via_user.org`, wrong CardDAV path) and its
  `fail()` discards every label, so it emits nothing but an exit code. Router:
  `orgs.custom_domains` is written and never read; `ContentHash`/`manifest` still
  have no writers (confirms §6); package names from a lockfile reach a filesystem
  path unvalidated, so `"../../.."` escapes the store root (superuser-only).
  `davconfig/webdav.go:13-19` still describes tenant WebDAV as unauthenticated-
  broad — §6 marks that **closed**, so the comment now invites a redundant "fix".
  Comment rot referencing the deleted junction persists in ~12 core/text/calc/
  drive sites; text and calc both cite a "public share-link render endpoint" that
  is registered nowhere. mail's `mergeSharedFolderStates.ts` has zero production
  importers but keeps a 3-test suite, and `useThreadListItems.ts:136-383` calls
  plain `users.id` values `userOrgIdsForFilter` with a comment about
  "the relevant user_orgs" — that file is the one a reader opens to understand
  thread scoping, which is exactly how §3.2's class survives.
- ~~**Residual N+1s in mail**~~ **FIXED 2026-07-27 (P4-13):** Login batches via
  `FindRecordsByIds`, the FTS thread arm uses one IN-subquery, the deletion
  sweep uses one DISTINCT query (and no longer deletes on a query error).
- **e2e discipline.** contacts' positive assertions are still bare
  `getByText('Alice')` (the deny-side ones are correctly testID-scoped) — the
  collision class §3.6 predicts "as more packages return"; calc and calendar have
  the same shape. mail is the worst instance: `mail-inbox.spec.ts:136-146`
  asserts five advanced-search labels are absent from the **entire page**
  (`getByText('Size', {exact:true})).toHaveCount(0)`), so drive rendering a
  "Size" column fails a test about mail's search dropdown;
  `mail-shared-mailbox-admin.spec.ts:87` matches `/already|unique|in use|exists/i`
  anywhere in the DOM, and `mail-labels.spec.ts:14` walks a hardcoded
  `ancestor::*[5]`. `helpers.ts:143` hard-`goto`s `/settings/members` against the
  discipline the same file documents; `invite-flow.spec.ts:140` asserts
  `url()).toContain('/')`, which is vacuous. takeout's spec uses `page.goto` for
  in-app nav plus inline 10s–120s timeouts.
- **Duplication worth lifting.** `webdav/tshooks_register.go` and
  `caldav/tshooks_register.go` are ~130 near-identical lines (`normalizeHandlerSource`
  and `isKnownHookName` byte-identical) — and have **already drifted**: caldav
  rejects unknown hook names *before* compiling, webdav after, so a typo
  partially registers. text/calc `authorizeAnonShare` is byte-identical; the
  router builds the tenant binary from two duplicated test helpers.
- **§6 corrections.** The "`userOrgId` wired to server contracts (~45 sites)"
  item is **stale and better than described**: the contracts are already user-id
  based (`/api/invite-link/${userId}`, no `?user_org_id=` anywhere), leaving ~8
  pure naming-residue sites — a mechanical rename, no API moves. And
  `bootstrapTenantOnce` may be **removable rather than relocatable**:
  `apis.Serve` already runs `RunAllMigrations()` unconditionally inside the
  confined tenant (`apis/serve.go:155`), so the first spawn applies the same
  migrations in isolation; dropping it closes the item without building a
  one-shot subprocess path.
- **Cosmetic.** Two drive migrations share the `1782000000` prefix (ordering
  rests on lexicographic filename sort). `biome.json` excludes still target
  `app/a/[orgSlug]/…`. `time.NewTicker(MaxIdle/2)` panics for `MaxIdle == 1ns`.
  The socket is chmod'd 0600 only *after* `net.Listen` creates it at 0755.
  ~15 `biome-ignore` comments in text/calc against the "never" rule (all carry
  rationales). CardDAV re-runs bcrypt per backend call; DAV auth is timing-
  distinguishable and unrated-limited.

### 7.6 Verified-good — don't re-audit these

Recorded so a later pass doesn't spend the effort again:

- **mail's image-proxy SSRF guard is the strongest in the tree**
  (`endpoints_image_proxy.go:34-94`): token-gated, scheme allowlist, pre-flight
  private-host check, redirect re-validation capped at 3 hops, and a
  `safeDialContext` that **pins the dialed IP** to one that passed validation —
  DNS rebinding closed — plus a 10MB `io.LimitReader`. calendar's ICS fetcher
  has the same pinned-dial shape with regression tests.
- **mail's webhook authz fix is present and guarded** (5 tests covering
  owner/admin 200 vs member/guest/no-role 403-without-secret); inbound and bounce
  compare secrets with `subtle.ConstantTimeCompare` and resolve an unknown secret
  to `NoopProvider`. FTS input is sanitized and every dbx param is bound.
- **text's embed IDOR fix is carried and neuter-sensitive**: drive_items
  allowlist + per-record `driveshare.CheckRead` + empty-auth fail-closed +
  traversal rejection, with four denial tests each carrying a bytes-bearing decoy
  plus a positive control.
- **calendar's CalDAV lift is in good shape**: `Defaults` covers the only two
  required-no-default fields (verified against the migration), the `OnError`
  decorator wraps the *interface* with a compile-time assertion so a new protocol
  method breaks the build, creates use save-evaluate-rollback, every denial is a
  `webdav.NewHTTPError(404, …)` with `errors.Is` intact, and the five wire-format
  tests genuinely round-trip real iCalendar bytes in both directions.
- **`core.App` widening (§3.4) is complete in mail** — `*pocketbase.PocketBase`
  survives only at `register.go:29`, the one spot §3.4 says to keep.
- **The `ts_hooks` seam is correct**: `RegisterExtras` runs before
  `jsvm.MustRegister` (§5.10), the fast path is one atomic load, both
  `filterList` implementations *intersect* rather than trust the handler's
  return, and every TS hook point is narrowing-only — a hook can hide but never
  reveal.
- **Core's org-era sweep is genuinely clean**: zero live `user_org` / `orgs` /
  `org_provisioning` references in `core/server`, every filter string resolves
  against the shipped migrations, and `userorg`'s contents are all still live
  despite the vestigial package name.

**One false positive, recorded so it isn't re-raised:** the `author_user_org`
field in `drive/pb-migrations/1781200000` is *not* a surviving lying field —
`1781400000_drop_drive_preview_comments.js` drops the whole collection
unconditionally (verified; its down-migration deliberately throws). Reading the
app shell's assembled migration set makes dropped collections look live.

### 7.7 Verification — every gate, green

| Scope | Result |
|---|---|
| `multi-org` | `go build` / `vet` / `test ./... -count=1` **PASS**; `-race` **PASS**; `GOOS=linux` build + `test -c` **PASS** |
| `core/server` | build / vet / `test ./...` **PASS** (21 pkgs; `davauth` has no test file) |
| app shell `server/` | build / vet / test **PASS**; `pnpm run typecheck` **PASS** |
| `tinycld` (core TS) | `pnpm run checks` **PASS** (10 biome warnings, all in calc/text; 1 schema-version info); core vitest **477 tests PASS** |
| contacts | Go **PASS** (no test files); `tinycld-pkg check` **PASS** (20) |
| takeout | `tinycld-pkg check` **PASS** (23 — the suite is 23 now, not §3.7's 22) |
| mail | Go **PASS** (+`-count=1`); `tinycld-pkg check` **PASS** (127 files, 138 tests) |
| drive | Go **PASS**; `tinycld-pkg check` **PASS** (97) |
| text | Go **PASS**; `tinycld-pkg check` **PASS** (920) |
| calc | Go **PASS**; `tinycld-pkg check` **PASS** (1378) |
| calendar | Go **PASS**; `tinycld-pkg check` **PASS** |

e2e and dev servers were deliberately **not** run (parallel reviewers would
collide on ports — §5.3); every e2e finding above is from reading the specs.

### 7.8 What to do first

> **Superseded 2026-07-27.** Items 1–4 below shipped (verified against code —
> see REMEDIATION-PLAN.md's reconciliation notes; that file is the canonical
> tracker). The current order, from the verified-open set:
>
> 1. ~~**P1-7** — `carddav.PutAddressObject` evaluates no PB rule~~ **done** —
>    shipped in core `cb1fec3` (`saveAuthorized` + `backend_authz_test.go`),
>    verified green 2026-07-27; the tracker had drifted.
> 2. ~~**P2-4** — tenant `AppURL`~~ **done 2026-07-27** — materialized as
>    `.runtime/app.json`, adopted (persisted) as `Meta.AppURL` at tenant boot;
>    `TestTenant_AdoptsMaterializedAppURL` covers it through the real binary.
> 3. ~~**The verified-open mail batch**~~ **done 2026-07-27** — P3-2 first
>    (fixture renamed to the shipped `user` fields + delivery assertions,
>    confirmed red against the phantom schema), then P2-5, P2-7, P2-13, P2-6.
> 4. ~~**P5-1** — Linux CI~~ **authored 2026-07-27** —
>    `.github/workflows/confinement.yml` (sibling checkouts + sudo run of
>    `TestConfinement_*`); needs a push to run, verify the first run. P3-9's
>    darwin skip stub landed with it.
> 5. ~~P3-1's remainder / P4-1 / P4-4~~ **done 2026-07-27** — drive/text/calc
>    were already converted (tracker drift); calendar + coreserver guest
>    suites converted to rlstest and verified by neutering. P4-1 fixed at BOTH
>    unlink sites (teardown inode guard + the child's `SetUnlinkOnClose`);
>    P4-4 fixed via atomic symlink-generation swap in `materialize`.
>
> **PHASE 3 IS COMPLETE (2026-07-27), so Phases 0–3 — the entire merge gate —
> are done.** P3-3, P3-4, P3-5, P3-7 and P3-8 all landed this pass; see
> REMEDIATION-PLAN.md for per-item evidence. Every gate is green in every
> repo (router build/vet/test, core's 21 packages, all seven members'
> `tinycld-pkg check`, `pnpm run checks`, `pkg:check` 582 tests) and the
> touched e2e suites were actually RUN, not just read.
>
> **Making the tests capable of failing found SIX live bugs that every green
> suite had been hiding — which is the whole thesis of §7 confirmed
> empirically.** Each was fixed with a regression test verified red first:
>
> 1. **Calendar sharing was impossible.** `1830000004`'s
>    `user = @request.auth.id` conjunct meant the only creatable membership
>    was your own, so an owner adding a teammate got a bare 400. Its own
>    probe test certified it by having the owner add *themselves*. Fixed by
>    `1830000006` (see P1-5's regression note — read it; it is the sharpest
>    fixture-trap instance in the tree).
> 2. **Every real Drive takeout import failed** with "unexpected EOF" —
>    fflate's streaming `Unzip` misparsed the PK headers of embedded
>    docx/pptx archives inside data-descriptor entries. The unit suite used
>    synthetic `zipSync` fixtures, which carry local-header sizes and so
>    never exercise the scanner. Now iterates the central directory, with a
>    test that streams the REAL fixture bytes.
> 3. **Bad DAV Basic credentials returned 500 with no `WWW-Authenticate`**,
>    so clients read a wrong password as a server fault and never re-prompted.
>    CardDAV authenticated in the backend; CalDAV/WebDAV already did it at the
>    route. Found by a falsifiable cross-org probe added under P3-4.
> 4. **Core never mapped direct PocketBase field errors to forms** —
>    `error.response.data` IS the field map, but `extractValidationErrors`
>    only looked at `data.data`, so every validation failure became a generic
>    "Something went wrong" toast.
> 5. **`login()` was not idempotent**, deadlocking any spec that composed
>    `createInvitedUser` with it. It presented as a 30s timeout — the failure
>    mode a bumped budget would have "fixed" while leaving the deadlock.
> 6. **mail's e2e delivered to an address the server no longer mints**
>    (`user@tinycld.org` vs the username-derived `tester@tinycld.org`), so
>    every inbound 403'd.
>
> Two of these (1 and 2) shipped broken to users; the suite was green
> throughout. Note also that takeout's e2e spec was **orphaned** — the package
> shipped no `playwright.config.ts`, so `tinycld-pkg test:e2e` could not run
> it at all, which is why bug 2 survived.
>
> **Next, after the gate:** ~~Phase 4~~ **PHASE 4 IS COMPLETE (2026-07-27)** —
> the router cluster (P4-2, P4-3, P4-5, P4-6, P4-7, P4-8, P4-11 — the last
> also closed P6-3's README items) and the mail cluster (P4-12 joined live
> queries, P4-13 batched N+1s); see REMEDIATION-PLAN.md for per-item
> evidence. Remaining: P5-2…P5-4, and Phase 6 docs — where P6-1
> (CLAUDE.md/CONTRIBUTING still teach the deleted org contract) ranks above
> its severity.

The original list, kept for the review record:

1. **The socket-dir chown** (§7.1) — one-line class of fix, breaks the whole
   isolation claim until done.
2. **`davauth` disabled check** (§7.2) — one place, closes CardDAV + CalDAV +
   WebDAV at once.
3. **WebDAV create authorization** (§7.2) — port CalDAV's
   save-evaluate-rollback; then delete the permissive `CreateRule` from the
   test fixtures so the gap can't reopen.
4. **Restore the guest clause** in drive's `1782000000` (§7.2) and settle
   `commentor` in **one** place — decide whether it may edit, then make the rule
   and `driveshare.rank()` agree.
5. **The evict/respawn socket race** (§7.3) — data-plane outage on the Deploy
   path.
6. **The dead/broken features** (§7.3): bell notifications, the share-link
   member redirect, tenant `AppURL`, the `/drive` route collision, and mail's
   phantom `org` write. Each is small and each is currently shipping broken.
7. **Finish `4d52992`** (§7.3): test `deriveMailboxAddress` (including the
   `i<=99` exhaustion and unicode-username paths that silently produce no
   mailbox), fix `seed.ts` to derive from username, correct the help topic.
8. **Then the fixture traps** (§7.4). Every one of them is a test that will not
   tell you when the fix above regresses. Re-point them at the shipped
   migrations rather than re-declaring the strings, and copy **mail's
   `guest_rls_test.go` shape** (paired positive controls) — it is the only suite
   in the tree that would actually go red.

**A note on sequencing.** Items 1–5 are security or data-plane; 6–7 are
user-visible breakage. But item 8 is what keeps the rest fixed: six of the eight
findings above were *already* covered by a test that passed. Fixing the code
without re-pointing the fixture leaves the next regression equally invisible.

---

## 8. Design docs

- **Specs/plans** (this repo, `docs/superpowers/`): TypeScript hooks
  (`2026-07-23-typescript-hooks*`), tenant sandbox
  (`2026-07-23-tenant-jsvm-sandbox*`), multi-org CardDAV
  (`2026-07-23-carddav-multi-org.md`), process isolation
  (`FOLLOWUP-os-process-isolation.md`).
  Note `FOLLOWUP-os-process-isolation.md` is the *pre-implementation* brief; its
  seven deferred decisions are answered by the shipped code except #6 (resource
  limits) and #7 (control-plane provisioning). `README.md` describes what
  actually exists.
- **Fork seams + router design** (2026-07-22): in the fork's git history.
- **Core capability/hook reference**: `tinycld/docs/hooks.md` (both Go↔TS seams,
  incl. the hook-point contract and its ordering trap), `tinycld/docs/packages.md`.
- **WebDAV hook points, administrator-facing**: `drive/help/webdav-hooks.md`.
- Approved plans: `~/.claude/plans/{streamed-orbiting-pretzel,rustling-popping-babbage,rosy-weaving-blossom,tidy-napping-lighthouse}.md`
  (the last is per-process isolation).
