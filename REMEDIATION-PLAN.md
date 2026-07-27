# Remediation plan — 2026-07-26 final review

Companion to `HANDOFF.md` §7. Every finding in §7.1–§7.5 appears here exactly
once, assigned to a phase. Phase order is by **risk of shipping without it**,
then by dependency.

> **Status reconciliation — 2026-07-27.** The checkboxes below had drifted
> from the code: every Phase 0 item and most of Phase 1 were implemented but
> never checked off. Each item marked `[x]` below was re-verified against the
> shipped code/migrations on 2026-07-27 (evidence noted inline). Items left
> `[ ]` were re-verified as genuinely open, except where noted "unverified".

**How to work this doc.** Phases 0–3 are merge blockers. Each item carries its
§7 severity, the repo to commit in, and a **Done when** line that names the
verification — a fix without one is not finished. Check items off in place and
add the PR link, the way `REVIEW-TODO.md` was worked.

> **The discipline that matters.** Six of the eight most serious findings were
> already covered by a passing test. So: **for every code fix in Phases 0–3,
> write the failing test FIRST and confirm it goes red against current `HEAD`.**
> If it passes before your fix, you have written another fixture that certifies
> the bug. That is the whole lesson of this review and it is not negotiable.

**Repos.** `multi-org` (router), `tinycld` (app shell + `core`), and the seven
feature siblings — each its own git repo under `github.com/tinycld/`, each
needing its own PR. All have remotes and `multi-org` is pushed and in sync, so
nothing is stranded.

---

## Decisions — RESOLVED 2026-07-26

All three are answered. Recorded here because each determines *what* the fix is,
not merely how it is written.

- **D1 — May a `commentor` edit? → NO.** A commentor reads and leaves comments;
  it never edits. This confirms `1781100000`'s written contract, so **both halves
  of the divergence are wrong and both change**: the PB rules currently
  over-grant (`role ?!= "viewer"` admits commentor to update), and the Go
  under-grants (`rank()` returns 0, denying even read). See **P0-5**.
- **D2 — Is calendar in scope for tenant hosting? → YES; move the gates into
  rules.** Option (b) (declare it single-tenant-only) is rejected. The
  owner-only-create, self-promote/repoint and last-owner guards must become
  collection rules, because a tenant links no feature package. This is the larger of
  the two options and it is now a **Phase 1 blocker**, not a deferral. See
  **P1-5**, rewritten accordingly.
- **D3 — `multi-org` remote → it already has one; the HANDOFF was stale.**
  Verified: `origin` is `git@github.com:tinycld/multi-org.git`, `origin/multi-org`
  exists, and local is **0 ahead / 0 behind** — everything is pushed, nothing is
  stranded. All eight sibling repos likewise have remotes. **Phase 5's Linux CI
  is therefore unblocked today.** §2's "No remote. Nothing is pushed." is
  corrected in `HANDOFF.md`.

---

## Phase 0 — Stop the bleeding (merge blockers, security)

Nothing merges to a shared branch until these are done. Each is small; the
grouping is by "an attacker or a disabled user gets something they must not".

### P0-1 🔴 Per-org socket directory — `multi-org`
**DONE — verified in code 2026-07-27:** `socketPath` (manager.go) builds `<root>/run/<slug>/<slug>.sock`, per-org dir 0700 under a 0711 parent; hashed fallback same shape.
The one **critical**. `spawn_linux.go:115` chowns the *shared* socket dir to each
tenant's uid, so the last tenant to spawn owns the directory holding every other
org's socket and can unlink-and-rebind one to intercept its traffic.

- Change `socketPath` (`manager.go:349,366`) to `<root>/run/<slug>/<slug>.sock`
  and the temp fallback to `mt-<hash>/<slug>/`, both `0700`.
- `MkdirAll` the **per-org** dir, chown only that, never its parent.
- Re-check the ~104-byte `sun_path` ceiling (§5.12) — the extra segment eats
  budget; `maxSocketPath` logic must still hold, and the fallback must still fit.
- **Done when:** a Linux test spawns two tenants under distinct uids and asserts
  tenant B cannot `stat`/`unlink` tenant A's socket **and** that the parent run
  dir is still root-owned. Must fail against current `HEAD`.

### P0-2 🟠 `davauth` checks `users.disabled` — `tinycld` (core)
**DONE — verified in code 2026-07-27:** `davauth.Authenticate` rejects disabled users; `davauth_test.go` + `ratelimit_test.go` exist.
Highest leverage in the review: one function closes CardDAV + CalDAV + WebDAV.
`davauth.Authenticate` (`davauth.go:26-52`) validates the password and nothing
else, so a suspended account keeps DAV access with email + password.

- Reject when `record.GetBool("disabled")` after `ValidatePassword`.
- **`davauth` has no test file at all** — create one. Cover: enabled user
  succeeds; disabled user is refused; wrong password is refused.
- **Done when:** the new test fails against `HEAD` and passes after, and a live
  smoke (§4) confirms a disabled user gets 401 on `/carddav`, `/caldav`,
  `/drive`.

### P0-3 🟠 WebDAV authorizes creates — `tinycld` (core)
**DONE — verified in code 2026-07-27:** `filesystem.go` save-evaluate-rollback (`RunInTransaction` + `CanAccessRecord` against `CreateRule`) on PUT-new and MKCOL.
`PUT`-of-new-file and `MKCOL` evaluate **no rule**, so `createRule` — the only
place the disabled and guest clauses live for creates — is never read.

