# Remediation plan — 2026-07-26 final review

Companion to `HANDOFF.md` §7. Every finding in §7.1–§7.5 appears here exactly
once, assigned to a phase. Phase order is by **risk of shipping without it**,
then by dependency.

> **Status reconciliation — 2026-07-27.** The checkboxes below had drifted
> from the code: every Phase 0 item and most of Phase 1 were implemented but
> never checked off. Each item marked `[x]` below was re-verified against the
> shipped code/migrations on 2026-07-27 (evidence noted inline). Items left
> `[ ]` were re-verified as genuinely open, except where noted "unverified".

> **Reconciliation audit — 2026-07-28.** A full second-pass audit re-verified
> every item against the shipped code and re-derived the §7 coverage mapping
> from scratch. Result: 60 of 64 items hold with evidence; **two items marked
> done were not** (P3-6's fix never takes effect; P2-11 closed 1 of its 3
> defects), **one §7.4 finding had no owner at all** (calendar's self-only
> member list rule — hidden by a coverage-table misfiling), and two HANDOFF
> statements were never reconciled (§5.6's phantom golden test, §6's
> `BeforeOverwrite` gap). See **Phase 7 — Reconciliation findings** and the
> corrected Coverage check below. Reopened items keep their original text with
> a `REOPENED` note; the fixes are tracked as R-items.

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

- [x] **P1-1 🟡 Disabled clause on text + calc comments** **DONE — verified 2026-07-27:** migration `1782200000_comments_disabled_and_creator.js` in both repos. **Correction (later same day):** the two repos sharing one filename broke the generator's global-uniqueness check (`packages:generate` failed at HEAD); calc's copy renamed to `1782200001` — idempotent, so DBs that applied it under the old name are unaffected. — `text`, `calc`.
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
  > **⚠️ REGRESSION FOUND AND FIXED 2026-07-27 — `1830000006`, calendar
  > `c6eec3b`.** `1830000004` conjoined the restored owner check with
  > `user = @request.auth.id`, which reads as "the row's user must be the
  > caller" — so the only creatable membership was your own and **calendar
  > sharing was impossible in the shipped code**: an owner adding a teammate
  > got a bare 400 and `AddMemberDialog` silently stayed open. The clause was
  > aimed at the takeover shape the header names
  > (`POST {calendar: <any>, user: <self>, role: "owner"}`), which `viaOwner`
  > already blocks by requiring the CALLER to hold an owner membership on the
  > target calendar — so it added no protection against its own stated threat
  > while removing the feature. `1830000006` drops it (D1-style product call:
  > confirmed with the user before changing a shipped security rule).
  >
  > **Why nothing caught it, and the lesson.** `member_create_rule_probe_test.go`
  > — written specifically to certify this rule — had its owner add
  > **themselves** to a second calendar they already owned, which satisfies
  > `user = @request.auth.id`. It asserted the one shape sharing never uses.
  > This is §7.4's fixture trap in its purest form: a test authored *for* a
  > security rule, passing on a rule that broke the feature.
  > `member_share_rls_test.go` now covers owner-adds-someone-else against the
  > SHIPPED rule (rlstest), with four denials — outsider→self, outsider→third
  > party, viewer re-share, editor→third party. **Two of those denials were
  > themselves vacuous on the first attempt** and were rewritten: "editor adds
  > themselves" passes on `UNIQUE(calendar, user)` whatever the rule says, and
  > "editor targets an unrelated calendar" is denied by the membership half of
  > the predicate before the role half is consulted. Only an editor holding a
  > row on the target calendar, adding someone else, isolates `role ?= "owner"`.
  > Every one verified by neutering.
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
- [x] **P1-7 🟡 `carddav.PutAddressObject` evaluates no rule** **DONE — verified 2026-07-27:** `saveAuthorized` (backend.go) save-evaluate-rollback on both create (`CreateRule`) and update (`UpdateRule`) paths, landed in `cb1fec3`; `backend_authz_test.go` covers create-denied, update-denied and a positive control (all three re-run green). — `tinycld` (core).
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
- [x] **P2-3 🟠 Share links never redirect signed-in members** **DONE — verified 2026-07-27:** `org_slug` deleted from `anon-identity.ts`; drive's `share-routing.ts` owns the member redirect without it. **Correction (later same day):** the helper lived inside `public-screens/`, so the route generator emitted a nonsense `/p/drive/share/share-routing` route whose re-export failed drive's `*.tsx`-only exports map, breaking the app-shell typecheck; moved to `tinycld/drive/lib/`. — `tinycld` (core)
  + `drive`. `ShareSession.orgSlug` is typed `string` but the server stopped
  sending `org_slug`, so the redirect gate is always falsy and members fall
  through to the public preview; the target is a dead `/a/` route anyway. Core:
  delete `orgSlug` from `ShareSession`/`SessionResponse` (its only consumer
  ignores it). Drive: gate on `item_id`, redirect to `/drive?file=…`.
