# Finding — a tenant is not the same server as single-org

**Found:** 2026-07-27, while tracing whether the calendar takeover was real.
**Status:** **FIXED** 2026-07-27 — see "How it was closed" at the end.
**Severity:** privilege escalation in every tenant, independent of installed
packages.

---

## The question that found it

> "Why is a tenant-assembled org different from a single-org one? They should be
> identical."

They should be, and they are not. The difference is not a designed boundary —
it is a hand-rolled subset that drifted from the composition root it was copied
from.

## The two composition roots

`tinycld/server/main.go` calls one thing:

```go
coreserver.Register(app, coreserver.Options{
    RegisterExtras: registerPackageExtensions,
    ...
})
```

That wires jsvm, quota, notify, realtime, userorg, the users field guard, the
disabled-user guard, schema hooks, the DAV CORS bypass, and then the feature
packages.

`multi-org/cmd/serve-org/main.go` does not call it. It hand-rolls a subset:
`jsvm.Register` and `quota.Register`. Everything else is absent from every
tenant:

| registration | app | tenant |
|---|---|---|
| `RegisterUsersFieldGuard` | yes | **NO** — security |
| `registerDisabledUserGuardCore` | yes | **NO** — security |
| `notify.Register` | yes | NO |
| `notify.RegisterCommentMentionHooks` | yes | NO |
| `realtime.Register` | yes | NO |
| `userorg.Register` | yes | NO |
| `RegisterUsersDemoAuditHook` | yes | NO |
| `registerSchemaHooks` | yes | NO |
| `registerDavCorsBypass` | yes | NO |

Nothing detects the drift. Each time a registration was added to
`coreserver.Register`, the tenant silently did not get it, because no test runs
the tenant configuration.

## Why it is severe — proven, not inferred

`users.updateRule` is **deliberately loose**:

```
@request.auth.id != "" && (id = @request.auth.id
    || @request.auth.role = "owner" || @request.auth.role = "admin")
```

It is loose on purpose: `RegisterUsersFieldGuard` narrows it to "self, or an
owner/admin editing an allowlisted field". PocketBase rules cannot constrain
WHICH fields a write touches, so per-field policy has to be Go.

Asking `CanAccessRecord` directly, with a plain member PATCHing their own `role`
to `"owner"`, returns **true**. The rule alone permits it. In a tenant, with no
field guard bound, that is privilege escalation to org owner in one REST call —
no packages installed, no feature code involved.

`registerDisabledUserGuardCore` being absent is the same shape: a suspended
user's REST access in a tenant depends on a hook that is not there. (The DAV
paths are separately covered — `davauth.Authenticate` checks `disabled`
directly since P0-2 — but REST is not.)

## Relationship to the calendar finding

REMEDIATION-PLAN P1-5 (calendar member authz) is the same bug one level down,
and this is its root cause. Calendar's owner check lived in a Go request hook
that a tenant never binds, so the permissive `createRule` was the whole
authorization there. Verified by executing the attack: an unrelated signed-in
user POSTs `{calendar: <victim's>, user: <self>, role: "owner"}` to
`/api/collections/calendar_members/records` and gets HTTP 200 with an owner
membership.

That is fixed (migration `1830000004` + a pb-hook for the bootstrap), but the
fix treats a symptom. **Any** guard a package or core keeps in Go has the same
exposure until this is closed.

## Why the earlier framing was wrong

`docs/SCOPE-tenant-feature-go.md` treats this as a question about linking
FEATURE Go into tenants, with a product decision attached (may hosted
third-party packages ship Go?). That scope is still valid on its own terms, but
it is **not the first problem**. The first problem is that CORE's own guards are
missing, which no feature-linking decision addresses.

Read this file before that one.

## Direction for the fix

The tenant should call the same composition root the app does, minus only what
genuinely cannot run there. The genuine exclusions are narrow and follow the
actual architectural rule — **a service that must BIND A PORT moves into core so
the router can open it** — so what a tenant must skip is port binding and
host-only concerns (the static file server, Sentry init, CLI flags), not
authorization.

Sketch:

- Give `coreserver` a tenant-shaped entry point (or an `Options` flag) that
  registers the shared guards and skips the host-only pieces, and have
  `serve-org` call it instead of hand-listing registrations.
- Make the divergence impossible to reintroduce silently: a test that asserts
  the two configurations register the same guard set, so adding a registration
  to core without teaching the tenant fails.
- Audit what a tenant should genuinely NOT have (realtime? notify? — each needs
  a decision and a recorded reason, not an omission).

The last point is the real work. Some of these may be deliberate omissions
whose rationale was never written down; each needs an explicit answer rather
than being restored wholesale.