- Add a `ruleCreate` arm to `ruleFor()` (`filesystem.go:122-134`).
- Port CalDAV's proven shape (`caldav/backend.go:345-367`): save inside
  `RunInTransaction`, evaluate, roll back on refusal. `CanAccessRecord` filters
  on `id = record.Id` and so cannot authorize an unsaved record (§3.7).
- Apply to both `Mkdir` (`filesystem.go:309-345`) and the `existing == nil`
  branch of `persistWrite` (`file.go:186-205`).
- **Delete the permissive `CreateRule` from both fixtures** in
  `filesystem_test.go:143-176` — it is what masks this — and add
  `TestCreateDeniedByCreateRule`, mirroring caldav's
  `TestPutCalendarObject_ViewerCannotCreate`.
- **Done when:** the new test fails against `HEAD`; a disabled user's WebDAV PUT
  and MKCOL are both refused live.

### P0-4 🟠 Restore drive's guest-create clause — `drive`
**DONE — verified in code 2026-07-27:** migration `1782100000_restore_guest_clause_and_settle_commentor.js`.
`1782000000_exclude_disabled_from_drive.js:27` restated `createRule` and dropped
the `@request.auth.role != "guest"` clause that `1781300000` added as an explicit
security fix. Every fresh DB reopens the hole.

- New migration (do **not** edit `1782000000` — it has shipped to real DBs):
  set `createRule` to `@request.auth.id != "" && @request.auth.role != "guest"
  && @request.auth.disabled != true`.
- Fix the comment that cites the wrong predecessor (`1716200001`).
- **Done when:** a guest create is denied, an ordinary member's create still
  succeeds (positive control), and both assertions read the rule from the
  **shipped migration** rather than a test constant (see P3-1).

### P0-5 🟠 Settle `commentor` in one place — `drive` + `tinycld` (core)
**DONE — verified in code 2026-07-27:** same migration settles the rules per D1 (commentor reads + comments, never edits); `driveshare.RoleCommentor` ranks readable.
**D1 resolved: a commentor may read and comment, never edit.** Both halves of
the divergence are wrong, so both change. Do them in one PR pair — fixing only
one side leaves the contradiction, just pointing the other way.

- *Rules — stop over-granting.* `role ?!= "viewer"` currently admits commentor to
  UPDATE, so they can rename/replace a shared item via REST `PATCH` and WebDAV
  PUT-overwrite/MOVE. Replace it with an explicit `role ?= "editor" || role ?=
  "owner"` (plus the `created_by` disjunct) in a **new** drive migration — do not
  edit `1782000000`, it has shipped. Audit every rule using that idiom:
  `drive_items` **and** `drive_item_versions` (fold into P1-3, same migration).
- *Go — stop under-granting.* Add `RoleCommentor` to `driveshare` ranked between
  viewer and editor, teach `rank()` (`driveshare.go:100-111`), and admit it in
  `CheckRead` but **not** in the write checks. Today `rank()` returns 0, so a
  commentor is denied even **read** on every driveshare-gated path — download
  tokens, text/calc render, realtime admission — which is why a signed-in
  commentor currently has *less* access than an anonymous visitor on the same
  link.
- *Comments must stay reachable.* A commentor's whole purpose is commenting, so
  verify they can still create `text_comments`/`calc_comments` after P1-1/P1-2
  tighten those rules — the two changes meet here, and it is easy to lock the
  commentor out of the one thing they are for.
- Update `driveshare.go:10-14`, whose doc comment omits both this role and the
  disabled clause.
- **Done when:** table-driven `driveshare_test.go` cases cover commentor for read
  (allow), **edit (deny)**, and realtime admission (allow); a REST `PATCH` as
  commentor is refused; a comment create as commentor succeeds; and a signed-in
  commentor reaches a shared doc live. Every one must fail against `HEAD` —
  today's suite has **zero** commentor cases.

### P0-6 🟠 Admin disable rotates the token key — `tinycld` (core)
**DONE — verified in code 2026-07-27:** `users_guard.go` calls `RefreshTokenKey()` on the admin disabled-flip path; self-disable path unchanged (`account_delete.go`).
Self-disable rotates (`account_delete.go:151`); the admin path and the
`adminEditableUserFields["disabled"]` record-update path do not, so an admin
suspending a compromised account leaves every session live until JWT expiry.

- Call `RefreshTokenKey()` on the admin disable path and on the guarded field
  update (`users_guard.go:41-47`).
- Mirror the §6 note: re-enable forces a fresh sign-in. That trade is already
  accepted for self-disable; make the admin copy say so too.
- **Done when:** a test asserts an admin-disabled user's *existing* token is
  rejected on the next request.

---

## Phase 1 — Close the remaining authorization gaps

Same class as Phase 0, lower reachability. Land immediately after.

- [x] **P1-1 🟡 Disabled clause on text + calc comments** **DONE — verified 2026-07-27:** migration `1782200000_comments_disabled_and_creator.js` in both repos. — `text`, `calc`.
  Neither `text_comments` nor `calc_comments` carries
  `@request.auth.disabled != true`, so a disabled user with surviving share rows
  can list, view **and create** comments via REST; the Go gate never runs for
  `/api/collections/*_comments`. New migration in each repo.
  **Done when:** deny-tests for all three verbs, each with a positive control.
- [x] **P1-2 🟡 `created_by` disjunct on those same rules** **DONE — verified 2026-07-27:** same `1782200000` migration. — `text`, `calc`.
  Both omit the disjunct drive_items and `driveshare` honour, so an item creator
  with no share row can open and edit the doc but sees zero comments and cannot
  post one. Fold into P1-1's migration. Fix calc's comment claiming it mirrors
  drive_items while omitting it.
