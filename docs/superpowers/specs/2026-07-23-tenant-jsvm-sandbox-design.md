# Harden the tenant server-side JS boundary (in-process sandbox) — Design Spec

**Date:** 2026-07-23
**Repos:** `~/code/tinycld/pocketbase` (the fork), `~/code/tinycld/multi-org` (the router).
**Status:** Approved design, pending implementation plan.

---

## 1. Problem

The multi-org router hosts N organizations in **one OS process**. Each tenant's
server-side JS (`pb_hooks` files and `pb_migrations`) runs on a shared sobek
runtime that is wired with the **full stock PocketBase JSVM API** (`jsvm.go`
`registerHooks` / `registerMigrations` → `sharedBinds` calls `BindOS`, `BindHTTP`,
`BindFilesystem`, `BindFilepath`, …).

Org authors are **fully untrusted / hostile** (self-signup, unvetted). Under the
stock API a hostile hook can, today, from any org's subdomain:

| Capability | Binding | Impact |
|---|---|---|
| Arbitrary command execution | `$os.exec` / `$os.cmd` (`exec.Command`) | Full host takeover (RCE) |
| Read any host file | `$os.readFile` / `readDir` / `$filesystem.local` / `fileFromPath` | Read **every other org's `data.db`** + the control-plane DB |
| Write/delete any host file | `$os.writeFile` / `remove` / `removeAll` / `rename` | Corrupt/destroy other tenants |
| Read process secrets | `$os.getenv` / `$os.args` | Steal `MT_SUPERUSER_PASSWORD`, TLS keys, S3 creds |
| Arbitrary outbound HTTP | `$http.send` / `fileFromURL` | Exfiltrate stolen data; SSRF (cloud metadata `169.254.169.254`, internal scan) |

Because all orgs share one process and one filesystem view, the per-org SQLite DB
and per-subdomain dispatch provide **no protection** against a malicious hook. The
biggest single risk the operator flagged — filesystem access — is real, but it is
one of several capabilities from the same root cause: **untrusted tenant JS is
granted host-level capabilities.**

### The reference model: managed WordPress hosting

This is the WordPress-multisite problem (many tenants, each running untrusted
plugin/theme code). Managed WP hosts contain it in **two layers**:

1. **App-runtime capability stripping** — `disable_functions = exec,shell_exec,
   system,proc_open,…` and `open_basedir` (confine the filesystem to the site
   dir).
2. **OS-level isolation** — a per-tenant PHP-FPM pool running under a **separate
   Unix user**, or a container/VM per site. This is the layer that actually
   contains a determined attacker.

Stock PocketBase-in-the-router is the *pre-hardening* WP config: every dangerous
function enabled, no `open_basedir`, one shared process. **This spec delivers
layer 1** (see §5 for the honest ceiling and why layer 2 is deferred).

---

## 2. Goals / non-goals

**Goals**

- Tenant hooks and migrations run with a **deny-by-default allowlist** of JS
  bindings: no raw filesystem, no `exec`, no env access, no outbound HTTP.
- Enforcement lives **at the engine seam in the fork** (never wire the capability),
  not as a post-hoc blocklist that clobbers globals.
- The **control-plane app keeps the full API** (it is operator-authored and trusted).
- Existing packages keep working — verified: **no real tinycld feature/package hook
  uses `$os`/`$http`/`$filesystem` today** (only the generated `types.d.ts`
  reference them). The locked-down subset breaks nothing that exists.

**Non-goals (this phase)**

- OS-level isolation (per-process / per-uid / namespace / container). Deferred —
  see §5.
- Mediated outbound HTTP (egress proxy with IP/destination allowlist). Outbound is
  **fully denied** this phase; revisit on real demand.
- Resource limits / DoS containment (CPU, memory, wall-clock caps). Out of scope
  for the capability-boundary work; noted as residual risk in §5.
- Confined-but-present filesystem access for hooks. Decided: **zero** raw FS.

---

## 3. Design

### 3.1 Fork: a sandbox flag on `jsvm.Config`

Add one field to `plugins/jsvm/jsvm.go` `Config`:

```go
// Sandboxed, when true, installs only the capability-safe JS bindings and
// omits the host-capability bindings ($os, $http, $filesystem, $filepath) from
// BOTH hook and migration runtimes. Intended for running untrusted (tenant)
// code. Default false preserves the full stock single-app API.
Sandboxed bool
```

