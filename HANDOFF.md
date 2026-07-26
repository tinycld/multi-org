# Handoff — Multi-Org PocketBase Router

**Updated:** 2026-07-25
**Goal:** one router hosts many organizations — each org its own **OS process**,
SQLite DB, client bundle, and server-side JS, sharing versioned code on disk but
isolated at the kernel boundary.

This is a working brief, not a changelog. Full narrative history is in the git
log of this file and of the four repos.

---

## 1. Where things stand

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

Under isolation this splits cleanly: **feature Go stays out of tenants**, but
*core* libraries can link into `serve-org` (it is the trusted core process for
that org). CardDAV and WebDAV do exactly this — driven by declarative config the
router materializes, never by feature code with full `$app` reach.

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

**Three caveats, all live:**
1. **`TestConfinement_*` require Linux + root and there is no CI**, so on a macOS
   dev box the security property is asserted by construction only. Closing this
   is the highest-value next step.
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
| `~/code/tinycld/pocketbase` | `feat/multitenant-fork` | **Must stay checked out here** — the router's `replace ../pocketbase` needs both seams + sobek |
| `~/code/tinycld/multi-org` | `multi-org` | The router. No remote. |
| `~/code/tinycld/tinycld` | `multi-org` | App shell + `@tinycld/core` (nested at `tinycld/core`) |
| `~/code/tinycld/{contacts,mail,drive,text,calc,calendar,google-takeout-import}` | `multi-org` | Migrated features (all seven) |

Nothing is pushed. `multi-org/` and `pocketbase/` are gitignored by the parent
and are **not** pnpm members.

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
# Linux+root only and SKIP everywhere else. Cross-compile at minimum:
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
- **Linux CI for `TestConfinement_*`** (§1). Per-process isolation has shipped,
  but the tests that prove it need Linux + root and never run today. Until they
  do, the boundary is verified by construction only. This is now the top item.
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
- **Tenant VMs still get no `$` bindings and no hook points.** `serve-org` sets
  neither `OnInit` nor `OnLoaderInit`, because reaching them means importing
  `coreserver`, which drags Sentry, webpush, postmark and go-message into the
  tenant binary — the process the isolation model most wants small. Needs a
  narrow bindings-only package that both `coreserver` and `serve-org` can share.
  Until then, `webdavHook` and `$drive.*` work single-tenant only.
- CardDAV **and CalDAV** now run **inside** each tenant process (core libs,
  `core.App`-driven, fed by `<orgDir>/.runtime/carddav.json` and `caldav.json`).
  CalDAV followed the **WebDAV** shape, not CardDAV's: a `Source` field map plus
  four opt-in TS hook points (`beforeWrite`, `beforeDelete`, `canRead`,
  `filterList` — no `beforeMove`, since CalDAV has no cross-calendar move). Like
  `webdavHook`, `caldavHook` works **single-tenant only** until tenant VMs get
  bindings. IMAP/SMTP are the remaining case:
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
- Give `multi-org` a remote if it should be shared/CI'd.

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
- **Other packages' collection rules.** Audited, and the gap is small: drive was
  the one that mattered (it owns shared content). `contacts.listRule` is
  `owner = @request.auth.id` and mail's message rules are `user ?= @request.auth.id`,
  so a disabled user reaches only their OWN rows there, never anyone else's.
  The single genuinely-open rule is `mail_mailbox_aliases.listRule`
  (`@request.auth.id != ""`, migration 1713000014) — alias metadata rather than
  message content, so low severity, but it should get the same
  `@request.auth.disabled != true` clause for consistency.

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

## 7. Design docs

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