- [x] **P1-3 🟡 Disabled clause on `drive_item_versions` + `drive_share_links`** **DONE — verified 2026-07-27:** covered by `1782100000` (versions + share_links rules restated with the disabled clause and creator disjunct). —
  `drive`. Versions carry restorable **file content** and their viewRule gates
  blob access; both were missed by `1782000000`. Versions rules also lack
  `created_by` and use the `role ?!= "viewer"` idiom (fix with P0-5).
- [x] **P1-4 🟡 Calendar's disabled gap** **DONE — verified 2026-07-27:** `tenant_rules_authz_test.go` asserts `@request.auth.disabled != true` on all three collections' five verbs, reading the shipped migrations. — `calendar`. No calendar rule anywhere
  carries the clause, and it owns *shared* content via memberships. P0-2 covers
  the CalDAV path; this covers REST. New migration adding the clause to
  calendars, events, and members.
- [x] **P1-5 🟠 Move calendar's member-authz gates into rules** — `calendar`.
  **DONE 2026-07-27** (migration `1830000004` + a bootstrap pb-hook). But see
  `docs/FINDING-tenant-composition-gap.md`: this fix treats a symptom. The root
  cause was that `serve-org` bound none of CORE's guards either, so every
  Go-held guard had the same exposure. **That root cause is now FIXED**
  (2026-07-27): `serve-org` composes via `coreserver.RegisterTenant`, so core's
  guards (users field guard, disabled-user guard, org-pkg guard) run in every
  tenant, with a parity test preventing silent re-divergence.
  `docs/SCOPE-tenant-feature-go.md` **landed later the same day**: calendar's
  FEATURE Go now links into tenants (pinned menu, gated by the org's package
  set), so the interim pb-hook was deleted — the Go bootstrap hook
  (`registerOwnerMembershipBootstrap`) covers both compositions and
  `tenant_hooks_bootstrap_test.go` binds it the way a tenant runs it. The
  rules from `1830000004` remain the tenant's authorization backstop.
  **D2 resolved: calendar IS in scope for tenant hosting, so the gates must
  become rules.** A tenant links no feature package, so today a tenant user could
  `POST calendar_members {calendar: <any>, user: self, role: "owner"}` and take
  any calendar, including over the tenant's CalDAV. The largest item in Phase 1
  — treat it as its own PR.

  **Start by re-testing the constraint that caused this.** `1715400000` relaxed
  `createRule` to `@request.auth.id != ""` because **PB v0.36** evaluated the
  original back-relation rule inconsistently (non-superuser POSTs always 400'd
  even for a genuine owner):
  ```
  calendar.calendar_members_via_calendar.user ?= @request.auth.id
      && calendar.calendar_members_via_calendar.role ?= "owner"
  ```
  The tree now runs a **v0.39.8 fork**, and §7 separately verified that the
  events editor rule's two `?=` conditions on one relation path *do* evaluate
  correctly today (one membership row must satisfy both). So **first write a
  test that restores the original rule and attempts an owner-authored create.**
  If it passes, the blocker is gone and the fix is largely reverting
  `1715400000`. If it still 400s, escalate — a tenant-safe calendar then needs
  a schema change (e.g. a denormalized `owner` column on `calendar_members`
  that a hook maintains and the rule compares directly), not a cleverer rule.

  Three properties must end up in rules, not Go — port each from
  `register.go:257-362` and `registerCalendarMemberAuthz`:
  1. **Owner-only member create** (the hook above).
  2. **No self-promotion / repointing** — the field-scoped guard blocking a
     member from raising their own `role` or moving their row to another
     `calendar`. PB rules cannot constrain *which fields* a write touches, so
     this likely needs the update rule plus a narrowed `updateRule` field set,
     the same shape `users_guard.go` uses for `disabled`.
  3. **Last-owner protection** (delete + demotion). This one may be genuinely
     inexpressible as a rule; if so, say so explicitly in the migration comment
     and accept that a tenant can orphan a calendar — a much smaller blast
     radius than self-promotion, and it must be a *recorded* decision rather than
     an oversight.

  Keep the Go hooks as defence-in-depth for the single-tenant app; they are not
  the problem, they are just not sufficient alone.

  **Correction (2026-07-27):** the constraint here is that `serve-org` links no
  feature package *today* — it is build wiring, not an architectural ban on
  feature Go in tenants. The actual rule is narrower: **a service that must BIND
  A PORT moves into core so the router can open the port.** Performance-sensitive
  work stays in Go; TS/JS hooks are the customization seam. So "port it into
  rules" is one option; linking the package's Go into the tenant is a legitimate
  alternative this item should weigh rather than assume away.
  **Done when:** each of the three has a deny-test driven through the **rule
  engine with no hooks bound** (mirroring how a tenant runs), each failing
  against `HEAD`; whatever cannot be expressed is documented in the migration and
  in §6.
- [x] **P1-6 🟡 WebDAV write-verb existence masking** **DONE — verified 2026-07-27:** write verbs mask (`filesystem.go`: an entry the viewer may not read is invisible, not forbidden, on the write paths too). — `tinycld` (core).
  `RemoveAll`/`Rename` return 403 and `Mkdir`/`Rename` return `ErrExist` for
  invisible records, so DELETE/MOVE/MKCOL probes confirm another user's paths
  exist while reads correctly 404. Mask with `canRead`-then-`ErrNotExist` like
  the read verbs. Note the tension: the `(parent, name)` namespace is globally
  unique, so a create into an occupied invisible name must still fail somehow —
  prefer a generic conflict over a distinguishable one.