Default `false` ⇒ byte-for-byte current behavior (single-app PocketBase, the
control-plane app, and upstream users are unaffected). The router sets
`Sandboxed: true` for tenant apps (§3.3).

### 3.2 Fork: allowlist the binds (deny by default)

There are **two** bind sites, and both must be gated — a hostile author can put
`$os.exec` in a *migration* just as easily as in a hook:

- `registerHooks` → `sharedBinds` (the executor + loader VMs)
- `registerMigrations` → the inline per-migration VM setup

Introduce a single helper that both call, e.g.:

```go
// bindSafe installs the capability-safe bindings common to every runtime.
func bindSafe(vm *sobek.Runtime) {
    BindCore(vm); BindDbx(vm); BindSecurity(vm)
    BindForms(vm); BindMails(vm)
    // $template, __hooks, require/console/process/buffer are set by the caller
}

// bindHostCapabilities installs the host-reaching bindings. Omitted when Sandboxed.
func bindHostCapabilities(vm *sobek.Runtime) {
    BindOS(vm); BindFilepath(vm); BindHTTP(vm); BindFilesystem(vm)
}
```

Each bind site becomes:

```go
bindSafe(vm)
if !p.config.Sandboxed {
    bindHostCapabilities(vm)
}
// hook site additionally: BindApis(vm) — safe (routing/middleware/errors only)
```

**Allowed for tenants (safe subset):**

| Bind | Why it is safe |
|---|---|
| `BindCore` | Type/field mapper, `toBytes`/`toString` reader helpers (size-limited). No host reach. |
| `BindDbx` | Query-builder expressions; execution is scoped to the tenant's own `$app` DB. |
| `BindSecurity` | Crypto/random/JWT/hashing helpers. Pure. |
| `BindMails` / `BindForms` | Mailer + record-upsert form constructors; operate through `$app`. |
| `BindApis` | Routing/middleware/API-error helpers **only** (`requireAuth`, `bodyLimit`, `ApiError`, …). *(hook site only; see 3.2.1.)* |
| router / cron / hook registration binds | `routerAdd`, `cronAdd`, `onRecord*` etc. — register handlers; no host reach. |
| `$template`, `require`, `console`, `process`, `buffer` | Templating + node-compat shims. *(`process` reviewed in 3.2.2.)* |

**Denied for tenants (never bound when `Sandboxed`):**

- `BindOS` — **entire** `$os` object: `exec`/`cmd` (RCE), `getenv`/`args` (secrets),
  `readFile`/`writeFile`/`readDir`/`remove`/`removeAll`/`rename`/`mkdir*`/`truncate`/
  `stat`/`dirFS`/`openRoot`/`openInRoot`/`tempDir`/`getwd`/`exit` (raw FS + process
  control).
- `BindFilesystem` — `$filesystem.local`, `fileFromPath`, `fileFromURL` (raw FS +
  SSRF). *(uploads go through `$app` record-file APIs, which are org-DB-scoped.)*
- `BindHTTP` — `$http.send` (SSRF/exfil) and the `FormData` constructor it installs.
  *(see 3.2.3 for the `FormData` caveat.)*
- `BindFilepath` — `$filepath.glob`/`walk`/`walkDir` (path enumeration) and the
  pure path helpers. Denied wholesale for a clean boundary; pure helpers can be
  re-added later if a hook legitimately needs them.

Verified during audit: `$os` / `$http` / `$filesystem` are **only** set inside
their own `Bind*` functions and are **not referenced** by any safe bind — so simply
not calling the three functions cleanly removes the capabilities with no dangling
references.

#### 3.2.1 `BindApis` is hook-only and safe

`BindApis` installs routing middleware, record-enrichment helpers, and API-error
constructors. Its one filesystem-adjacent member, `$apis.static` (serves a dir via
`os.DirFS`), lets a hook mount a directory as static files — but the **path is
author-supplied and the served content is the org's own** `pb_public`. This does
**not** grant reading arbitrary host paths into JS (it wires an HTTP static handler,
not a JS read primitive). **Decision: keep `$apis.static`**, but the implementation
MUST confirm the served root cannot escape the org dir; if that can't be
guaranteed cleanly, drop `static` from the sandboxed `BindApis` (split it like
`BindOS`). Flagged as an implementation checkpoint, not a blocker.

#### 3.2.2 `process` shim