- [x] **P2-4 🟠 Tenant `AppURL` is never set** **DONE 2026-07-27:** `Config.OrgURL` → `.runtime/app.json` (`writeAppConfig`, mirroring packages.json) → `--app-config` → serve-org persists `Settings().Meta.AppURL` post-bootstrap (persisted, not in-memory, so a tenant settings reload can't revert it); serve-multi wires `https://<slug>.<MT_BASE_DOMAIN>`. Tests: `app_config_test.go` + real-binary `TestTenant_AdoptsMaterializedAppURL` (asserts the settings value `{APP_URL}` interpolates from; template interpolation itself is upstream PB behavior). Written red-first. — `multi-org`. **More urgent
  since 2026-07-27:** feature-Go linking means tenants now run mail's Go and
  core's invite/password-reset mailers, so these emails can actually SEND once
  an org has mail creds — carrying `http://localhost:8090` links. Every org's
  verification, password-reset and email-change links point at
  `http://localhost:8090`. Materialize the org's public URL from
  `MT_BASE_DOMAIN` + slug into `.runtime/`, and have `serve-org` set
  `Settings().Meta.AppURL` at boot. **Done when:** a tenant's rendered auth email
  contains the org's real URL.
- [x] **P2-5 🟠 mail's phantom `org` field** **DONE 2026-07-27:** write, type field, provider.tsx scaffolding, both Go fixtures (`seedDomainMailboxAndOrg` deleted — collapsed into `seedDomainAndMailbox`), and the `dn()` factory all dropped; mail Go + `tinycld-pkg check` green. — `mail`. `provider.tsx:552` writes
  `org: orgId` into `mail_domains` — no migration defines it, and `orgId` is
  always `''`. It compiles because the local mirror `types.ts:29` declares it,
  which also makes `org` look filterable on every `mail_domains` query. Remove
  the write, the type field, the dead scaffolding
  (`provider.tsx:38,50,86,124,530`), the fixture uses
  (`imap_fetcher_test.go:148-149`, `aliases_test.go:40,205`) and the test factory
  (`useSendableIdentities.test.ts:22`).
- [x] **P2-6 🟠 Finish commit `4d52992`** **DONE 2026-07-27:** (a) `lifecycle_test.go` covers verbatim/normalized derivation, suffix collisions, unicode-sanitizes-away and the `i<=99` exhaustion; decision recorded in `handleUserCreated` — no-address stays non-fatal (a mailboxless user is recoverable, a blocked signup is not) but now logs at **Error**. (b) mail `seed.ts` derives from `username` (SeedContext gained `username`, populated by `seed-db.ts`); (c) `help/mailboxes.md` fixed (email→username, dead `user_org` reference dropped). — `mail`. The username-derived address
  change is unguarded and half-applied. (a) Test `deriveMailboxAddress`
  (`lifecycle.go:123-148`) — it has **none** — covering the `i<=99` exhaustion
  path and a unicode username, both of which currently produce **no mailbox at
  all** behind a log line. Decide whether that silent outcome is acceptable; it
  should probably be a hard error. (b) Fix `seed.ts:1663-1670,1729`, still
  deriving from the email local-part — the exact bug the commit fixed in Go — so
  seeded users get a different address than the server provisions. (c) Fix
  `help/mailboxes.md:10`.
- [x] **P2-7 🟡 Mail search failures are invisible** **DONE 2026-07-27:** all three server swallows (mailbox lookup, FTS query, structured query) now log at Error and return a 500 ApiError (`TestHandleSearch_QueryFailureIsAnError`, red-first — the fixture app's missing FTS tables are the forced error); client `useMailSearch` captures (`mail.search`) and the error threads through `SearchContext` to a rendered `SearchFailedState` (`useMailSearch.test.tsx` covers the hook). — `mail`. Server turns a SQL
  error into `HTTP 200 {"items":[],"total":0}`; client sets an `error` no
  consumer reads and never captures. This is the mechanism that let §3.2's
  `ts.user_org` bug present as a silent zero. Return a real status from the
  server; surface and capture on the client. **Done when:** a forced query error
  produces a visible failure state, not an empty list.
- [x] **P2-8 🟡 Calendar subscription data loss** **DONE 2026-07-27 — both `[S]` findings reproduced live first** (sync split into a testable `applyFeed` seam; suite runs the shipped migrations): (a) pruning now scoped to `from_subscription` events (new field via migration `1830000005`, no backfill — errs toward keeping data; matched events re-marked so pruning converges), with a positive prune control; (b) the `ical_uid` unique index is per-`(calendar, ical_uid)` in the same migration, sync creates stamp `Event.Defaults` before the codec (a validated user edit of an imported event now succeeds), and every write failure is logged at Error and surfaced in aggregate so the sync records `subscription_error` instead of reporting success. Sync path widened to `core.App` (§3.4). — `calendar`. Two `[S]`
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
- [x] **P2-10 🟡 Accept-invite shows "Welcome to "** **DONE 2026-07-27:** interpolation dropped (org branding has no source — §6 — so there is no honest name to render); copy is now "You're invited", `InviteInfo` matches the handler's actual response, and both e2e assertions tightened from `/Welcome to/i` to the exact copy. — `tinycld`. Client expects
  `orgName`/`orgSlug` the handler no longer sends. Either send a deployment name
  or drop the interpolation. Tighten the e2e, which passes on a loose
  `/Welcome to/i`.
- [x] **P2-11 🟡 Takeout counts dropped records as imported** ~~DONE — verified 2026-07-27~~ **REOPENED 2026-07-28 → R3, re-closed same day (see R3):** the count compensation is real (`{skipped: 1, imported: -1}`, `batch-inserter.ts`), but the item's other two defects shipped untouched — the `DocumentPicker` promise still has no `.catch` (`lib/takeout-import/index.ts:76-84`, an unhandled rejection), and all ten dedup lookups still `catch { /* not found */ }` on ANY error, so a transient failure still creates duplicates. — `takeout`. The
  two early-return skip paths don't compensate the `imported: 1`, so a failed
  parent calendar reports all its events as imported. Also: the `DocumentPicker`
  promise has no `.catch`, and dedup lookups treat any rejection as "not found"
  so a transient error creates duplicates.
- [x] **P2-12 🟡 Demo reset leaks `realtime_doc_updates`** **DONE 2026-07-27:** `wipeOrphanedRealtimeJournal` deletes journal rows whose `room_id` no longer resolves to a drive_item — keyed on room existence rather than this run's deletions, so it also drains rows leaked by pre-fix resets. Runs after the drive_items wipe. — `tinycld`. No FK, so
  nothing cascades and per-room truncation never fires for a deleted room —
  unbounded growth. Add it to the per-collection wipe.
- [x] **P2-13 🟡 IMAP multi-term BODY search ORs** **DONE 2026-07-27:** `buildFTSMatchSet` rewritten — per-term match sets intersected (Text terms union their two indexes, then AND with the rest), unanswerable criteria fail closed, failed FTS queries logged. `imap_search_fts_test.go` (4 cases over live-shaped FTS tables), multi-term cases red-first. — `mail`. Both arms of the
  "intersect for subsequent terms" branch are byte-identical, so
  `SEARCH BODY "a" BODY "b"` matches either. Implement the intersection.
- [x] **P2-14 🟡 mail's swallowed failures** **DONE 2026-07-27:** `EmailBody` captures + renders a visible "Couldn't load this message" state (and now rejects non-OK fetches instead of rendering the error body); `useSaveDraft` captures (`mail.draft.save`); `useAttachments` distinguishes a typed `AttachmentValidationError` (toast only — validation isn't a bug) from genuine failures (toast + capture); `useMailBulkActions` gained a shared `onError` (capture + `mutation.error` toast) on all six mutations. — `mail`. `EmailBody.tsx:41` renders
  a failed body fetch as an empty email; `useSaveDraft` doesn't capture (its
  `useSendEmail` sibling does); `useAttachments` toasts without capturing;
  `useMailBulkActions` has no `onError` at all, so a bulk action failing across N
  threads is silent.
- [x] **P2-15 ⚪ Silent-failure residue elsewhere** **DONE 2026-07-27:** `use-share-visitor-role` narrows the bare catch to 404 and captures everything else; `ShareDialog` no longer closes on a failed share-save — it captures, shows an inline error, and keeps the pending list so Done can be retried. ~~Phase 2 is now complete.~~ *(P2-11 reopened 2026-07-28 → R3, re-closed same day.)* — `tinycld` (core), `drive`.
  `use-share-visitor-role.tsx:92-95` still has the bare `catch { return null }`
  that *hid* the original bug — narrow it to a 404 and capture otherwise.
  `ShareDialog.tsx:181` swallows a failed share-save and closes as if it worked.

---

## Phase 3 — Make the tests capable of failing

**The most important phase for the long run.** Everything above can regress
silently until this lands. Work it immediately after Phase 2 — or in parallel by
a second person, since it barely touches the same lines.

- [x] **P3-1 🟡 RLS suites must read the shipped migrations.**
  **DONE 2026-07-27.** Re-verified against code: drive `guest_rls_test.go` +
  `disabled_rls_test.go` and text/calc `comments_rls_test.go` were ALREADY
  converted to `rlstest` (the earlier PARTIAL note had drifted). This pass
  converted the two real stragglers — calendar `guest_rls_test.go` and core
  `coreserver/guest_rls_test.go` (which needed the fixture's pre-existing
  username index/identity dropped so core's chain applies as on a real DB) —
  each **verified by actually neutering** the authoritative migration
  (calendar `1830000004`, core `1870000000`): deny-tests go red, controls stay
  green. Two suites deliberately keep exercised constants: mail (the endorsed
  model — byte-identical constants + paired controls) and core
  `caldav/backend_test.go`, which mirrors CALENDAR's rules — core tests must
  not read a feature sibling's migrations or a partial assembly breaks; the
  drift guard for those rules is calendar's own `tenant_rules_authz_test.go`.
  Original text: seven suites
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
  > **Straggler found 2026-07-28 → R4.** The original seven-suite list included
  > calendar `calendar_members_authz_test.go`, which the DONE note silently
  > dropped from both the converted set and the endorsed keepers. It still
  > hand-declares `calMembersUpdateRule` (`:38`) and applies it to the fixture
  > itself, and its `|| (user = @request.auth.id)` self-clause appears in no
  > shipped migration and is checked by no drift guard.
- [x] **P3-2 🟡 mail's inbound fixture declares a schema that does not exist** **DONE 2026-07-27:** fixture fields renamed `user_org`→`user` (matching migration `1713000000`); `assertThreadStateInInbox` added to `_KnownRecipientStoresMessage` + `_IdempotentRetry`, confirmed red against the phantom fixture before the rename. —
  `mail`. The sharpest instance in the review: the fixture declares
  `mail_mailbox_members.user_org` and `mail_thread_state.user_org` while
  production reads and writes `user`, so `TestHandleInbound_KnownRecipientStoresMessage`
  and `_IdempotentRetry` **pass while thread state is written keyed to `""`** —
  green through a total failure to deliver mail. Shared by `imap_fetcher_test.go`
  and `smtp_inbound_server_test.go`. Rename the fixture fields, then assert
  delivery actually lands on the recipient's state row.
- [x] **P3-3 🟡 mail's folder counts have no coverage** **DONE 2026-07-27** (mail
  `018494c`): dead `computeMailboxFolderCounts` + its 13 tests and the
  `EMPTY_FOLDER_COUNTS` re-export deleted; `useMailboxFolderCounts.test.tsx`
  mounts the real hook against real TanStack DB collections (predicate
  executes; u2's rows excluded), asserts the view's column set FROM the
  shipped migration, and covers the thread_state invalidation bridge incl.
  unsubscribe. Each guard verified red by neutering (predicate, migration
  column, queryKey). `mailListHelpers.test.ts` `as any` stubs replaced with a
  typed fixture. — `mail`. 13 tests guard
  `computeMailboxFolderCounts`, which the app no longer calls; the real counts
  come from the `mail_folder_counts` view, which has **zero** coverage — not its
  column names, not the predicate, not the realtime bridge. Structural cause: **no
  vitest file in mail mounts a hook**, so no live-query shape in the package is
  tested. Delete the dead tests (and the dead re-export), add coverage for the
  view. Also `mailListHelpers.test.ts:132-168`: three `as any` stubs carry 5 of 8
  fields, so a rename stays green — the cast is load-bearing, remove it.
- [x] **P3-4 🟡 Router tests that cannot fail** **DONE 2026-07-27** (`ef0ade0`):
  `TestBuildCmd_EnvIsAllowlistOnly` pins the constructed child env (the e2e
  secrets test is annotated as defence-in-depth — the sandbox scrubs
  process.env, so it cannot observe an env leak); `assertFullyPopulated`
  makes the manifest wire round-trips falsifiable (zero==zero can no longer
  hide a dropped field; verified red by dropping a fixture field) and a
  missing CardDAV round-trip was added; the cross-org CardDAV test gained
  falsifiable principal probes (org A's user must 401 against org B's
  tenant) — which found a LIVE bug: bad DAV Basic credentials returned 500
  with no WWW-Authenticate (CardDAV authenticated in the backend, unlike
  CalDAV/WebDAV), fixed in core `c4a7fc0` with route-level auth +
  `register_auth_test.go` (verified red pre-fix); the malicious-migration
  test now asserts the error names `$os`. The confinement store test was
  already repaired under P5-1. — `multi-org`. Four:
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
- [x] **P3-5 🟡 Takeout's mirrored-schema guard covers 1 of 9+ collections**
  **DONE 2026-07-27** (takeout `0761e9a`): `batch-inserter-schema.test.ts`
  drives a full import through a recording pb and pins every dedup/poll
  filter string and every create payload's exact key set for all ten foreign
  collections (contacts, calendar_calendars, calendar_events, drive_items,
  drive_shares, labels, mail_threads, mail_messages, mail_thread_state,
  label_assignments), hand-verified against the owning migrations, plus a
  no-`user_org`/`org` residue sweep. Verified red by reintroducing a
  `user_org` write. —
  `takeout`. Every other foreign write is `pb`-mocked, so a rename in any owning
  package passes the whole suite. Current field names were hand-verified correct,
  so this is a guard gap, not live drift. Extend the field-name assertion to
  every mirrored collection.
- [x] **P3-6 🟡 `package-scripts` tests never run** ~~DONE — verified 2026-07-27~~ **REOPENED 2026-07-28 → R2, re-closed same day (see R2). The original fix never took effect.** The glob was added to the WORKSPACE-ROOT `vitest.config.ts`, whose root has no `package-scripts/` dir (the tests live at `tinycld/package-scripts/tests/`) — `npx vitest list` from the root still collects 1 file / 3 tests, the exact green-while-running-nothing symptom this item describes. The stale `CORE_DIR` alias is likewise unfixed (points at root `core/`, which holds only `types/`). This is the plan's own fixture-trap lesson applied to a checkbox: the DONE note verified the glob string exists, not that it matches anything. — `tinycld`. 12 tests
  orphaned from every runner: the workspace-root vitest globs point at paths that
  no longer exist, so a root run collects 1 file and reports green. They pass
  when forced. Fix the globs and the stale `CORE_DIR` alias.
- [x] **P3-7 🟡 e2e assertions that another package can satisfy** **DONE
  2026-07-27** (mail `4711b4e`, contacts `3ccdf2f`, calc `c8d4020`, calendar
  `ba39016`, core `eda65a9`): mail's page-wide advanced-search absence checks
  scope to a new `advanced-search-panel` testID, the duplicate-mailbox error
  match scopes to the form's error summary, and label rows carry
  `mail-sidebar-label-<id>` (replacing `ancestor::*[5]`); contacts' positive
  assertions join its deny-side ones on the contact-row testID; calc's
  number/name assertions (incl. page-wide `toHaveCount(0)`) scope to a new
  `calc-grid-root`; calendar's sharing assertions scope to
  `calendar-member-row-<userId>` (replacing a `../..` walk). `SidebarItem`
  gained an optional testID passthrough. All suites verified green. — The collision
  class §3.6 predicted. mail is worst (`mail-inbox.spec.ts:136-146` asserts five
  labels are absent from the **entire page**, so drive rendering a "Size" column
  fails a mail-search test; `mail-shared-mailbox-admin.spec.ts:87` matches four
  common words anywhere in the DOM; `mail-labels.spec.ts:14` walks a hardcoded
  `ancestor::*[5]`). contacts' positive assertions are bare `getByText('Alice')`
  though its deny-side ones are correctly scoped; calc and calendar the same.
  Scope everything to row/test IDs.
- [x] **P3-8 ⚪ e2e discipline** **DONE 2026-07-27** (tinycld `95e3984` +
  helpers commit, takeout `2932474`, calendar `c6eec3b`, core error fix):
  `createInvitedUser` SPA-clicks Settings → Members instead of `page.goto`;
  invite-flow's vacuous `url().toContain('/')` deleted (the preceding
  `waitForURL(LANDED_URL)` is the real assertion); takeout's spec navigates
  in-app (month stepping via header arrows), matches the event popover by
  testID instead of `[style*="width: 360"]`, matches drive rows by aria-label,
  and collapses its inline 10s–120s timeouts to two named budgets.
  **Running these found five live bugs, each fixed with a regression test
  verified red first** (the spec was orphaned — takeout shipped no
  `playwright.config.ts`, so `test:e2e` could not run it at all):
  1. fflate's streaming `Unzip` misparsed embedded docx/pptx archives inside
     data-descriptor entries, failing EVERY real Drive import with
     "unexpected EOF"; replaced with central-directory iteration.
  2. `login()` was not idempotent — after `createInvitedUser` it hit the
     already-authenticated shell, found no form, and deadlocked until the test
     budget died (presenting as a timeout, which is what a bumped budget would
     have wrongly "fixed").
  3. calendar sharing was impossible — see P1-5 follow-up below.
  4. mail's e2e delivered to `user@tinycld.org`, an address the
     username-derived mailbox scheme no longer mints, so every inbound 403'd.
  5. core's `extractValidationErrors` never mapped direct PocketBase field
     errors (`error.response.data` IS the map), so every form validation
     failure surfaced as a generic toast. — `tinycld`, `takeout`. `helpers.ts:143`
  hard-`goto`s `/settings/members` against the discipline the same file
  documents; `invite-flow.spec.ts:140` asserts `url()).toContain('/')`, which is
  vacuous; takeout's spec uses `page.goto` for in-app nav plus inline 10s–120s
  timeouts and a `[style*="width: 360"]` selector.
- [x] **P3-9 🟡 `TestConfinement_*` do not skip — they do not exist** **DONE 2026-07-27:** `confinement_stub_test.go` (`//go:build !linux`) makes `-run TestConfinement` print an explicit SKIP on darwin; CI (P5-1) is the real fix. —
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
- [x] **P4-1 🟡 Post-Evict socket unlink race.** **DONE 2026-07-27 — the race had TWO unlinkers:** (1) teardown's `os.Remove` now guarded by inode ownership (`OrgInstance.sockIno` recorded at readiness, `ownsSocket()` before removal); (2) the CHILD's Go `UnixListener` unlink-on-close — which deletes whatever is at the path when the drained listener finally closes — disabled in serve-org (`SetUnlinkOnClose(false)`; the router owns the file's lifecycle). `TestTenant_EvictThenImmediateTrafficStaysReachable` holds an in-flight request through the evict (the open-SSE Deploy shape), spawns the replacement inside the drain window, and asserts the org stays reachable — red against HEAD, and still red with only the parent-side guard, which is what exposed the child-side unlink. A shut-down instance
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
- [x] **P4-2 🟡 One failed spawn counts as two crashes** **DONE 2026-07-27:** the supervisor now waits for the instance's FATE (a new `published` channel, closed at map-publication) before accounting — the naive fix (mark closed before Kill) still double-counted, because a fast-dying child's supervisor passes its non-blocking check before the spawn failure path runs. Spawn failures close `closed`, so the load path records exactly one crash and the host-killed child is no longer logged at Error as "exited unexpectedly". `TestBackoff_FailedSpawnCountsOnce` asserts consecutive==1 AND the backoff interval ≤ backoffMin+jitter (red at HEAD: consecutive=2), plus log-content absence. Bonus: a child crashing between readiness and publication is now reaped from the map (previously it could linger dead until the idle sweep).
- [x] **P4-3 🟡 Drain budget the child can never use** **DONE 2026-07-27:** `killTimeout = drainTimeout + 5s` — a child holding an open SSE stream legitimately uses ALL of its `--drain` (its `srv.Shutdown` returns only at ctx expiry), so every SSE-holding teardown ended in an avoidable SIGKILL. `TestTeardown_KillPatienceExceedsChildDrainBudget` pins the relationship between the two production constants (red at HEAD: 5s ≤ 10s); the ignores-SIGTERM kill test re-anchored on `inst.shutdown` with explicit budgets so the suite doesn't wait out the raised production window.
- [x] **P4-4 🟡 `Deploy` re-materializes a running tenant** **DONE 2026-07-27:** `Materialize` now builds each of pb_hooks/pb_public/pb_migrations into a fresh generation dir (`<name>.genN`) and atomically renames a symlink over the live name (dirs can't be rename-replaced; symlinks can). The immediately-previous generation is kept until the next deploy so an in-flight path resolution never lands in a deleted tree; older ones are GC'd. Covers Deploy AND load-during-predecessor-drain. `TestMaterialize_LiveTreeNeverGoesDark` hammers 50 rebuilds under a concurrent reader — red against the in-place clear (sustained ENOENT streaks), green now (the residual is a one-syscall darwin EINVAL blip from rename-over-symlink, explicitly tolerated and documented). A brief old-hooks/new-assets mix during the drain remains — strictly better than the prior 404 window and unavoidable without per-instance tree pinning. — before evicting it, so
  the live tenant 404s on static assets during that window and the whole drain.
  Evict first, or materialize to a temp dir and rename.
- [x] **P4-5 🟡 WebDAV manifest prefix has no default or validation** **DONE 2026-07-27:** empty webdav prefix defaults to `/dav/<slug>` (the reserved protocol namespace from P2-1 — core's `isDavPath` — so a defaulted mount can never shadow the SPA); `validateDAVPrefix` rejects malformed prefixes (no leading slash / bare root / trailing slash) and duplicates across packages, for BOTH WebDAV and CalDAV source resolution (a duplicate was a boot panic inside the tenant; now a load error naming both packages). Four new tests, all red at HEAD. Cross-protocol collisions remain out of scope (each protocol resolves independently) — documented on the helper.
- [x] **P4-6 🟡 The proxy drops the client IP twice.** **DONE 2026-07-27:** contract made positional — the router guarantees the RIGHTMOST X-Forwarded-For entry it forwards is the best-known client IP, matching PB's default resolution (`UseLeftmostIP` false). `ForwardedConfig{Proto, PeerIsClient}` on `orgmanager.Config`, wired by serve-multi from MT_TLS_MODE: proxy mode forwards the LB's chain VERBATIM (appending the LB's own peer IP — what bare `SetXForwarded()` did — shadows the client it reported) and passes through the LB's X-Forwarded-Proto, defaulting to the configured `https`; file/autocert append the peer so a client-spoofed inbound chain lands LEFT of the genuine entry. TrustedProxy is materialized via `.runtime/app.json` (`trustedProxyHeaders`, now always written) and adopted persisted into `Settings().TrustedProxy` at tenant boot alongside AppURL. Three router-side header tests (red at HEAD) + real-binary `TestTenant_ResolvesClientIPThroughForwardedChain` driving proxy→materialize→adopt→`e.realIP()` (verified red by neutering the adoption: `{"ip":"invalid IP"}`).
- [x] **P4-7 🟡 `evalManifest` has no interrupt** **DONE 2026-07-27:** `time.AfterFunc(manifestEvalTimeout=5s) → vm.Interrupt` around the manifest VM run. `TestEvalManifest_InterruptsRunawayScript` runs a `while(true){}` manifest (verified red by neutering the timer: never returns); the timeout is a var only so the test can shrink the spin window to 200ms.
- [x] **P4-8 ⚪ Cosmetic cluster** **DONE 2026-07-27:** (a) sweep ticker interval floored at 1ms — `MaxIdle=1ns` panicked the sweeper goroutine, taking the process down (`TestSweep_TinyMaxIdleDoesNotPanic`, verified red by neutering); (b) serve-org sets umask 0177 around the socket `net.Listen` so the file is 0600 AT creation (chmod kept as belt); (c) `buildCmd` no longer takes the unused `ctx` — the signature itself now prevents re-introducing §5.9's CommandContext bug, comment moved to the doc; (d) `execProcess.Signal` delivers to the process group like `Kill`, so graceful SIGTERM reaches grandchildren instead of only the SIGKILL fallback sweeping them.

*Isolation depth* — `multi-org`
- [x] **P4-9 🟡 `chownTree` never chmods** **DONE — verified 2026-07-27:** `chownTree` chmods 0600 files / 0700 dirs during the walk (`spawn_linux.go`)., so one org's `pb_data` stays
  mode-readable by other tenant uids — the `ATTACH DATABASE` read the boundary
  claims to close. Chmod dirs `0700` and files `0600` as you chown.
- [x] **P4-10 🟠 Namespaces are set unconditionally** **DONE 2026-07-27:** all privileged confinement (clone flags, uid window, store remount) gated on euid 0 (`ce29b38`). Surfaced live by the confinement workflow's first run — the controlplane integration tests spawn through the production spawner on the unprivileged runner and every spawn EPERM'd. `TestSpawner_NonRootSpawnsUnconfined` pins the degraded mode from CI's unprivileged step. — so on a **non-root Linux
  host every tenant spawn fails** (`operation not permitted` → every org 503s),
  despite `NewSpawner` warning that promises a degraded-but-working mode. Gate
  the clone flags the way the uid block already is.
- [x] **P4-11 ⚪ No network namespace** **DONE 2026-07-27 — resolved as "correct the docs", recorded as a deliberate decision:** tenants make legitimate outbound connections (calendar ICS subscription fetches, mail provider APIs, Sentry), so `CLONE_NEWNET` without veth/NAT plumbing would break shipped features for no boundary gain — `$http` is withheld by the sandbox and the DB/filesystem boundary doesn't depend on the network. README diagram fixed (mount+pid ns, no netns) and the decision documented in its security section; per-tenant egress policy, if ever wanted, is an operator firewall concern. The README pass also fixed the P6-3 items in the same file: per-org socket path, CardDAV-only descriptions of davconfig/serve-org, the already-fixed reserved-slugs bullet, the stale "no CI" section, and the fork replace path (now the vendored `../tinycld/third_party/pocketbase`). P6-3 is thereby closed too.

*Feature performance* — `mail`
- [x] **P4-12 🟡 JS-stitched joins.** **DONE 2026-07-27** (mail `5167742`): `useMailboxes` and `useSendableIdentities` each resolve membership → mailbox → domain (→ aliases, left-joined) in ONE query expression; the settings screen's `useMailboxData` (admin view: every mailbox, not the caller's) resolves relations in join expressions across its queries rather than Map-stitching, with only per-mailbox grouping Maps remaining. *(Correction 2026-07-28: the earlier "each ... in ONE query" wording overstated the third hook.)* Cosmetic residue → R8: `flattenSendableIdentities.ts` kept its filename after its export was renamed `groupSendableIdentities`. The settings screen also sheds its first-domain-only artifact (mailboxes on every configured domain now list). New `useMailboxHooks.mount.test.tsx` mounts the real hooks over real TanStack DB collections so the joins and the user predicate actually execute — verified red by neutering the predicate, which also caught the first fixture draft being insensitive (a personal-typed decoy hides behind splitMailboxes' find-first; the decoy must be shared-typed). Mailbox admin + compose e2e green. `flattenSendableIdentities` → `groupSendableIdentities` (joined-row grouping); `splitMailboxes` narrowed to the pure split/sort.
- [x] **P4-13 ⚪ Residual N+1s** **DONE 2026-07-27** (mail `e067b59`): IMAP Login batches mailbox+domain resolution into two `FindRecordsByIds` lookups; the Text-search thread arm maps FTS thread matches to messages in one thread-IN-subquery statement (no per-match query, no IN-expansion limit); `handleUserDeleted` probes memberships with one DISTINCT query — and on a query error now sweeps NOTHING, where the old per-mailbox error path DELETED the mailbox on unknown state. Sweep gained its first test (membered kept / memberless personal swept / shared untouched). Full mail Go suite + all 8 IMAP e2e specs green. **Phase 4 is now complete.**

---

## Phase 5 — Prove the boundary (the §6 top item, now unblocked)

Do **not** start before P0-1 and P4-9/P4-10 land: standing up CI against a
boundary that is known-broken just encodes the break.

- [x] **P5-1 Linux CI running `TestConfinement_*` as root.** **DONE — GREEN 2026-07-27** (run 30318853658: all five `TestConfinement_*` pass as root; the first run failed and correctly surfaced P4-10, which is exactly the job this CI exists to do): `.github/workflows/confinement.yml` clones all 7 sibling repos (the go.mod replaces) on the `multi-org` branch, runs build/vet/full suite, then `sudo go test -run TestConfinement` (ubuntu runners are full VMs, so root has real CAP_SYS_ADMIN; private siblings need a `SIBLING_CHECKOUT_TOKEN` secret). The P3-4 confinement repairs (real store-file probe through the mount namespace, per-org socket-dir assertions) were verified already in the tree. Not green until pushed — verify the first run. **Unblocked** — the
  remote exists and everything is pushed (D3). The only real prerequisites are
  technical: two of these tests are vacuous even on Linux (P3-4), so repair them
  first or CI certifies less than it appears; and P0-1/P4-9/P4-10 must land or CI
  will faithfully encode a broken boundary. Needs a root-capable runner
  (privileged container or a VM), since the namespace and uid work requires
  `CAP_SYS_ADMIN`.
- [x] **P5-2 Resource limits.** **DONE 2026-07-28:** `MT_TENANT_MEMORY_MAX` /
  `MT_TENANT_PIDS_MAX` / `MT_TENANT_CPU_MAX` (no defaults — unset is unlimited)
  are validated/canonicalized by `orgmanager.TenantLimitsFromEnv` (cross-platform,
  unit-tested red-first incl. every invalid shape logging at Error and dropping
  to unset) and written by `placeInCgroup` into `memory.max`/`pids.max`/`cpu.max`
  **before** the pid lands in the group. Incoherent configs warn loudly
  (cgroup root with no limits; limits with no root), and a limit the kernel
  refuses fails the whole placement with a warning rather than leaving a
  group that looks confining and is not. `TestConfinement_CgroupLimitsApplied`
  (root, Linux, runs in the P5-1 confinement workflow) asserts the kernel's
  canonical readback (`64M` → `67108864`), so bytes merely landing in a file
  the kernel rejected cannot pass. Original text: `MT_CGROUP_ROOT` creates a per-tenant cgroup but
  writes no limits, so a runaway tenant can still starve the host. (§6, brief
  decision #6.)
- [x] **P5-3 Provisioning out of the control plane.** **DONE 2026-07-28 —
  removed, not relocated**, exactly as §7 suggested: `bootstrapTenantOnce`
  deleted; `CreateOrg` materializes, activates, then **verifies by booting the
  tenant through the org manager** (`Provisioner` gained a `verify` hook;
  serve-multi wires it to `mgr.Get`). Migrations apply inside the confined
  tenant (`apis.Serve` → `RunAllMigrations` before readiness); a boot failure
  comes back through the readiness pipe with the child's reason and rolls the
  org back to `provisioning` (resumable). Red-first:
  `TestCreateOrg_VerifiesTenantBootBeforeReturning` +
  `TestCreateOrg_VerifyFailureRollsBackActivation` (both confirmed red at
  HEAD). The five provisioning integration tests were rewired to the
  production shape (counting spawner pins provision-time spawn == 1, Get
  reuses it); `TestIntegration_MaliciousMigrationCannotExec` still asserts the
  sandbox reason (`$os`) — now proving it survives the trip through the
  readiness pipe — plus a new rollback-to-provisioning assertion. §7 refines this: it may be
  **removable rather than relocatable** — `apis.Serve` already runs
  `RunAllMigrations()` unconditionally inside the confined tenant, so the first
  spawn applies the same migrations in isolation. `bootstrapTenantOnce` mainly
  fails `CreateOrg` fast and flips status to `active`. Try deleting it and
  reporting migration failure through the readiness handshake before building a
  one-shot subprocess path.
- [x] **P5-4 Live smoke per §4**, extended with what this review found: a
  disabled user against all three DAV protocols, a WebDAV **create** (not just an
  update), a hard reload of `/drive`, and a commentor on a shared doc.
  **DONE 2026-07-28 — performed against a fresh boot of the assembled app
  (`/tmp/tinycld-server serve`, shipped migrations + hooks), all green:**
  - *Schema:* 45 collections, zero `orgs`/`user_org`/`org_provisioning`;
    `users.role` + `users.disabled` present.
  - *RLS live:* guest `drive_items` create denied (400), member create 200.
  - *Disabled user (P0-2/P0-6):* CardDAV **and** CalDAV **and** WebDAV PROPFIND
    all 401 via Basic (owner controls 207 on the same paths); WebDAV PUT-create
    401; password login 403; **pre-disable token 401 after ADMIN disable**
    (key rotation confirmed on the admin path); re-enable restores fresh login.
  - *WebDAV create (P0-3):* member PUT-new 201 → GET 200 round-trip, MKCOL
    201; guest PUT-create refused; `OPTIONS` answers `Dav: 1, 2`; another
    user's unshared path reads **404, not 403**.
  - *Commentor (P0-5/D1):* commentor share → read 200, `PATCH` rename denied
    (masked 404), `text_comments` create 200; unshared user's comment create
    denied (400).
  - */drive collision (P2-1):* `GET /drive` no longer reaches the DAV mount
    (no 401/`WWW-Authenticate`; this bare boot has no SPA bundle so it 404s —
    the full reload-lands-in-app property is the P2-1 e2e); `/dav/drive`
    correctly challenges Basic.
  - *CalDAV wire (§3.7):* **minimal VEVENT** (no TRANSP/CLASS) PUT 201 with
    `busy_status=busy`/`visibility=default` persisted from `Event.Defaults`;
    `RRULE:FREQ=WEEKLY;BYDAY=TU,TH;COUNT=10` **round-trips byte-identical**
    (no `VALUE=TEXT` mangling, no dropped rule); cross-user event GET 404.
  - *Mail scoped read (§3.2 class):* `/api/mail/search?q=…&folder=inbox` 200
    against the shipped schema — post-P2-7 a bad column would 500 loudly, so
    the silent-zero failure mode is structurally closed.

---

## Phase 6 — Docs, dead code, duplication

Low risk, high readability value. Safe to hand to a fresh pair of hands.

- [x] **P6-1 Instruction docs that now mislead.** **DONE 2026-07-28** (workspace
  `3f733cf`, tinycld `c3bc736`): CLAUDE.md and CONTRIBUTING.md teach the shipped
  single-org contract — `OrgScope` is `{userId}`, roles on `users.role`, bare
  routes, `app/(app)`/`app/p` route re-exports, the real `SeedContext`
  (`user.username` included), rule-mirroring in Go instead of "manual equivalent
  authorization". The joined-query reference example is now mail's
  `useMailboxes.ts` (OrganizationsTab is a static stub). `docs/packages.md`
  de-orged the same way (incl. `expand: { owner: coreStores.users }`).
  Original text: `~/code/tinycld/CLAUDE.md` and
  `tinycld/CONTRIBUTING.md` still teach the `user_org` junction, `/a/<orgSlug>`
  routes, `getRoleForOrg`, and `OrgScope` as `{orgId, userOrgId, orgSlug}`
  (shipped: `{userId}`). **These are the files an agent reads first**, so this
  ranks above its severity.
- [x] **P6-2 `HANDOFF.md` itself.** **DONE 2026-07-28** — header, §6 and §7
  reconciled with Phase 6 complete; see HANDOFF for the two small findings this
  phase surfaced but did not fix (DeleteAccountModal's visual default vs the
  submitted plan, and the five deliberate inline biome waivers).
- [x] **P6-3 Router README** **DONE 2026-07-27** (folded into P4-11's README pass — see that entry).
- [x] **P6-4 User-facing help still teaches multi-org.** **DONE 2026-07-28**
  (tinycld `62d86e4`, contacts `dae5712`, mail `5799157`, calendar `b62f582`):
  organizations.md rewritten around the workspace + shipped roles
  (owner/admin/member/guest); super-admins.md stops advertising in-app org
  management; account-settings.md documents disable (reversible, admin
  re-enable) and delete (reassign / delete-everything / no-peers) flows;
  contacts' carddav.md points at `/carddav/u/ab/default/` and its
  directory/labels/importing topics de-orged; mail's provider-setup.md now
  says the provider is deployment-wide `system_settings` configured from the
  Setup dashboard (the env-var fallback it documented no longer exists —
  `errNoProvider`'s message pointed at the deleted `MAIL_PROVIDER` var and was
  fixed with it). Also found+fixed: calendar's caldav-hooks.md claimed tenants
  don't run package TS — stale since `RegisterTenant` landed.
- [x] **P6-5 Dead and lying code.** **DONE 2026-07-28** (multi-org `83abb5b`,
  contacts `9ac2de5`, mail `f9f18f0`): the contacts smoke script was FIXED
  rather than deleted (auth without the deleted expand, real
  `/carddav/u/ab/default/` path, ok/fail actually print); router dropped
  `orgs.custom_domains`, the never-written `packages.content_hash`/`manifest`
  columns and `store.ContentHash` (the live manifest.json materialization is
  untouched); the davconfig comment now records the closed state (rules +
  core/quota travel; only the version snapshot is lost in a tenant);
  `mergeSharedFolderStates` + its suite deleted. The two router test defects
  were repaired, not deleted: `TestTenant_EvictTerminatesTheProcess` now
  probes the pid with signal 0 (its `pid` capture was dead — the test never
  checked the process), and `TestGet_AfterShutdownStoresNothing` asserts the
  refusal error instead of discarding it (it passed identically on `nil, nil`).
- [x] **P6-6 Comment rot + naming residue.** **DONE 2026-07-28** (tinycld
  `2d214c7`, text `7698dc2`, calc `8a1984a`, drive `ea2cc00`+`b05e864`, mail
  `c1922b0`, contacts `2541e3b`): all inventoried live-voice `user_org`
  comment sites in core (mention maps, contact suggestions, share-editor,
  MembersDrawer, UserMenu, audit-log.md, dynamic-packages-remaining.md)
  corrected; the phantom "public share-link render endpoint" comments in
  text/calc now name the auth-gated endpoint as the only caller (any future
  caller must bring its own check); drive's stale `1716200001` citations
  repointed at `1782100000` and drive.pb.ts/webdav register say five points;
  mail's `userOrgIds*` → `userIds*` (the identifier was actually
  `userOrgIdsForFolder`, not `...ForFilter`); the `MemberRow.userOrgId`
  duplicate field collapsed onto `userId`; drive's `snapshotCurrentFile`
  `userOrgID` param renamed. **Scope grew:** drive's and contacts' READMEs were
  saturated with the same rot (nonexistent `resolveItemAndUserOrg` /
  `getUserOrgForOrg`, per-org book paths, `(item, user_org, …)` column sets) —
  both rewritten against the shipped code. calc e2e helper comments describing
  an org-context race the synchronous shims made impossible were also fixed,
  along with calc's mislabeled `RejectsItemIDMismatch` test comment + dead
  `_ = itemID`.
  > **Residue found 2026-07-28 → R5.** The "all inventoried live-voice sites"
  > claim over-reached: drive's `ShareDialog` chain still ships a
  > `currentUserOrgId` prop (~10 sites; its value is a plain user id —
  > `use-share-data.ts:98` returns `currentUserOrgId: userId`), three drive
  > tests key fixtures on `ownerUserOrgId: 'uo1'`, and text's tests carry live
  > `userOrgID` loop variables plus present-tense "expects `user_org` in
  > production" comments in three Go test files.
- [x] **P6-7 Duplication worth lifting.** **DONE 2026-07-28** (tinycld
  `fbe1069`, text `4240a54`, calc `2e8977b`, multi-org `7e0d40a`). The ordering
  bug was real and RED-FIRST:
  `TestWebDAVHookTypoRegistersNothing` (fixture
  `webdavHook({ beforeWrite(e){}, beforeWrit(e){} })`) failed against HEAD —
  the valid handler stayed registered on a hook file that errored, exactly the
  partial registration caldav's validate-first order prevents; the existing
  unknown-name test used a lone typo'd key and could not catch it. The shared
  `core/davhooks` now owns HostBindings, `NormalizeHandlerSource`, and the
  validate-BEFORE-compile order; webdav/caldav keep only their name lists +
  binding names, and coreserver's two identical adapters collapsed into one.
  `sharelink.AuthorizeAnonRoom`/`ReadOnlyForConn` replace the byte-identical
  text/calc pair (thin ShareClaims adapters remain; both packages' admission
  suites now exercise the lifted code). The router's two `buildTenantBinary`
  copies — which had already diverged, the controlplane copy lacking the
  chmods the confinement tests need — merged into
  `internal/testsupport.BuildTenantBinary` (one build per test run).
  Deliberately NOT lifted, per the §6 assessment: renderETag/handleRender
  (httpetag) and the Register/RegisterTenant/SetBootstrap boilerplate.
- [x] **P6-8 Cosmetic.** **DONE 2026-07-28** (tinycld `1506afa`, calc
  `791c76d`, text `9c427e6`, drive `5de30d7`):
  - Drive's `add_thumb_index_hashes` renamed to `1781900000` (order-preserving);
    its up() is now guarded so DBs that applied it under the old name re-run it
    as a no-op. The `exclude_disabled` twin KEEPS its name — renaming it would
    re-run its rule restatement after `1782100000` and regress the guest clause.
  - biome.json: five dead `app/a/[orgSlug]`/`app/share` excludes deleted;
    schema bumped to the 2.5.1 CLI (was the standing "1 info").
  - The ~15 biome-ignores resolved as: **10 were dead** (the keys had become
    composite templates; biome flagged them `suppressions/unused` — the
    standing "10 warnings") and were deleted; the deliberate positional-key
    waivers for calc's pivot/CSV components + text's ImportWarningBanner moved
    into the canonical `noArrayIndexKey` override (MarkdownRenderer's existing
    pattern), so `pnpm run checks` and `tinycld-pkg check` now agree. Five
    inline waivers remain by choice (text build.ts noConsole ×2, the
    `__DEV__`-guarded editor warning, anchored-overlay's narrow dep, calc
    SheetTabs a11y) — each is load-bearing under the member gate and carries
    its rationale; a whole-file config-off would silence future genuine hits.
    (Along the way: biome applies the react-domain rules inconsistently for
    sibling paths passed as `../<member>`, which is why the root lint called
    the suppressions unused while the member gate needed them.)
  - `members.tsx`'s three `as unknown` casts deleted — the generated users
    type has carried `username`/`role`/`is_demo` since the schema regen, so
    the stated justification was already false.
  - All 27 `/a/acme/...` fixture paths in unit tests (and coreserver's
    `static_test.go`) de-slugged; `use-server-address-gate` had the encoded
    old path baked into an expectation.
  `pnpm run checks` now finishes with zero errors, warnings, and infos.

---

## Phase 7 — Reconciliation findings (2026-07-28 second-pass audit)

The audit that produced this phase re-verified all 64 items against the shipped
code (60 held with file:line evidence) and re-derived the §7 coverage mapping
from scratch. These are the items that fell out. R1–R4 are the substance; R5–R8
are residue and doc truth.

- [x] **R1 🟠 Calendar "Shared with" cannot list other members** **DONE
  2026-07-28:** migration `1830000007_members_visible_to_calendar_members.js`
  (list/view = `enabled && viaMember`, down restores self-only). Red-first:
  four new rlstest cases in `member_share_rls_test.go` (owner-lists-added-
  member, member-lists-co-members, owner-views-row, shipped-rule clause) all
  failed at HEAD showing exactly one self row; outsider-lists-nothing control
  green throughout. The sharing e2e now reloads and asserts the owner sees the
  SHAREE's member row — the assertion the old spec conspicuously lacked; it
  failed against the pre-migration server (reproducing the bug live, and also
  catching that `packages:generate` must run for a new migration to reach the
  per-file symlink tree) and passes now. Full calendar Go suite + all 3
  sharing e2e tests green. — `calendar`.
  The one §7.4 finding that reached "all items closed" with **no owner**
  (§7.4: "calendar's member list/delete rules are self-only, `1715000000:265-271`").
  The coverage table's misfiled P1-4 made the §7.4 count appear to close over
  it. Still live: `1830000004` set `calendar_members.listRule/viewRule` to
  `enabled && user = @request.auth.id` and `1830000006` fixed only
  `createRule`; no Go hook widens listing, and pbtsdb populates the local
  collection through the list rule. So an owner who adds a teammate sees the
  row only optimistically — after a reload, "Shared with" shows just their own
  row, and there is no row to remove the teammate from (`deleteRule` DID gain
  `viaOwner`, but the UI can't reach it). The sharing e2e passes because it
  verifies membership via the **sharee's** CalDAV PROPFIND and only ever
  asserts the owner's own row — precisely what §7.4 predicted.
  **Decision (D4): membership rows are visible to every member of that
  calendar.** New migration (`1830000007`) sets list/view to
  `enabled && calendar.calendar_members_via_calendar.user ?= @request.auth.id`
  — the same back-relation shape `1830000006` settled for create. Owners need
  the full list to manage sharing; co-members seeing who else has access is
  the standard product shape and reveals only `(calendar, user, role, color)`.
  **Done when:** rlstest cases fail against `HEAD` — owner lists the added
  member, a non-owner member lists co-members, an outsider still lists
  nothing (control) — the tenant drift guard pins the new clause, and the
  sharing e2e asserts the owner sees the **sharee's** member row after a
  reload (the assertion shape today's spec conspicuously lacks).
- [x] **R2 🟡 P3-6 reopened — `package-scripts` tests still orphaned** **DONE
  2026-07-28:** the glob now lives in `tinycld/vitest.config.ts` (whose root
  contains `package-scripts/`) — the app shell's member run collects 600 tests
  (588 + the 12), green. The workspace-root config adopted the bootstrap
  template's corrected `CORE_DIR`/paths, then BOTH were narrowed to what the
  root owns (`tests/**` + the package-scripts glob): the template's
  `tinycld/core/**` globs ran core's suite without the app-shell config's
  stubs (`app-updater` etc.) and failed on collect — root `npx vitest` now
  runs 15/15 green instead of erroring or vacuously collecting 1 file. —
  `tinycld` (+ workspace root, + bootstrap template). See P3-6's REOPENED note
  for the diagnosis. Fix where the tests actually live: glob
  `package-scripts/tests/**` from `tinycld/vitest.config.ts` (whose root
  contains `package-scripts/`), repair or remove the workspace-root config's
  dead `core/**`/`package-scripts/**` globs and stale `CORE_DIR` alias, and
  correct the bootstrap workspace template so fresh assemblies don't re-mint
  the dead glob. **Done when:** a vitest list from the config that claims them
  shows the 12 tests collected, and they pass.
- [x] **R3 🟡 P2-11 reopened — takeout's other two defects** **DONE
  2026-07-28:** `DocumentPicker` chain gained `.catch` → `captureException
  ('takeout-pick')`; all ten dedup catches narrowed via `isNotFound` (status
  404 only — anything else rethrows into `insertRecords`' error accounting).
  `batch-inserter-dedup-errors.test.ts` red-first (500 and status-less
  rejections: no create, error surfaced — both failed pre-fix by stash; 404
  control creates). The schema suite's recording pb now rejects with a
  404-shaped error, which the narrowing itself forced — a plain `Error` no
  longer reads as "not found". Member check green. — `takeout`. See
  P2-11's REOPENED note. Add `.catch` + `captureException` to the
  `DocumentPicker` promise; narrow the ten bare dedup `catch` blocks to
  404-only ("not found → create") and rethrow anything else so a transient
  error fails the row visibly instead of minting a duplicate. **Done when:** a
  unit test forcing a non-404 dedup failure asserts no create happens and the
  error surfaces; a 404 still proceeds to create (control). Must fail against
  `HEAD`.
- [x] **R4 🟡 P3-1 straggler — `calendar_members_authz_test.go`** **DONE
  2026-07-28 — kept, reframed as a deliberate fixture; new drift guard.** The
  suite legitimately tests the Go guard (defence-in-depth), which is
  UNREACHABLE behind the shipped owner-only updateRule — so the permissive
  pre-`1830000004` rule is applied on purpose, renamed
  `permissiveSelfUpdateRule` with a header saying exactly that (a fixture
  choice, not a mirror claim). What was missing shipped-side:
  `tenant_rules_authz_test.go` now also asserts the ABSENCE of the
  self-update clause on `calendar_members.update` — the takeover opening
  `1830000004` closed had no tripwire against returning. — `calendar`.
  See P3-1's straggler note. Convert the suite to the shipped rules via
  `rlstest`, or delete it if `member_share_rls_test.go` +
  `tenant_rules_authz_test.go` already cover every behavior it asserts —
  whichever it is, no hand-declared rule constant survives outside the two
  endorsed keepers (mail, core caldav).
- [x] **R5 ⚪ P6-6 residue — live-voice `userOrg` identifiers** **DONE
  2026-07-28:** drive's ShareDialog chain `currentUserOrgId` → `currentUserId`
  (dialog + Connected + `use-share-data`); the three test fixtures' dead
  `ownerUserOrgId: 'uo1'` key (excess-property check was disabled by the
  `...opts` spread, so the REQUIRED `ownerUserId` was silently undefined at
  runtime) → `ownerUserId: 'u1'`; text's spec vars `userOrgID`/
  `distinctUserOrgIDs` renamed, the three Go test files' present-tense
  `user_org` comments corrected, `uo-*` fixture ids de-orged, and the stale
  `user_org_resolver_test.go` reference repointed at `authorship_cache_test.go`
  (the resolver is gone — identity comes from `conn.AuthID()`). Drive + text
  checks green; residue grep clean. — `drive`,
  `text`. See P6-6's residue note for the inventory. Rename
  `currentUserOrgId` → `currentUserId` through the ShareDialog chain,
  `ownerUserOrgId` fixture keys → `ownerUserId`, text's `userOrgID` loop
  variables, and fix the three Go test files' present-tense `user_org`
  comments. **Done when:** a live-voice grep over drive + text matches only
  historical/negating references, and checks stay green.
- [x] **R6 ⚪ HANDOFF §5.6 claims a golden test that does not exist** **DONE
  2026-07-28 — the test was written, making the claim true** (preferred over
  editing the claim): paired golden tests with the same fixture and
  byte-identical expected output — router
  `internal/controlplane/transpile_golden_test.go` and fork
  `plugins/jsvm/transform_golden_test.go` — each referencing the other, both
  green (both repos pin esbuild v0.28.1; an option change or lone version
  bump on either side turns that side red). §5.6 and the §7.5 finding
  updated. —
  `multi-org` + `tinycld` (fork). §5.6 says the two esbuild call sites (fork
  `transformSource`, router `transpileForStore`) are "kept in sync by a golden
  test"; §7.5 itself records that no such test exists. Make the claim true —
  paired golden tests, same fixture in both repos, byte-identical expected
  output, so divergence turns one side red — or correct §5.6 to state
  sync-by-inspection. Assess the call sites first; prefer the test.
- [x] **R7 ⚪ Tenant `BeforeOverwrite` gap is open but untracked** **DONE
  2026-07-28 — the seam was added (closed for real, not just recorded):**
  `webdav.RegisterSourceHooks(app, slug, hooks)` (app-store registry;
  `NewFileSystem` adopts onto the matching materialized Source; explicit
  source hooks win) + drive's `RegisterTenant` registers the now-shared
  `driveWebDAVHooks` — RegisterExtras runs before the sources mount, so the
  ordering is built in. Red-first: two core webdav tests (materialized-shaped
  source adopts; explicit beats registered — failed to compile at HEAD, the
  seam didn't exist) and a drive composition test verified red by neutering
  the one-line wiring. The three stale comments (webdav `source.go`, drive
  `register.go`, router `davconfig/webdav.go`) and HANDOFF §6 now record the
  closed state. — `multi-org`
  + `tinycld` (core) + `drive`. §6 records it in present tense ("Remaining
  gap, small"), no plan item owns it, yet HANDOFF's header now claims
  "Nothing in §6 remains open." The gap is real and code-documented
  (`webdav/source.go:115`: the materialized tenant Source carries no Go hooks,
  so a tenant-served overwrite never archives the previous version — drive's
  Go IS linked into tenants now, but `drive.RegisterTenant` cannot attach
  hooks to a source core mounted from JSON). Either add the seam (feature Go
  attaches `Hooks` to a named materialized source at RegisterExtras time) or
  record the accepted limitation; in both cases make HANDOFF §6 tell the
  truth.
- [x] **R8 ⚪ Cosmetics + doc truth from the audit** **DONE 2026-07-28:**
  `flattenSendableIdentities.ts` → `groupSendableIdentities.ts` (git mv + five
  import sites, mail check green); HANDOFF header/§5.6/§6/§7.4/§7.5
  reconciled with Phase 7. The two note-only records stand as written. —
  `mail`, `multi-org`.
  Rename `flattenSendableIdentities.ts` → `groupSendableIdentities.ts` (the
  export was renamed under P4-12; the filename and five import sites were
  not). Reconcile HANDOFF's header/§6/§7 with this phase (the "all 64 done" /
  "nothing open" claims are stale while R-items are open). Note for the
  record, no action: the `/dav` prefix reservation (P2-1) is enforced by
  documentation only — nothing programmatically rejects a package slugged
  `dav`; the biome schema bump (P6-8) landed only in `tinycld/biome.json`
  (root/utils/bootstrap configs lag behind, checks still green).

---

## Coverage check

**64 original items + 8 reconciliation items (R1–R8) across 8 phases.**
Corrected 2026-07-28 — the original table's arithmetic hid R1 (see below).
Every §7 finding is assigned:

| §7 section | Where it lands |
|---|---|
| 7.1 critical (1) | P0-1 |
| 7.2 high security (4) | P0-2, P0-3, P0-4, P0-5 |
| 7.3 high correctness (9) | P2-1…P2-6, P4-1, P4-10; calendar-tenant → P1-5 |
| 7.4 medium (24) | P0-6, P1-1…P1-3, P1-6, P1-7, P2-7…P2-14, P4-2…P4-7, P4-9, P4-12; self-only member list → **R1** |
| 7.4 tests-that-cannot-fail (12) | P3-1…P3-6, P3-9 (P3-4 absorbs five router bullets) |
| 7.5 low / cleanup (11) | P1-8, P1-9, P2-15, P3-7, P3-8, P4-8, P4-11, P4-13, P6-1…P6-8 |
| §6 / §7.2 lead-in (calendar disabled clause) | P1-4 |
| §6 carried forward (3 + 1) | P5-1, P5-2, P5-3; `BeforeOverwrite` → **R7** |

Per phase: **P0** 6 · **P1** 9 · **P2** 15 · **P3** 9 · **P4** 13 · **P5** 4 ·
**P6** 8 · **P7** 8.

> **Corrections to the original table (2026-07-28).** §7.3 has 9 findings, not
> 8 (the cell always listed 9 items). §7.4's test cluster has 12 bullets, not
> 11, and maps to seven P3 items — P3-7/P3-8 actually remediate §7.5's "e2e
> discipline" bullet, where they now appear. §7.5 has 11 bullets, not 10.
> **P1-4 covers no §7.4 bullet** — it traces to §7.2's lead-in / §6's
> "calendar entirely" note; filing it under §7.4 made that section's count
> appear to close while the self-only-member-list finding sat unassigned
> (now R1). One finding is deliberately split rather than assigned once:
> §7.5's silent-failure-residue bullet lands in P2-15 (core + drive halves)
> and P2-11/R3 (the takeout half).

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