- [ ] **P1-7 🟡 `carddav.PutAddressObject` evaluates no rule** — `tinycld` (core).
  Self-consistent today (owner-scoped collection) but a future contacts rule
  change silently would not apply. Add rule evaluation on both create and update,
  reusing P0-3's transaction helper.
- [x] **P1-8 ⚪ Store path traversal** **DONE — verified 2026-07-27:** `store.validRef` rejects non-npm-shaped names/versions and `.`/`..` segments; `VersionDir` validates before pathing. — `multi-org`. Package names from a
  lockfile or publish body reach `filepath.Join` unvalidated
  (`lockfile.go:48` → `store.go:21`), so `"../../.."` escapes the store root.
  Superuser-only, hence low, but it is two lines: apply a `validSlug`-style regex.
- [x] **P1-9 ⚪ DAV auth hardening** **DONE — verified 2026-07-27:** `davauth` rate-limits (`TooManyFailures`, refused before bcrypt) and burns a `dummyHash` compare on unknown users so they are not timing-distinguishable. — `tinycld` (core). Timing-distinguishable
  username enumeration (miss short-circuits before bcrypt) and **no rate limiting
  on any DAV path**, which makes online guessing viable. Compare against a dummy
  hash on the miss path; add a rate limit. Also: CardDAV re-runs bcrypt per
  backend call (2+ per operation, amplified by PROPFIND) — cache the
  authentication for the request.

---

## Phase 2 — Fix what is shipping broken

User-visible breakage. No security exposure, so it follows Phase 1 — but each of
these is live right now.

- [x] **P2-1 🟠 `/drive` route collision** **DONE — verified 2026-07-27:** drive's WebDAV moved to the reserved `/dav/drive` prefix (manifest + Go Source) and `isDavPath` no longer claims `/drive`. — `tinycld` + `drive`. Hard navigation
  (reload, pasted link) to `/drive`, `/drive/recent`, `/drive/<path>` reaches the
  Basic-Auth WebDAV mount and yields a browser auth popup; the SPA is
  unreachable. Literal routes beat the SPA catch-all. **Migration-created** —
  `/a/<org>/drive` never collided. Move the DAV mount to a reserved prefix
  (`/dav/drive` or `/_webdav`), update `webDAVSource.Prefix`, the manifest block,
  `help/webdav-hooks.md`, and the dev proxy (`scripts/dev.ts:555`, whose comment
  at `:545` still claims `/a` ownership). Reserve the prefix so no package slug
  can claim it.
  **Done when:** an e2e does a **hard reload** on `/drive` and lands in the app —
  today's specs navigate by SPA click and cannot catch this.
- [x] **P2-2 🟠 Bell notifications are dead app-wide** **DONE — verified 2026-07-27:** `NotifyContextSync` gates on `userId` only; its comment records this exact bug. — `tinycld` (core).
  `NotifyContextSync.tsx:13-20` gates on an `orgId` that `useOrgInfo()` now
  always returns as `''`, so the context is never set, every dispatch no-ops,
  **and each one fires `captureException('notify.bell.no_context')`** — Sentry
  noise on a dead path. Takeout completion/failure never notifies. Drop `orgId`
  from `NotifyContext` and gate on `userId` alone. Rewrite `bell.test.ts:22`,
  which currently calls `setNotifyContext` directly and so certifies the bug.
  **Done when:** a test drives the real mount path, not a hand-set context.
- [x] **P2-3 🟠 Share links never redirect signed-in members** **DONE — verified 2026-07-27:** `org_slug` deleted from `anon-identity.ts`; drive's `share-routing.ts` owns the member redirect without it. — `tinycld` (core)
  + `drive`. `ShareSession.orgSlug` is typed `string` but the server stopped
  sending `org_slug`, so the redirect gate is always falsy and members fall
  through to the public preview; the target is a dead `/a/` route anyway. Core:
  delete `orgSlug` from `ShareSession`/`SessionResponse` (its only consumer
  ignores it). Drive: gate on `item_id`, redirect to `/drive?file=…`.
- [ ] **P2-4 🟠 Tenant `AppURL` is never set** — `multi-org`. **More urgent
  since 2026-07-27:** feature-Go linking means tenants now run mail's Go and
  core's invite/password-reset mailers, so these emails can actually SEND once
  an org has mail creds — carrying `http://localhost:8090` links. Every org's
  verification, password-reset and email-change links point at
  `http://localhost:8090`. Materialize the org's public URL from
  `MT_BASE_DOMAIN` + slug into `.runtime/`, and have `serve-org` set
  `Settings().Meta.AppURL` at boot. **Done when:** a tenant's rendered auth email
  contains the org's real URL.
- [ ] **P2-5 🟠 mail's phantom `org` field** — `mail`. `provider.tsx:552` writes
  `org: orgId` into `mail_domains` — no migration defines it, and `orgId` is
  always `''`. It compiles because the local mirror `types.ts:29` declares it,
  which also makes `org` look filterable on every `mail_domains` query. Remove
  the write, the type field, the dead scaffolding
  (`provider.tsx:38,50,86,124,530`), the fixture uses
  (`imap_fetcher_test.go:148-149`, `aliases_test.go:40,205`) and the test factory
  (`useSendableIdentities.test.ts:22`).
- [ ] **P2-6 🟠 Finish commit `4d52992`** — `mail`. The username-derived address
  change is unguarded and half-applied. (a) Test `deriveMailboxAddress`
  (`lifecycle.go:123-148`) — it has **none** — covering the `i<=99` exhaustion
  path and a unicode username, both of which currently produce **no mailbox at
  all** behind a log line. Decide whether that silent outcome is acceptable; it
  should probably be a hard error. (b) Fix `seed.ts:1663-1670,1729`, still
  deriving from the email local-part — the exact bug the commit fixed in Go — so
  seeded users get a different address than the server provisions. (c) Fix
  `help/mailboxes.md:10`.