The vendored `process` node-compat module (`internal/nodejs/process`) may expose
`process.env` / `process.argv`. If it does, that re-opens the secret-theft vector
that denying `$os.getenv` closes. The implementation MUST audit the vendored
`process` module and, when `Sandboxed`, ensure `process.env` is **empty/absent**
and `process.argv` carries no host argv. If the shim can't be neutered per-VM,
scrub it in the sandboxed bind path. **Implementation checkpoint.**

#### 3.2.3 `FormData` constructor

`BindHTTP` also installs the global `FormData` constructor (used to build
multipart bodies for `$http.send`). With `$http.send` denied, `FormData` is inert
but harmless. If any safe path needs `FormData`, hoist its registration out of
`BindHTTP` into a safe bind; otherwise let it go with `BindHTTP`. **Implementation
checkpoint.**

### 3.3 Router: request the sandbox for tenants

`internal/orgmanager/manager.go` `load()` passes `Sandboxed: true`:

```go
jsvm.MustRegister(pb, jsvm.Config{
    HooksDir:      filepath.Join(orgDir, "pb_hooks"),
    MigrationsDir: filepath.Join(orgDir, "pb_migrations"),
    HooksWatch:    false,
    HooksPoolSize: m.cfg.HooksPool,
    ProgramSource: m.cfg.Programs,
    Sandboxed:     true, // untrusted tenant code
})
```

**Second untrusted-code site — provision-time migrations.**
`internal/controlplane/provisioning.go` `bootstrapTenantOnce()` opens a transient
tenant app to run the tenant's materialized `pb_migrations` (author-controlled JS)
at provision time — and it runs **inside the control-plane process**. It currently
registers jsvm **without** `Sandboxed`, so a hostile author's migration can
`$os.exec` / read the control-plane env here, which is arguably *worse* than a
tenant-runtime escape. This call MUST also pass `Sandboxed: true`:

```go
jsvm.MustRegister(pb, jsvm.Config{
    HooksDir:      filepath.Join(orgDir, "pb_hooks"),
    MigrationsDir: filepath.Join(orgDir, "pb_migrations"),
    HooksWatch:    false,
    Sandboxed:     true, // untrusted tenant migration JS, run in the control-plane process
})
```

So there are **two router call sites** to flip to `Sandboxed: true`: the runtime
load path (`orgmanager.load`) and the provision path (`bootstrapTenantOnce`). The
control-plane's *own* app (registry/provisioning routes) is operator-authored; if
it registers jsvm for its own hooks it stays full-capability — but the *tenant*
apps it bootstraps must be sandboxed.

No change to `materialize`, `store`, `frontrouter`, or `progcache`. The
`ProgramSource` cache is unaffected: sandboxed and non-sandboxed apps never share a
process today (only tenants run sandboxed), and cache keys are `(src, strict)` on
identical tenant source — still correct.

---

## 4. Testing

**Fork (`plugins/jsvm/`):**

- **Deny tests** — with `Sandboxed: true`, a hook and a migration each asserting
  that `$os`, `$http`, `$filesystem`, `$filepath` are `undefined`, and that
  `typeof $os === 'undefined'` etc. (both bind sites).
- **RCE/secret/exfil negative tests** — a sandboxed hook attempting `$os.exec(...)`,
  `$os.getenv('MT_SUPERUSER_PASSWORD')`, `$os.readFile('/etc/passwd')`,
  `$http.send({url})` each throws a ReferenceError (binding absent), not executes.
- **`process.env` test** — sandboxed `process.env.MT_SUPERUSER_PASSWORD` is
  undefined (3.2.2).
- **Allow tests** — `Sandboxed: true`, a hook using `routerAdd`, `$app` DB query
  via a bound collection, `$security.*`, `$template.*` all work.
- **Default-off regression** — `Sandboxed: false` (or unset) still exposes the full
  API (existing suites stay green, unmodified).

**Router (`internal/`):**

- Extend the existing `orgmanager` e2e (`e2e_test.go`) so the tenant hook fixture
  additionally proves a sandboxed capability is denied end-to-end: a fixture hook
  route that returns whether `$os` is defined → asserts `false` through the served
  HTTP path. This proves the flag is actually threaded (a reviewer-style check that
  the test fails if `Sandboxed` is dropped).
- **Provision-path deny test** — a `controlplane` test provisioning a package whose
  migration attempts `$os.exec` (or reads `$os.getenv`) must fail closed: the
  migration's dangerous binding is absent, so it errors rather than executing in the
  control-plane process. Proves `bootstrapTenantOnce` passes `Sandboxed: true`.
- `controlplane` happy-path integration test unchanged (a benign TS/JS migration
  still provisions its collection).

