# multitenant

A multi-tenant router for [PocketBase](https://pocketbase.io). A fronting HTTPS
server dispatches by subdomain to either a **control-plane** PocketBase app (the
org/package/deployment registry + provisioning API, in-process) or a lazily
spawned **tenant process**. Each tenant is a stock PocketBase running in its own
OS process — confined to its own uid, mount and PID namespaces, and cgroup on
Linux — whose `pb_hooks`/`pb_public` are materialized as symlink farms from a
version-addressed package store. The router reaches each tenant over a per-org
unix domain socket and never holds a tenant app object.

> **Private module.** This imports a local PocketBase fork (see
> [Fork dependency](#fork-dependency)). It is not published.

## Architecture

```
                       :443 (wildcard TLS)
                     *.tinycld.org
                          │
        ┌─────────────────▼──────────────────┐
        │ server.Serve → frontrouter          │   subdomain dispatch
        │   admin.*  → control-plane mux       │
        │   <slug>.* → orgmanager.Get(ctx,slug)│
        └───────┬───────────────────┬─────────┘
                │                   │
       ┌────────▼──────┐   ┌────────▼─────────┐
       │ control-plane │   │ OrgManager       │  lazy spawn + singleflight
       │ PocketBase app│   │  map[slug]*Inst  │  + supervise + idle eviction
       │ orgs/packages/│   └────────┬─────────┘
       │ deployments   │            │ ReverseProxy over
       │ + provisioning│            │ <root>/run/<slug>/<slug>.sock
       └───────────────┘   ┌────────▼─────────┐
                           │ serve-org process│  ← OS boundary
        host-side, shared: │  own uid + mount │
          • store          │  + pid ns        │
          • materialize    │  stock PB + jsvm │
            (symlinks      │  (Sandboxed)     │
             pb_hooks/     │  + Card/Cal/Web  │
             pb_public)    │    DAV           │
                           └──────────────────┘
                             one process per org
```

### Packages

| Package | Responsibility |
|---|---|
| `internal/store` | Immutable, version-addressed package store (`<root>/packages/<name>/<version>/`). |
| `internal/lockfile` | Per-org `{name: version}` lockfile; parse + resolve against the store. |
| `internal/materialize` | Symlink-farm an org's `pb_hooks` (from `server/`) and `pb_public` (from `client/dist/`) from resolved packages. |
| `internal/controlplane` | Control-plane PocketBase app: `orgs`/`packages`/`deployments` schema, `Provisioner` (create/deploy/suspend/resume/archive/publish), HTTP routes, and the DB-backed `OrgLookup`. |
| `internal/orgmanager` | Lazy per-org process supervisor: materialize → spawn `serve-org` → readiness handshake → reverse proxy. Singleflight-collapsed spawns, crash supervision with backoff, drain-then-kill `Evict`, idle sweeper. |
| `internal/orgerr` | The three sentinels (`ErrOrgNotFound` / `ErrOrgNotActive` / `ErrOrgUnavailable`) the front router classifies into 404 / 503. |
| `internal/davconfig` | JSON wire formats for the runtime config the host hands each tenant: CardDAV / CalDAV / WebDAV source lists and quota sources. |
| `internal/frontrouter` | Plain `http.Handler`: `Host` → subdomain → control-plane / org / apex-redirect. |
| `internal/server` | Single `http.Server` + wildcard autocert TLS + graceful shutdown. |
| `cmd/serve-multi` | The router. Wires it all together. |
| `cmd/serve-org` | The tenant. One org on a unix socket: `coreserver.RegisterTenant` (core guards, sandboxed jsvm, CardDAV/CalDAV/WebDAV, quota) plus the pinned feature-Go menu (`internal/tenantpkgs`), gated by the org's resolved package set. |

## Running

Both binaries are required — the router spawns the tenant one:

```sh
go build -o bin/ ./cmd/serve-multi ./cmd/serve-org
MT_ROOT=./mt_data MT_BASE_DOMAIN=tinycld.org MT_ADDR=:443 ./bin/serve-multi
```

`serve-org` is resolved next to the `serve-multi` executable by default, so a
deployed pair stays together without configuration.

| Env | Default | Purpose |
|---|---|---|
| `MT_ROOT` | `./mt_data` | Data root: package store, control plane, per-org dirs. |
| `MT_BASE_DOMAIN` | `tinycld.org` | Subdomain dispatch base. |
| `MT_ADDR` | `:443` | Listen address. |
| `MT_TLS_MODE` | `proxy` | `proxy` / `file` / `autocert`. |
| `MT_TLS_CERT`, `MT_TLS_KEY` | — | Required for `file`. |
| `MT_SUPERUSER_EMAIL`, `MT_SUPERUSER_PASSWORD` | — | Upserts the control-plane superuser on boot. Without it every provisioning route 401s. |
| `MT_TENANT_BINARY` | sibling `serve-org` | Override the tenant executable path. |
| `MT_MAX_RESIDENT_ORGS` | — | Cap on resident tenant processes. When full, the least-recently-used idle org is evicted to admit a newcomer; if every resident org has live connections the newcomer gets 503. Unset ⇒ unlimited — one request per enumerable slug then holds every org resident. |
| `MT_MAX_CONCURRENT_SPAWNS` | `4` | Cap on simultaneous cold starts (each runs migrations + hook compilation). Excess loads wait, bounded by the spawn timeout. |
| `MT_TENANT_UID_BASE`, `MT_TENANT_UID_RANGE` | — | **Linux.** The uid window tenants are mapped into. Unset ⇒ tenants run as the host user and are **not** confined. |
| `MT_CGROUP_ROOT` | — | **Linux.** cgroup v2 dir to place tenants under. |
| `MT_TENANT_MEMORY_MAX` | — | **Linux.** Per-tenant `memory.max`: bytes with optional `K`/`M`/`G`/`T` suffix (e.g. `512M`), or `max`. Unset ⇒ unlimited. |
| `MT_TENANT_PIDS_MAX` | — | **Linux.** Per-tenant `pids.max`: positive integer (e.g. `256`), or `max`. Unset ⇒ unlimited. |
| `MT_TENANT_CPU_MAX` | — | **Linux.** Per-tenant CPU as cores, a positive decimal (e.g. `1.5`), or `max`. Written as `cpu.max` quota against a 100ms period. Unset ⇒ unlimited. |
| `MT_MAIL_PORTS_ENABLED` | — | `true` boots the mail router: IMAPS/SMTPS demuxed to orgs by TLS SNI (`<slug>.<base>`), the MX routed by RCPT TO via the `org_mail_domains` registry. Requires a TLS source — enabled-but-certless is a boot error. |
| `MT_MAIL_TLS_CERT`, `MT_MAIL_TLS_KEY` | falls back to `MT_TLS_CERT`/`MT_TLS_KEY` | Wildcard cert the router terminates mail TLS with. Tenants never hold this key. |
| `MT_IMAPS_ADDR`, `MT_SMTPS_ADDR`, `MT_MX_ADDR` | `:993`, `:465`, `:25` | Mail listener addresses; the literal value `off` disables that one listener. |
| `MT_MX_HOSTNAME` | `MT_BASE_DOMAIN` | Identity the `:25` greeting announces and the HELO name toward tenants. |

None of these reach a tenant process: children are spawned with an explicitly
constructed environment (see the security section).

## Tenant security boundary

**The boundary is the OS process.** Each org runs in its own `serve-org`
process, and on Linux that process is confined: its own uid (so another org's
`pb_data` is unreadable by the kernel's own rules), its own mount and PID
namespaces, and its own cgroup. The package store is bind-mounted read-only at
its real absolute path, because `materialize` fills `pb_hooks`/`pb_migrations`
with absolute symlinks into it — a naive `chroot` to the org directory would
break every one of them.

This closes the class of exploit that in-process allowlisting provably could
not. The motivating case: `$app.db()` hands tenant JS a raw SQL surface, so a
sandboxed hook could run `ATTACH DATABASE '<other-org>/data.db'` and read
another org's secrets. Our SQLite driver (modernc) exposes no authorizer API, so
that could not be denied at the driver layer — but a process that cannot open
the file at all does not need to. The same boundary covers filesystem, resource,
and *unknown-future* vectors uniformly, which is the point: four successive
audits each found another capability re-entering through a *kept* binding, and
that pattern is what per-process isolation ends.

**There is deliberately no network namespace.** Tenants make legitimate
outbound connections — calendar ICS subscription fetches, mail provider APIs,
Sentry — so `CLONE_NEWNET` without veth/NAT plumbing would break shipped
features for no boundary gain (`$http` is already withheld by the sandbox as
defence in depth, and the DB/filesystem boundary does not depend on the
network). If per-tenant egress policy is ever wanted, it is an operator
firewall concern, not a namespace one.

`TestConfinement_*` in `internal/orgmanager` asserts these properties directly
(cross-org `ATTACH` fails, tenants run as distinct non-root uids, host secrets
are absent from the child environment). **They require Linux and root**; they
run as root in CI (`.github/workflows/confinement.yml`), and on a macOS dev box
`-run TestConfinement` prints an explicit skip stub — see the caveat below.

### Retained in-process hardening (defence in depth)

The fork's jsvm **Sandboxed** mode stays on inside every tenant process
(`jsvm.Config{Sandboxed: true}`): it withholds `$os`, `$http`, `$filesystem` and
`$filepath` from both the hook and migration runtimes, neuters `process.env` /
`process.argv`, withholds `$apis.static`, denies file-based `require()`, and
restricts `$template` to `loadString`. It is cheap and it narrows the blast
radius, but it is no longer what the security model rests on.

Tenants are also spawned with an environment built from an explicit allowlist
rather than a filter over `os.Environ()`, so host secrets (`MT_SUPERUSER_PASSWORD`,
`MT_TLS_KEY`, and anything added later) are excluded by construction.

`jsvm.Register` is used rather than `MustRegister`, so a hook or migration that
throws at load fails that one org's spawn — reported to the router over the
readiness pipe — instead of panicking anything shared.

> **⚠️ macOS is not a security boundary.** Namespaces, cgroups and uid
> separation are Linux-only. The darwin spawner runs a plain subprocess and logs
> a warning saying so. It exists so the router runs on a development machine;
> **do not host untrusted tenants on it.**

Provisioning runs no tenant JS in the control-plane process. `POST /api/orgs`
materializes the org, flips it active, and then **boots the tenant through the
org manager** to verify it: the first spawn applies the org's migrations inside
the confined tenant process (`apis.Serve` runs them before the readiness
report), and a failure travels back through the readiness pipe with the child's
reason, rolling the org back to `provisioning` for a retried create to resume.
The control plane never opens a tenant app.

### Process hygiene

`serve-multi` puts all of its work in `run() error` and keeps `log.Fatal` in
`main` alone. That is load-bearing, not style: `log.Fatal` and `os.Exit` skip
deferred functions, so calling either after a tenant could exist orphans every
child process — they outlive the router and keep holding its port and sockets.
Any new failure path must `return err`, never exit in place.

### Resource limits

When `MT_CGROUP_ROOT` is set, each tenant is placed in its own cgroup v2 group
with the configured `MT_TENANT_MEMORY_MAX` / `MT_TENANT_PIDS_MAX` /
`MT_TENANT_CPU_MAX` limits written **before** the pid, so a tenant never runs
unlimited inside its group. There are deliberately no default limits: unset
means unlimited, and the spawner warns loudly when a cgroup root is configured
with no limits at all (the group then constrains nothing) or limits are set
with no cgroup root (nothing will ever apply them). An invalid value is logged
at Error and treated as unset — a typo can't take every org down, but it can't
pass silently either. If a limit cannot be written (controller not delegated),
the whole placement fails with a warning and the tenant runs outside the
cgroup: the same enforcement (none), stated honestly, instead of a group that
looks confining and is not. `TestConfinement_CgroupLimitsApplied` proves the
kernel accepts the written limits.


## Operator prerequisites & known gaps

This module composes correctly at the transport, isolation, and provisioning
layers (the end-to-end tests spawn two real tenant processes that serve their own
JS hook routes and their own CardDAV over separate sockets). The following must
still be handled before it hosts a real tenant.

### 1. Linux CI for the confinement tests — DONE

The `confinement` workflow (`.github/workflows/confinement.yml`) clones the
sibling repos, runs the full suite, and runs `TestConfinement_*` as root on
every push/PR. Its first run caught a live bug (non-root spawns EPERM'd),
which is exactly the job it exists to do.

### 2. Wildcard TLS

`server.Serve` uses autocert with a permissive `HostPolicy`. A wildcard
`*.MT_BASE_DOMAIN` certificate **cannot** be issued via HTTP-01; operators must
supply a DNS-01 solver or a pre-issued wildcard cert. The autocert cache lives at
`./pb_control/autocert`.

### Minor / cleanup

- **Store naming:** `store.ContentHash` / the `packages.content_hash` field / the
  "content-addressed" wording are vestigial — the store resolves by
  `<name>/<version>`, and `content_hash`/`manifest` are never populated by
  `PublishPackage`. Either wire content hashing or drop the vocabulary.
- **Reserved subdomains:** `validSlug` rejects `admin` and `www`
  (`provisioning.go`), matching what the front router claims for the control
  plane / apex redirect.
- **peerVersions solver:** `lockfile.Resolve` stays a pure store lookup;
  `controlplane.CheckPeerVersions` (compat.go) enforces every resolved
  package's `peerVersions` ranges — `CreateOrg` and `Deploy` both refuse to
  materialize an incompatible set. A required peer missing from the lockfile
  and an unparsable range are violations too (fail closed).
- **Cold start:** a tenant boot is PocketBase bootstrap + migrations + JS compile,
  i.e. hundreds of ms to seconds, paid by the first request after the 30-minute
  idle sweep evicts an org. There is no warm pool. Cross-org compiled-program
  sharing is also gone by construction (each process compiles its own) — that is
  the accepted cost of isolation.
- **Per-org memory:** N resident orgs are now N PocketBase processes rather than N
  apps in one heap. This, not CPU, is what bounds orgs-per-host.

## Fork dependency

`go.mod` has:

```
replace github.com/pocketbase/pocketbase => ../tinycld/third_party/pocketbase
```

That is a **vendored copy** of the fork (Go sources byte-identical to commit
`a37c8ac` of `feat/multitenant-fork`, `ui/` stripped). The fork's own checkout
at `~/code/tinycld/pocketbase` is for fork development and upstreaming; edits
there reach the router only once vendored.

The fork (branch `feat/multitenant-fork`) adds two seams this module still uses:

- **`apis.BuildServeMux`** — builds a per-app `http.Handler` without starting a
  server. Used for the **control plane**, which shares the router's process with
  the front server. Tenants no longer need it: each serves its own listener in
  its own process via stock `apis.Serve`.
- **`jsvm.Config.Sandboxed`** — the deny-by-default binding allowlist, retained
  as defence in depth inside each tenant process.

**`jsvm.ProgramSource` no longer has a consumer here.** It existed to share
compiled hook programs across orgs in one process; per-process isolation ends
that by construction (a `*sobek.Program` is a Go heap object with interior
pointers, and sobek has no bytecode serialization, so there is nothing to share
across an address-space boundary). It is still worth upstreaming on its own
merits for single-process embedders — just note this repo no longer exercises it.

Tenants also rely on one piece of **upstream** behaviour: `apis.Serve` uses
`core.ServeEvent.Listener` verbatim when set, which is how `serve-org` serves on
a unix socket without any fork change.

When `BuildServeMux` lands upstream, drop the `replace` and require that release.