- [ ] **P2-7 🟡 Mail search failures are invisible** — `mail`. Server turns a SQL
  error into `HTTP 200 {"items":[],"total":0}`; client sets an `error` no
  consumer reads and never captures. This is the mechanism that let §3.2's
  `ts.user_org` bug present as a silent zero. Return a real status from the
  server; surface and capture on the client. **Done when:** a forced query error
  produces a visible failure state, not an empty list.
- [ ] **P2-8 🟡 Calendar subscription data loss** — `calendar`. Two `[S]`
  findings, both needing a live repro first. (a) Setting `subscription_url` on a
  populated calendar deletes every local event (sync removes anything whose
  `ical_uid` is absent from the feed, and UI events all have generated UIDs) —
  no confirmation, no error. (b) The unique `ical_uid` index is **global** while
  the contract is per-calendar, so a second calendar on the same feed silently
  stays empty and still reports success; the create path also skips
  `Event.Defaults`, persisting `visibility=""` that a later validated save
  rejects as a 500. Stop swallowing (`subscription.go:183,193,200` are all
  `_ = app.SaveNoValidate(…)`), scope the index per calendar, apply Defaults.
- [x] **P2-9 🟡 Audit-log Members filter** **DONE — verified 2026-07-27:** `audit-log.tsx` filters `resource_type='users'`, matching the writer. — `tinycld`. Filters
  `resource_type = 'user_org'`; the writer stamps `"users"`. One-word fix.
- [ ] **P2-10 🟡 Accept-invite shows "Welcome to "** — `tinycld`. Client expects
  `orgName`/`orgSlug` the handler no longer sends. Either send a deployment name
  or drop the interpolation. Tighten the e2e, which passes on a loose
  `/Welcome to/i`.
- [x] **P2-11 🟡 Takeout counts dropped records as imported** **DONE — verified 2026-07-27:** skip paths compensate with `{skipped: 1, imported: -1}` (`batch-inserter.ts`). — `takeout`. The
  two early-return skip paths don't compensate the `imported: 1`, so a failed
  parent calendar reports all its events as imported. Also: the `DocumentPicker`
  promise has no `.catch`, and dedup lookups treat any rejection as "not found"
  so a transient error creates duplicates.
- [ ] **P2-12 🟡 Demo reset leaks `realtime_doc_updates`** — `tinycld`. No FK, so
  nothing cascades and per-room truncation never fires for a deleted room —
  unbounded growth. Add it to the per-collection wipe.
- [ ] **P2-13 🟡 IMAP multi-term BODY search ORs** — `mail`. Both arms of the
  "intersect for subsequent terms" branch are byte-identical, so
  `SEARCH BODY "a" BODY "b"` matches either. Implement the intersection.
- [ ] **P2-14 🟡 mail's swallowed failures** — `mail`. `EmailBody.tsx:41` renders
  a failed body fetch as an empty email; `useSaveDraft` doesn't capture (its
  `useSendEmail` sibling does); `useAttachments` toasts without capturing;
  `useMailBulkActions` has no `onError` at all, so a bulk action failing across N
  threads is silent.
- [ ] **P2-15 ⚪ Silent-failure residue elsewhere** — `tinycld` (core), `drive`.
  `use-share-visitor-role.tsx:92-95` still has the bare `catch { return null }`
  that *hid* the original bug — narrow it to a 404 and capture otherwise.
  `ShareDialog.tsx:181` swallows a failed share-save and closes as if it worked.

---

## Phase 3 — Make the tests capable of failing

**The most important phase for the long run.** Everything above can regress
silently until this lands. Work it immediately after Phase 2 — or in parallel by
a second person, since it barely touches the same lines.

- [ ] **P3-1 🟡 RLS suites must read the shipped migrations.**
  **PARTIAL (verified 2026-07-27):** `core/rlstest` exists and calendar's
  suites use it (`tenant_rules_authz_test.go` is the model); drive's
  `guest_rls_test.go` and text/calc `comments_rls_test.go` still re-declare
  their rule strings as constants. Seven suites
  assert rule strings re-declared as constants in the test file: drive
  `guest_rls_test.go` + `disabled_rls_test.go`, text/calc
  `comments_rls_test.go`, calendar `guest_rls_test.go` +
  `calendar_members_authz_test.go`, core `coreserver/guest_rls_test.go`, core
  `caldav/backend_test.go`. All match byte-for-byte today, so they are tripwires
  that do not trip — **and drive's already failed to** (P0-4).
  **Copy mail's `guest_rls_test.go` shape**, the one suite in the tree that would
  actually go red: constants byte-identical to the shipped migration *plus* a
  positive control paired with every deny-test. Better still, load the rule from
  the migration so drift is impossible.
  **Done when:** for each suite, neutering the guard in the migration turns
  deny-tests red while controls stay green. Verify by actually neutering.
- [ ] **P3-2 🟡 mail's inbound fixture declares a schema that does not exist** —
  `mail`. The sharpest instance in the review: the fixture declares
  `mail_mailbox_members.user_org` and `mail_thread_state.user_org` while
  production reads and writes `user`, so `TestHandleInbound_KnownRecipientStoresMessage`
  and `_IdempotentRetry` **pass while thread state is written keyed to `""`** —
  green through a total failure to deliver mail. Shared by `imap_fetcher_test.go`
  and `smtp_inbound_server_test.go`. Rename the fixture fields, then assert
  delivery actually lands on the recipient's state row.