**Whole suite:** `go build ./... && go vet ./... && go test ./... -count=1 &&
go test -race ./...` in both repos, per HANDOFF §8.

---

## 5. Honest security ceiling & residual risk

**This phase is layer 1 only — the WordPress `disable_functions` / `open_basedir`
tier. It reduces blast radius; it is NOT containment of a determined attacker.**

The security literature is explicit that PHP's `disable_functions`/`open_basedir`
are defense-in-depth with documented bypasses (FastCGI abuse, `dl()`, extensions
that skip the stream layer, `mail()` command injection), which is why serious WP
hosts always pair them with per-user/container isolation. The same honesty applies
here:

- **sobek is not a security sandbox.** A determined attacker may still find engine
  escapes, and the JS runtime shares the Go process heap. A memory-safety or
  interpreter bug is cross-tenant.
- **No resource limits this phase.** A hostile hook can still DoS the shared
  process via CPU spin, memory exhaustion, or unbounded allocation, taking down
  **all** tenants. (Noted; out of scope — needs OS-level limits.)
- **Shared-process secrets remain reachable** via any capability we failed to
  strip. The allowlist must be audited as exhaustive — and it did NOT prove
  exhaustive (see the outcome note below).
- **`$app` is NOT safely org-scoped against a hostile author.** The original draft
  of this bullet assumed anything reachable through `$app` (DB, mailer, settings)
  was correctly org-scoped and not a leak. **That assumption is false.** `$app.db()`
  exposes a raw SQL surface over the shared connection; a sandboxed hook running
  `ATTACH DATABASE '<other-org>/data.db'` in a transaction reads another org's
  secrets and can write arbitrary `.db` files — cross-org host-file read/write.

**The real boundary — layer 2 (now REQUIRED, not merely deferred):** OS-level
isolation — each org in its own process under a separate uid, with a filesystem
namespace/chroot so an arbitrary-path open (`ATTACH '<abs>'`, `$app.newFilesystem`,
backups) physically fails at the OS layer, cgroup CPU/memory/pids limits, a scrubbed
environment (no inherited `MT_*` secrets), and a read-only rootfs except its own
`pb_data`. The router's `GetOrg(slug) → http.Handler` seam (already an abstraction in
`frontrouter`) is where a reverse-proxy-to-subprocess drops in. Layer 2 gets its own
spec/plan.

**The spec/docs and any operator-facing README MUST state plainly that Phase 1 is
blast-radius reduction, not attacker containment.** Do not let this ship described as
a hard sandbox.

---

## 6. Outcome note (2026-07-23) — why the allowlist was the wrong altitude

Phase 1 shipped the binding allowlist (Tasks 1–11): `$os`/`$http`/`$filesystem`/
`$filepath` withheld, `process.env`/`argv` scrubbed, `$apis.static` withheld,
file-based `require()` denied, `$template` restricted to `loadString`, and
load-time throws fail closed instead of panicking the shared process. All of that
stands as defense-in-depth and is verified by tests.

But a final adversarial review then demonstrated a **live cross-org exploit through
`$app.db()` raw SQL** (`ATTACH DATABASE`), and our SQLite driver (modernc) exposes
no authorizer to contain it cleanly in-process. This was the *fourth* capability an
audit found re-entering through a binding we had kept (after `$apis.static`,
`require`, `$template`). The pattern is the finding: **you cannot safely allowlist
the full stock PocketBase `$app`/DB API against a hostile author in one shared
process** — `$app.db()` alone is an open-ended raw-SQL surface, and each audit
surfaces another kept capability.

**Decision:** stop in-process patching; make **OS-level per-process isolation the
next deliverable** (it confines filesystem, DB, resource, and unknown-future vectors
uniformly). Phase 1 remains valuable hardening but must not be represented as
containing a hostile tenant.

---

## 6. Fork-delivery consideration

The `Sandboxed` flag is a clean, backward-compatible, nil-default addition in the
same spirit as the existing `ProgramSource` / `BuildServeMux` seams (HANDOFF §1),
and is plausibly upstream-worthy on its own ("run untrusted hooks with a reduced
API"). Decide at implementation time whether to (a) fold it into the
`feat/multitenant-fork` integration branch only, or (b) also stage a clean
`feat/jsvm-sandbox` branch off `v0.39.8` for a potential upstream PR #3, mirroring
the PR #1/#2 pattern. Recommended: build on the integration branch first, extract a
clean branch later if upstreaming.
