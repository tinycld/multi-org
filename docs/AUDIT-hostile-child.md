# Hostile-child audit — router↔tenant contract

**Status:** performed 2026-07-31 as design §7 step 5's hostile-child audit.
Threat model (design §4): a tenant process runs third-party package Go the
org's admin chose, in-process with full process privileges. The OS boundary
(uid / mount+PID namespaces / cgroup / allowlist env / read-only artifact
mount) contains it, but the router↔child *contract* must hold under the
assumption that the child is fully attacker-controlled. The router is trusted;
the child is not.

Each finding was verified against the code (CONFIRMED = the path was traced;
PLAUSIBLE = needs a runtime check). Fixes are tracked below with their status.

## CRITICAL

### C1 — router (root) follows tenant-planted symlinks writing `.runtime/*.json`
`manager.go` `writeCardDAVConfig`/`writeQuotaConfig`/`writePackagesConfig`/
`writeAppConfig`/webdav/caldav and `deploy.go` `writeDeployResult` all do
`os.WriteFile` with no `O_NOFOLLOW`/beneath guard. `chownTree` hands
`<orgDir>/.runtime` to the tenant uid every spawn, so a running tenant can
replace a config file (or `.runtime` itself) with a symlink; the next `load`
writes through it as root — arbitrary host file overwrite, cross-tenant DB
truncation, victim `app.json` (→ auth-email hostname) hijack.
**Fix:** write every `<orgDir>` file through an `O_NOFOLLOW` + `Lstat`
regular-file guard (helper `writeBeneath`). Status: **FIXED**.

### C2 — `/v1/resolve` runs git-spec lifecycle scripts as root in the router
`deploy.go` `decodeSpec` validates only with `pkgbuild.ValidatePackageSpec`,
which accepts git/URL specs; `/v1/resolve` then calls `ResolveSpec` →
`npm pack <spec>` **inline in the router process**, and `npm pack` on a git
spec runs the package's `prepare`/`prepack` scripts. Arbitrary RCE as the
router user, outside every confinement. `resolve.go`'s own comment ("the
router only passes registry specs today") documents the invariant that the
control socket broke.
**Fix:** restrict the control socket's spec surface to registry specs
(`sourceNpm`), mirroring the tenant-side gate in `pkg_hosted.go`. Status:
**FIXED** (registry-only gate on `/v1/resolve` and `/v1/versions`).

### C3 — tenant-triggered subprocesses inherit the router's secret env
`pkgbuild.RunCmd` leaves `cmd.Env` nil → the child inherits `os.Environ()`,
which for the router holds `MT_SUPERUSER_PASSWORD`, `MT_TLS_KEY`, etc. Every
tenant-reachable `npm`/`git` subprocess (resolve/versions/build) inherits it.
The tenant spawner gets this right (allowlist-constructed env); the router's
own inline tooling does not.
**Fix:** allowlist-construct the env for router-side `npm`/`git`/manifest-eval
subprocesses. Status: **FIXED** (RunCmd scrubs to PATH/HOME/TMPDIR; the git
gate C2 removes the git-clone surface entirely).

### C4 — build jobs can overwrite already-committed sibling artifacts
`cas.go` `Commit` renames the staged tree into `builds/<hash>/` preserving the
**builder-uid** ownership, and `confine_linux.go` gives the job no mount jail,
so a build job (running a package's lifecycle scripts as the builder uid) can
walk `builds/*` and overwrite every other org's `tinycld` binary / `pb_hooks`.
**Fix:** `chown root` the staged tree before the rename so committed artifacts
are immutable to the job uid. Status: **FIXED**.

## HIGH

- **H1** — `/v1/build`,`/v1/resolve`,`/v1/versions` have no rate limit / concurrency
  cap and no lockfile-size cap; resolution runs *before* the builder's
  `MaxConcurrent` semaphore. One org exhausts disk/fd/processes for all.
  **Fix:** cap lockfile size in `refsFor`; per-org token bucket + global
  in-flight cap on the three endpoints. Status: **FIXED** (size cap + per-org
  control-socket rate limit; global build concurrency already capped by the
  builder semaphore, resolution now gated behind the same per-org limiter).
- **H2** — `/v1/versions` is a tenant-controlled SSRF (`git ls-remote` on an
  arbitrary URL) with the router's creds/SSH. **Fixed with C2** (registry-only).
- **H3** — unbounded global `versionCache` in `pkgbuild/versions.go` → router
  OOM from distinct specs. Status: **FIXED** (LRU cap + periodic sweep).
- **H4** — control-socket `http.Server` has no timeouts / `MaxHeaderBytes` →
  slowloris fd-exhaustion of the router. Status: **FIXED**.
- **H5** — mail `TrackConn` taken before authentication lets unauthenticated
  clients pin orgs resident and starve admission. Status: **OPEN** — needs the
  splice to observe auth before tracking; tracked as follow-up (touches the
  mailrouter splice path, larger than the step-5 contract fixes).

## MEDIUM / LOW

- **M1** — ready→exit resets crash backoff to 1s forever (`clearCrash` on the
  readiness handshake, not on sustained health). Status: **FIXED** (clear only
  after a sustained-health interval).
- **M2** — readiness is a self-report; the router never probes the socket, so a
  ready-but-not-serving build forges a deploy commit. Status: **OPEN**
  (follow-up: dial + health-check before treating a spawn as ready).
- **M3** — a tenant can plant `.deploy/backup.db` a later operator deploy
  restores. Status: **PARTIAL** — the beneath-write guard (C1) + snapshot
  `Lstat` refuse a symlinked/irregular backup; binding the snapshot to its
  proposing deploy is an OPEN follow-up.
- **M4** — `chownTree` will `Lchown` a tenant-planted hardlink out of the org
  dir (gated by `fs.protected_hardlinks`). Status: **FIXED** (skip `Nlink>1`).
- **M5** — tenant stdout floods the router log sink (no rate limit). Status:
  **OPEN** (follow-up: token-bucket `pipeToLog`).
- **L1** — `--confine-packages` remount is child-enforced / post-`init()`;
  sound as defense-in-depth only (the binary can't be swapped by the tenant).
- **L2** — degraded (non-root) mode collapses the ctl.sock boundary → cross-org
  deploy authority. Status: **FIXED** (refuse to bind control sockets when
  confinement is absent).
- **L3** — `manifesteval` child only `RLIMIT_AS`; low risk (sobek, no bindings).
  Env scrubbing lands with C3.
- **L4** — cgroup path built from a slug; re-assert `SlugPattern` at the
  orgmanager boundary. Status: **FIXED**.

## Surfaces verified SOUND (falsifiability)

Readiness-pipe parsing (bounded size/time/memory, first-line-only), the
control handler being slug-bound by construction (no handler reads a slug from
the body), deploy spec-token validation before concatenation, `/v1/deploy`
serialization + rate limit, snapshot WAL-sidecar handling, snapshot discard on
a refused no-downs proposal, socket lifecycle (inode guard, unlink-on-close,
umask, one-dir-per-org), process-group kill + PID-namespace teardown (no
survivors), evict/load epoch race handling, uid allocation fail-closed +
stable, `chownTree` symlink handling + owner-only root gate, tenant HTTP socket
0600. The child env being allowlist-constructed (`spawn_exec.go`) is the
strongest part of the contract — and the reason C3 (the router's own inline
tooling not doing the same) stood out.