- [ ] **P3-3 🟡 mail's folder counts have no coverage** — `mail`. 13 tests guard
  `computeMailboxFolderCounts`, which the app no longer calls; the real counts
  come from the `mail_folder_counts` view, which has **zero** coverage — not its
  column names, not the predicate, not the realtime bridge. Structural cause: **no
  vitest file in mail mounts a hook**, so no live-query shape in the package is
  tested. Delete the dead tests (and the dead re-export), add coverage for the
  view. Also `mailListHelpers.test.ts:132-168`: three `as any` stubs carry 5 of 8
  fields, so a rename stays green — the cast is load-bearing, remove it.
- [ ] **P3-4 🟡 Router tests that cannot fail** — `multi-org`. Four:
  `tenant_e2e_test.go:201` (reads `process.env`, which the fork empties on every
  sandboxed VM — swapping `cmd.Env` for `os.Environ()` would leave it green);
  `confinement_linux_test.go:152-175` (writes to `/tmp`, never touches the
  package store, and `$os` is withheld anyway — deleting `confinePackages`
  wouldn't turn it red); `manifest_test.go:203,469,488` (`DeepEqual` round-trips
  built from the same mirrors, so a field missing from **all three** definitions
  compares equal and the tenant silently loses config);
  `carddav_integration_test.go:125,134` (the cross-org leak assertion is
  unfalsifiable — separate DBs and processes, the name only ever inserted in
  one). Also `integration_test.go:193`, which asserts only `err != nil` and so
  stays green with `Sandboxed` removed.
- [ ] **P3-5 🟡 Takeout's mirrored-schema guard covers 1 of 9+ collections** —
  `takeout`. Every other foreign write is `pb`-mocked, so a rename in any owning
  package passes the whole suite. Current field names were hand-verified correct,
  so this is a guard gap, not live drift. Extend the field-name assertion to
  every mirrored collection.
- [x] **P3-6 🟡 `package-scripts` tests never run** **DONE — verified 2026-07-27:** workspace `vitest.config.ts` globs `package-scripts/tests/**/*.test.{ts,tsx}`. — `tinycld`. 12 tests
  orphaned from every runner: the workspace-root vitest globs point at paths that
  no longer exist, so a root run collects 1 file and reports green. They pass
  when forced. Fix the globs and the stale `CORE_DIR` alias.
- [ ] **P3-7 🟡 e2e assertions that another package can satisfy.** The collision
  class §3.6 predicted. mail is worst (`mail-inbox.spec.ts:136-146` asserts five
  labels are absent from the **entire page**, so drive rendering a "Size" column
  fails a mail-search test; `mail-shared-mailbox-admin.spec.ts:87` matches four
  common words anywhere in the DOM; `mail-labels.spec.ts:14` walks a hardcoded
  `ancestor::*[5]`). contacts' positive assertions are bare `getByText('Alice')`
  though its deny-side ones are correctly scoped; calc and calendar the same.
  Scope everything to row/test IDs.
- [ ] **P3-8 ⚪ e2e discipline** — `tinycld`, `takeout`. `helpers.ts:143`
  hard-`goto`s `/settings/members` against the discipline the same file
  documents; `invite-flow.spec.ts:140` asserts `url()).toContain('/')`, which is
  vacuous; takeout's spec uses `page.goto` for in-app nav plus inline 10s–120s
  timeouts and a `[style*="width: 360"]` selector.
