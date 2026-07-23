# Follow-up brief: OS-level per-process tenant isolation

**Status:** NOT a design spec — a scoped brief to start the design from, when prioritized.
**Date opened:** 2026-07-23
**Relates to:** `2026-07-23-tenant-jsvm-sandbox-design.md` (§6 outcome note), HANDOFF §5 finding #3.

---

## Why this exists (the decision that led here)

The in-process jsvm **Sandboxed** mode (shipped, Tasks 1–11) is real blast-radius
reduction — but it does **not** contain a hostile tenant author, and a final
adversarial review proved a **live cross-org exploit** that in-process allowlisting
cannot cleanly close:

- `$app.db()` hands tenant JS a raw SQL surface over the shared connection. A
  sandboxed hook running `ATTACH DATABASE '<other-org>/data.db'` inside a
  transaction reads another org's secrets, and can create arbitrary `.db` files
  on the host (arbitrary host-file read/write).
- Our SQLite driver (**modernc.org/sqlite**, v1.54.0) exposes **no authorizer API**
  (`sqlite3_set_authorizer` is not available), so the `ATTACH`/filesystem-op class
  cannot be denied cleanly at the driver layer in-process.
- This was the *fourth* capability an audit found re-entering through a *kept*
  binding (after `$apis.static`, file-based `require`, `$template.loadFiles`). The
  pattern is the finding: **you cannot safely allowlist the full stock PocketBase
  `$app`/DB API against a hostile author in one shared process.**

**Conclusion:** OS-level per-process isolation is the **required** security boundary
(not optional hardening). It confines filesystem, DB (`ATTACH`), resources, and
*unknown-future* vectors uniformly — which in-process allowlisting provably cannot.

## What "done" means (the target)

Each org's PocketBase app runs in its **own OS process, confined to its own
directory**, such that:

- An arbitrary host-path open — `ATTACH '<abs path>'`, `$app.newFilesystem`,
  backups, or any future file-reaching capability — **physically fails at the OS
  layer**, not because a specific SQL statement or binding was blocklisted.
- One org's process cannot read another org's `pb_data`, the control-plane DB, the
  process environment (`MT_SUPERUSER_PASSWORD`, TLS keys), or arbitrary host files.
- A hostile or runaway tenant cannot starve the shared host (CPU / memory /
  wall-clock / fd / disk limits per tenant).
- The tenant subprocess does **not** inherit the parent's secret environment.

## What already exists to build on (concrete anchors)

- **The seam is already in the right place.** `frontrouter` dispatches via
  `GetOrg(slug) → http.Handler` (`internal/frontrouter/frontrouter.go`). Today that
  handler is an in-process `apis.BuildServeMux`. In a per-process model it becomes a
  **reverse proxy to the tenant's subprocess** (loopback TCP or Unix domain socket).
  The front router itself does not change.
- **`orgmanager` is the lifecycle owner.** `internal/orgmanager/manager.go` `load()`
  already does: resolve lockfile → `materialize.Materialize(orgDir, resolved)` →
  bootstrap PB + jsvm → build handler; plus singleflight-collapsed lazy load,
  `Evict`, and an idle-eviction sweeper. In a per-process model, `load` **spawns a
  subprocess** (and waits for its health check), `Evict` **SIGTERMs + reaps** it.
- **On-disk layout is per-org already.** Each org lives at
  `<Root>/pb_orgs/<slug>/{pb_data,pb_hooks,pb_public,pb_migrations}`
  (`manager.go:102`). A per-tenant filesystem view rooted here is natural.
- **`materialize` is unchanged.** The subprocess reads the same symlink-farmed dir.
- **A single-tenant entrypoint is mostly present.** `cmd/serve-multi` wires the
  multiplexed process; a per-org subprocess needs a *single-org* PocketBase
  entrypoint (serve one org from `<orgDir>` on a given socket) — a small new `cmd`.
- **DB connect hook exists.** `core.Config.DBConnect` (`DBConnectFunc`) lets a tenant
  process customize its SQLite connection — relevant if a per-connection lockdown is
  layered *in addition to* OS confinement (belt-and-suspenders), though OS
  confinement is the load-bearing fix.

## What in-process hardening is retained (defense-in-depth)

The `Sandboxed` jsvm mode stays ON inside each tenant subprocess — it's cheap and
still removes exec/env/outbound/file-read bindings. OS isolation is the boundary;
the allowlist is a second layer.

## Open decisions for the design session (do NOT pre-decide)

1. **Isolation primitive / deployment target.** The biggest fork. Options span:
   single Linux host we control (fork per-uid subprocess + mount/PID/net namespace +
   seccomp + cgroups — strongest, most portable-to-our-own-infra, most code);
   container-per-tenant via a local runtime (Docker/Podman — strong, heavier
   cold-start, runtime dependency); orchestrator/K8s (pods on demand — best ops,
   biggest jump from single binary); or a cross-platform stance (strong on Linux,
   documented weaker macOS-dev fallback since namespaces/cgroups are Linux-only).
   *This question gates everything else and was deferred here.*
2. **Supervision model.** Self-supervised in `serve-multi` (os/exec + a supervisor
   goroutine per org: health-check, crash-restart w/ backoff, idle-reap) vs. delegate
   to systemd template units (`tenant@<slug>.service`) vs. an orchestrator. Trade
   single-binary portability against offloading lifecycle/cgroup/uid mechanics.
3. **Transport & socket protocol.** Per-org Unix domain socket
   (`<Root>/run/<slug>.sock`, filesystem-permissioned to the org's uid — no port
   allocation, the socket file is itself an access-control point) vs. loopback TCP +
   a port registry. Plus the readiness/health handshake before the proxy goes live,
   and graceful drain on evict.
4. **Cold-start latency mitigation.** Subprocess boot (PB bootstrap + migrations + JS
   compile) is ~100s of ms–seconds vs. in-proc ~ms. Warm pool of N pre-spawned idle
   tenants vs. accept first-request latency + keep-alive TTL. (Note: the cross-org
   `ProgramSource` compiled-program *sharing* is lost in a per-process model — each
   process compiles its own; that's an accepted cost of isolation.)
5. **Secret containment mechanics.** Spawn with a scrubbed, explicit env (no inherited
   `MT_*`); how the tenant process gets *only* what it needs (its orgDir, its socket).
6. **Resource limits.** Which limits, what defaults, per-tenant overrides; cgroup v2
   vs. rlimits; behavior on limit breach (kill + restart backoff, or 503 the tenant).
7. **Control-plane relationship.** The control plane stays in the main process (full
   capability, operator-authored). Provisioning currently runs tenant migrations
   in-process via `bootstrapTenantOnce` — under per-process isolation, provision-time
   migration execution should ALSO move into an isolated one-shot subprocess (it runs
   untrusted migration JS; today it's sandboxed-but-in-the-control-plane-process).

## Non-goals / watch-outs

- Don't let the front router or control-plane schema change as a side effect — the
  `GetOrg → http.Handler` seam is designed to absorb this.
- macOS dev: if a cross-platform stance is chosen, be explicit that the dev fallback
  is NOT a security boundary; only the Linux path is.
- This is a large effort. When picked up, run it through brainstorming (decision #1
  first — it decomposes the rest), then its own spec → plan.