## Verification note

Both claims above were reproduced with throwaway probe tests (an executed
calendar takeover, and `CanAccessRecord` on the users rule) which were deleted
rather than committed — the permanent coverage belongs with the fix, asserting
the tenant configuration directly. `calendar/server/tenant_rules_authz_test.go`
is the pattern: shipped rules, real pb-hooks, no feature Go bound.

---

## How it was closed (2026-07-27)

The tenant now calls the **same composition root** the app does, exactly as the
"Direction for the fix" section proposed.

**One source of truth for the shared set.** `coreserver.Register` was split so
that everything both compositions run lives in `registerSharedEarly`
(Sentry + SystemConfig, which must precede any OnServe binding) and
`registerSharedCore` (the guards, invites, account lifecycle, notify, realtime,
audit, org-pkg guard, DAV CORS bypass). A new
`coreserver.RegisterTenant(app, TenantOptions)` composes a tenant from those
two plus the tenant-shaped engine wiring: sandboxed jsvm **with the `$`-binding
and hook-point seams wired** (`OnInit`/`OnLoaderInit` — so `webdavHook` /
`caldavHook` now dispatch in a tenant, closing that HANDOFF §6 item), quota
with `FixedLimits` from the router's config, and the DAV protocol servers from
the materialized source lists. `serve-org` deleted its hand-rolled jsvm+quota
block and its hand-mounted DAV handlers; it now calls `RegisterTenant` and owns
only transport (socket, readiness pipe, confinement, shutdown).

**The audit, resolved.** Every registration in `Register` is now either shared
or in an explicit host-only tail, each host-only entry carrying a reason in
`server.go`:

| registration | disposition | reason |
|---|---|---|
| users field guard, disabled-user guard, org_pkg_enabled guard | **shared** | authorization; the whole finding |
| invites, invite-links, password-reset mailer | **shared** | per-org account flows; mail is inert until the org has creds |
| account delete/disable/enable, admin offboard | **shared** | per-org account lifecycle |
| notify + comment-mention hooks, realtime broker | **shared** | per-org notifications / collab |
| audit hooks, users demo-audit hook | **shared** | per-org forensic trail |
| DAV CORS bypass | **shared** | tenant mounts the same protocols |
| Sentry + SystemConfig | **shared** | inert until the org's system_settings carry creds |
| package install/upgrade | host-only | router owns deploys; a self-restarting tenant escapes supervision |
| super-admin, VAPID admin endpoints | host-only | admin console is the control plane's |
| OTA app-update endpoints | host-only | served from the host build archive; an org dir has none |
| setup bootstrap / installer | host-only | control plane provisions tenants; AppURL is P2-4 |
| demo start/lead/reset | host-only | marketing-site machinery |
| schema-gen hooks | host-only | regenerates workspace TS; a tenant has no workspace |
| static/SPA serving | host-only | host-shaped; tenant static serving is separate open work |

**Impossible to reintroduce silently.**
`coreserver/composition_parity_test.go` composes one app via `Register` and one
via `RegisterTenant`, enumerates every `On*` hook by reflection, and requires
the per-hook handler-count difference to equal an allowlist (`hostOnlyHookDiff`)
that mirrors the table above. Adding a registration to `Register` without
deciding tenant-or-not fails the test with the offending hook name.

**Regression coverage for the proven holes.** `coreserver/tenant_test.go`
composes a real tenant via `RegisterTenant`, serves through the router mux, and
asserts a member's self-PATCH to `role=owner` is 403 and a disabled user's
password-auth and auth-refresh are 403. Both were verified red with the guards
removed from the shared set (the finding's whole history is tests that could
not fail).

**Not closed here, tracked elsewhere:** ~~feature-package Go still does not
link into a tenant~~ **CLOSED 2026-07-27** — `docs/SCOPE-tenant-feature-go.md`
landed (pinned menu in `multi-org/internal/tenantpkgs`, gated by
`.runtime/packages.json`, features split into `Register`/`RegisterTenant`),
which also deleted calendar's P1-5 pb-hook and closed the account-delete
reassignable-registry gap for menu packages (`userorg.RegisterReassignable`
runs from each feature's shared set in a tenant). Still open: invite/reset
mail delivers only once an org has mail creds, and tenant AppURL (P2-4) and
tenant static serving remain their own items.

---

## How it was closed (2026-07-27)

The tenant now calls the same composition root as the app. Concretely:

- **`coreserver.RegisterTenant(app, TenantOptions)`** (`coreserver/tenant.go`)
  is the tenant-shaped entry point. The shared set