- [ ] **P3-9 🟡 `TestConfinement_*` do not skip — they do not exist** —
  **PARTIAL (verified 2026-07-27):** on Linux without root there is now a real
  `t.Skip` (`confinement_linux_test.go:31`), and HANDOFF §4 documents the
  darwin vacuous-pass honestly — but on darwin the tests are still not
  compiled at all, so the remaining ask is a darwin-visible skip stub (or
  P5-1's CI, which supersedes it). —
  `multi-org`. `//go:build linux` means `-run TestConfinement` prints "no tests
  to run" and exits 0 on darwin. Add a darwin stub file that **`t.Skip`s
  explicitly**, so the output tells the truth. (The real fix is Phase 5.)

---

## Phase 4 — Correctness, robustness, performance

Nothing here is a merge blocker. Grouped so one person can take a cluster.

*Router lifecycle* — `multi-org`
- [ ] **P4-1 🟡 Post-Evict socket unlink race.** A shut-down instance
  unconditionally removes the socket after drain+kill, but the path is
  deterministic, so a `Get` inside that window spawns a replacement that binds
  the same path — and up to 15s later the old teardown deletes the **new**
  child's socket. Every dial then ENOENTs → 502, while the supervisor still
  believes the instance is healthy, so nothing respawns until the 30-minute idle
  sweep. On the `Deploy` path. *(Independently re-verified.)* Guard the unlink on
  still-owning the path (compare inode, or skip when a live instance holds the
  slug). **Done when:** a test evicts, immediately serves traffic, and asserts
  the org stays reachable. **Note:** P0-1 changes the socket layout — do P0-1
  first, then this.
- [ ] **P4-2 🟡 One failed spawn counts as two crashes** (`manager.go:243` +
  `:455`), so backoff starts at 2s not the documented 1s, and a child the host
  itself killed logs at Error as "exited unexpectedly". Assert the *interval*,
  which no test does today.
- [ ] **P4-3 🟡 Drain budget the child can never use** — parent SIGKILLs 5s after
  SIGTERM while handing the child `--drain 10s`. Make the parent's patience
  exceed the child's budget.
- [ ] **P4-4 🟡 `Deploy` re-materializes a running tenant** before evicting it, so
  the live tenant 404s on static assets during that window and the whole drain.
  Evict first, or materialize to a temp dir and rename.
- [ ] **P4-5 🟡 WebDAV manifest prefix has no default or validation** — an empty
  prefix mounts a site-wide catch-all or panics. CalDAV has
  `defaultCalDAVPrefix` and two tests for exactly this; copy both. Reject
  duplicate prefixes across packages (today: a boot panic). Fold in P2-1's
  reserved-prefix decision.
- [ ] **P4-6 🟡 The proxy drops the client IP twice.** `SetXForwarded()` in
  Rewrite mode discards the inbound XFF chain and forces
  `X-Forwarded-Proto: http` — wrong under the documented default
  `MT_TLS_MODE=proxy`; and over a unix socket `RemoteAddr` is empty with no
  `TrustedProxy` materialized, so PB's per-IP rate limiting collapses to a single
  bucket. Carry the chain explicitly, set the scheme from the TLS mode, and
  materialize `TrustedProxy`.
- [ ] **P4-7 🟡 `evalManifest` has no interrupt** — `while(true){}` in a
  `manifest.ts` hangs the publish handler unrecoverably. Set `vm.Interrupt` and a
  deadline. The "pure object literal" comment is an assumption about input, not
  an enforced property.
- [ ] **P4-8 ⚪ `MaxIdle == 1ns` panics** the ticker; the socket is chmod'd 0600
  only *after* `net.Listen` creates it at 0755 (set the umask first);
  `buildCmd` takes an unused `ctx` that is an active trap for re-introducing
  §5.9's bug; `Signal` targets the pid while `Kill` targets the group, so
  graceful shutdown never reaches grandchildren.

*Isolation depth* — `multi-org`
- [x] **P4-9 🟡 `chownTree` never chmods** **DONE — verified 2026-07-27:** `chownTree` chmods 0600 files / 0700 dirs during the walk (`spawn_linux.go`)., so one org's `pb_data` stays
  mode-readable by other tenant uids — the `ATTACH DATABASE` read the boundary
  claims to close. Chmod dirs `0700` and files `0600` as you chown.
- [ ] **P4-10 🟠 Namespaces are set unconditionally**, so on a **non-root Linux
  host every tenant spawn fails** (`operation not permitted` → every org 503s),
  despite `NewSpawner` warning that promises a degraded-but-working mode. Gate
  the clone flags the way the uid block already is.
- [ ] **P4-11 ⚪ No network namespace** despite the README diagram claiming one.
  Either add `CLONE_NEWNET` or correct the diagram (P6). Defence-in-depth: `$http`
  is withheld by the sandbox today.

*Feature performance* — `mail`
- [ ] **P4-12 🟡 JS-stitched joins.** `settings/mailboxes.tsx:37-143` opens five
  unfiltered whole-collection live queries and hand-joins across four `Map`s
  though every relation is declared; same shape in `useMailboxes.ts` and
  `useSendableIdentities.ts`. Against CLAUDE.md's explicit rule.
- [ ] **P4-13 ⚪ Residual N+1s** — two queries per mailbox membership in `Login`;
  a per-thread-match query inside the FTS loop; a membership query per personal
  mailbox (up to 1000) on every user deletion. The per-*org* fan-out is confirmed
  gone; these are bounded in practice.

---

## Phase 5 — Prove the boundary (the §6 top item, now unblocked)

Do **not** start before P0-1 and P4-9/P4-10 land: standing up CI against a
boundary that is known-broken just encodes the break.

- [ ] **P5-1 Linux CI running `TestConfinement_*` as root.** **Unblocked** — the
  remote exists and everything is pushed (D3). The only real prerequisites are
  technical: two of these tests are vacuous even on Linux (P3-4), so repair them
  first or CI certifies less than it appears; and P0-1/P4-9/P4-10 must land or CI
  will faithfully encode a broken boundary. Needs a root-capable runner
  (privileged container or a VM), since the namespace and uid work requires
  `CAP_SYS_ADMIN`.
- [ ] **P5-2 Resource limits.** `MT_CGROUP_ROOT` creates a per-tenant cgroup but
  writes no limits, so a runaway tenant can still starve the host. (§6, brief
  decision #6.)
- [ ] **P5-3 Provisioning out of the control plane.** §7 refines this: it may be
  **removable rather than relocatable** — `apis.Serve` already runs
  `RunAllMigrations()` unconditionally inside the confined tenant, so the first
  spawn applies the same migrations in isolation. `bootstrapTenantOnce` mainly
  fails `CreateOrg` fast and flips status to `active`. Try deleting it and
  reporting migration failure through the readiness handshake before building a
  one-shot subprocess path.
- [ ] **P5-4 Live smoke per §4**, extended with what this review found: a
  disabled user against all three DAV protocols, a WebDAV **create** (not just an
  update), a hard reload of `/drive`, and a commentor on a shared doc.

---

## Phase 6 — Docs, dead code, duplication

Low risk, high readability value. Safe to hand to a fresh pair of hands.

- [ ] **P6-1 Instruction docs that now mislead.** `~/code/tinycld/CLAUDE.md` and
  `tinycld/CONTRIBUTING.md` still teach the `user_org` junction, `/a/<orgSlug>`
  routes, `getRoleForOrg`, and `OrgScope` as `{orgId, userOrgId, orgSlug}`
  (shipped: `{userId}`). CLAUDE.md cites `OrganizationsTab.tsx` as the reference
  joined-query example — now a static stub with no query. `docs/packages.md` has
  the same drift. **These are the files an agent reads first**, so this ranks
  above its severity.
- [ ] **P6-2 `HANDOFF.md` itself.** Already corrected: the fork replace path
  (§2/§5.4), the confinement-skip claim (§4), §5.6's nonexistent golden test, and
  §6's incomplete disabled-user audit. Re-read after Phase 0–2 land and mark the
  §7 items done.
- [ ] **P6-3 Router README** — the diagram claims a netns that does not exist;
  reserved subdomains are listed as open but are fixed; `davconfig` and
  `serve-org` are still described as CardDAV-only.
- [ ] **P6-4 User-facing help still teaches multi-org.**
  `core/help/organizations.md` is entirely about the deleted org switcher and
  names roles ("admin / clerical / workforce") matching nothing shipped — and it
  is linked from `getting-started.md`. `core/help/super-admins.md` advertises org
  management. `help/account-settings.md` has **no coverage of the disable/delete
  flows**, so by the project's own standard the account-lifecycle feature is not
  done. contacts' `help/carddav.md` documents a per-org book path; mail's
  `help/provider-setup.md` documents per-org provider storage and "one worker per
  org", both deleted.
- [ ] **P6-5 Dead and lying code.** contacts `scripts/test-server-api.ts` is
  unrunnable (expands a deleted junction, wrong CardDAV path) and its `fail()`
  discards every label, so it emits nothing but an exit code. Router:
  `orgs.custom_domains` is written and never read; `ContentHash`/`manifest` have
  no writers. `davconfig/webdav.go:13-19` still describes tenant WebDAV as
  unauthenticated-broad — §6 marks it **closed**, so the comment invites a
  redundant "fix". mail's `mergeSharedFolderStates.ts` has zero importers but a
  3-test suite. Dead statements in two router tests.
- [ ] **P6-6 Comment rot + naming residue.** ~12 core/text/calc/drive sites still
  reference the deleted junction; text and calc both cite a "public share-link
  render endpoint" registered nowhere; drive cites a superseded migration and
  says "four interception points" where there are five; mail's
  `useThreadListItems.ts` calls plain user ids `userOrgIdsForFilter` — the one
  file a reader opens to understand thread scoping, which is exactly how §3.2's
  class survives. Plus the ~8 `userOrgId` naming sites §6 overstated (the
  contracts are already user-id based; this is a mechanical rename).
- [ ] **P6-7 Duplication worth lifting.** `webdav/tshooks_register.go` and
  `caldav/tshooks_register.go` are ~130 near-identical lines and have **already
  drifted** — caldav validates unknown hook names before compiling, webdav after,
  so a typo partially registers. Fix that ordering bug, then lift to a shared
  `core/davhooks` parameterized on prefix + names. Also: text/calc
  `authorizeAnonShare` is byte-identical (plus four neighbours); the router
  builds the tenant binary from two duplicated test helpers.
- [ ] **P6-8 Cosmetic.** Two drive migrations share the `1782000000` prefix
  (ordering rests on lexicographic filename sort — rename one). `biome.json`
  excludes still target `app/a/[orgSlug]/…`. ~15 `biome-ignore` comments in
  text/calc against the "never" rule (all carry rationales — either waive them
  explicitly or fix the underlying issues). `members.tsx` casts through
  `unknown` because the users schema type lacks `username`/`role`/`is_demo`.
  Stale `/a/acme/...` fixture paths in unit tests.

---

## Coverage check

**64 items across 7 phases.** Every §7 finding is assigned exactly once:

| §7 section | Where it lands |
|---|---|
| 7.1 critical (1) | P0-1 |
| 7.2 high security (4) | P0-2, P0-3, P0-4, P0-5 |
| 7.3 high correctness (8) | P2-1…P2-6, P4-1, P4-10; calendar-tenant → P1-5 |
| 7.4 medium (24) | P0-6, P1-1…P1-7, P2-7…P2-14, P4-2…P4-7, P4-9, P4-12 |
| 7.4 tests-that-cannot-fail (11) | P3-1…P3-9 |
| 7.5 low / cleanup (10) | P1-8, P1-9, P2-15, P4-8, P4-11, P4-13, P6-1…P6-8 |
| §6 carried forward (3) | P5-1, P5-2, P5-3 |

Per phase: **P0** 6 · **P1** 9 · **P2** 15 · **P3** 9 · **P4** 13 · **P5** 4 ·
**P6** 8.

Two items are deliberately absent. The **false positive** (§7's
`author_user_org`) — that collection is dropped by `1781400000`, verified. And
§7.6's verified-good list, which is there precisely so nobody re-audits it.

## Suggested sequencing

Phases 0–3 are the merge gate. Two people can run in parallel from the start:
one on Phase 0 → 1 → 2 (code), one on Phase 3 (tests) — they barely overlap, and
the Phase 3 work is what makes the Phase 0–2 fixes durable. Phase 4 splits
cleanly by cluster. Phase 5 needs the Phase 0/4 isolation fixes first (not a
remote — that exists). Phase 6 is parallel-safe throughout and is good work for
someone new to the codebase: the docs are wrong in ways that will actively
mislead them, which makes them the right person to notice.

**Three ordering constraints that will cost a redo if ignored:**
- **P0-1 before P4-1** — the socket-hijack fix changes the socket layout the
  unlink race lives in.
- **P0-5 before/with P1-1 and P1-2** — the commentor role and the comments rules
  meet each other; tighten the comments rules without settling commentor and you
  lock a commentor out of commenting, the one thing they exist to do.
- **P1-5 early in its phase** — it is the largest single item, and if the PB
  back-relation bug turns out to persist on v0.39.8 it needs a schema change,
  which is a different size of job. Find out first.
