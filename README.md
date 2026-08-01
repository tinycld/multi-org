# multitenant

A multi-tenant router for [PocketBase](https://pocketbase.io). A fronting HTTPS
server dispatches by subdomain to either a **control-plane** PocketBase app (the
org/deployment registry + provisioning API, in-process) or a lazily
spawned **tenant process**. Each tenant is a per-org build artifact (a stock
PocketBase linking exactly the org's package set) running in its own OS
process — confined to its own uid, mount and PID namespaces, and cgroup on
Linux — whose `pb_hooks`/`pb_public`/`pb_migrations` are symlinks into a
committed, content-addressed build under `<root>/builds/<recipe-hash>/`. The
router reaches each tenant over a per-org unix domain socket and never holds a
tenant app object.

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
       │ orgs/         │   └────────┬─────────┘
       │ deployments   │            │ ReverseProxy over
       │ + provisioning│            │ <root>/run/<slug>/<slug>.sock
       └───────┬───────┘   ┌────────▼─────────┐
               │           │ artifact process │  ← OS boundary
      trusted  │           │  own uid + mount │
      builder  │ repoint   │  + pid ns        │
    (per-recipe│  live     │  the org's own   │
     artifacts │  trees at │  dual-mode build │
     under      │ builds/  │  (Sandboxed jsvm │
     builds/    │ <hash>/  │   + Card/Cal/Web │
     <hash>/)   ▼          │    DAV)          │
    ────────────────────── └──────────────────┘
                             one process per org
```

### Packages

| Package | Responsibility |
|---|---|
| `internal/lockfile` | Per-org `{name: version}` package set; parse + marshal. Always names the app shell (`tinycld`) — the set is built into an artifact. |
| `internal/materialize` | Point an org's live `pb_hooks`/`pb_public`/`pb_migrations` names at a committed build artifact's trees (atomic symlink swap). |
| `internal/controlplane` | Control-plane PocketBase app: `orgs`/`deployments`/`control_settings` schema, `Provisioner` (create/suspend/resume/archive), the `Deployer` (the D6 deploy orchestrator: per-org serialization + rate limit, build → repoint → respawn with commit/revert, `.runtime/deploy-result.json`), HTTP routes, and the DB-backed `OrgLookup`. |
| `internal/orgmanager` | Lazy per-org process supervisor: repoint the live trees at the org's committed build → spawn the artifact's own dual-mode binary → readiness handshake → reverse proxy. Singleflight-collapsed spawns, crash supervision with backoff, drain-then-kill `Evict`, idle sweeper, and the router-bound per-org control socket (`ctl.sock`) tenants propose deploys over. |
| `internal/orgerr` | The three sentinels (`ErrOrgNotFound` / `ErrOrgNotActive` / `ErrOrgUnavailable`) the front router classifies into 404 / 503. |
| `internal/builder` | The trusted builder (DESIGN-org-package-agency §7 step 2): resolves a package set on the trusted side (tarball integrities + manifest facts → recipe hash), runs the shared `pkgbuild` pipeline in a confined re-exec'd job, and commits the runtime tree to the content-addressed cache at `<root>/builds/<recipe-hash>/` (idempotent commit, refcount-style `Sweep`). |
| `internal/frontrouter` | Plain `http.Handler`: `Host` → subdomain → control-plane / org / apex org-finder page. |
| `internal/webpage` | Branded HTML pages (cold-start/restart interstitials, unknown-org, apex org finder) + JSON error bodies for non-browser clients. |
| `internal/server` | Single `http.Server` + wildcard autocert TLS + graceful shutdown. |
| `cmd/serve-multi` | The router. Wires it all together — the only binary this module builds. |

The tenant binary is NOT built here. Each org runs its **own artifact binary**
— the app shell's dual-mode `main` (`tinycld.org/tinycld`), which dispatches to
core's `tenantmain` transport on `--org-dir` and registers its linked feature Go
unconditionally (the artifact is the gate). The builder produces one per recipe
and commits it into `builds/<hash>/tinycld`; for dev, build it standalone from
the `tinycld` sibling (`cd ../tinycld/server && go build -o … .`).

## Running

The router is the only binary to build here; it spawns each org's own artifact
binary:

```sh
go build -o bin/ ./cmd/serve-multi
MT_ROOT=./mt_data MT_BASE_DOMAIN=tinycld.org MT_ADDR=:443 ./bin/serve-multi
```

| Env | Default | Purpose |
|---|---|---|
| `MT_ROOT` | `./mt_data` | Data root: build-artifact cache (`builds/`), control plane, per-org dirs. |
| `MT_BASE_DOMAIN` | `tinycld.org` | Subdomain dispatch base. |
| `MT_ADDR` | `:443` | Listen address. |
| `MT_TLS_MODE` | `proxy` | `proxy` / `file` / `autocert`. |
| `MT_TLS_CERT`, `MT_TLS_KEY` | — | Required for `file`. |
| `MT_SUPERUSER_EMAIL`, `MT_SUPERUSER_PASSWORD` | — | Upserts the control-plane superuser on boot. Without it every provisioning route 401s. |
| `MT_MAX_RESIDENT_ORGS` | — | Cap on resident tenant processes. When full, the least-recently-used idle org is evicted to admit a newcomer; if every resident org has live connections the newcomer gets 503. Unset ⇒ unlimited — one request per enumerable slug then holds every org resident. |
| `MT_MAX_CONCURRENT_SPAWNS` | `4` | Cap on simultaneous cold starts (each runs migrations + hook compilation). Excess loads wait, bounded by the spawn timeout. |
| `MT_TENANT_UID_BASE`, `MT_TENANT_UID_RANGE` | — | **Linux.** The uid window tenants are mapped into. Unset ⇒ tenants run as the host user and are **not** confined. |
| `MT_CGROUP_ROOT` | — | **Linux.** cgroup v2 dir to place tenants under. |
| `MT_TENANT_MEMORY_MAX` | — | **Linux.** Per-tenant `memory.max`: bytes with optional `K`/`M`/`G`/`T` suffix (e.g. `512M`), or `max`. Unset ⇒ unlimited. |
| `MT_TENANT_PIDS_MAX` | — | **Linux.** Per-tenant `pids.max`: positive integer (e.g. `256`), or `max`. Unset ⇒ unlimited. |
| `MT_TENANT_CPU_MAX` | — | **Linux.** Per-tenant CPU as cores, a positive decimal (e.g. `1.5`), or `max`. Written as `cpu.max` quota against a 100ms period. Unset ⇒ unlimited. |
| `MT_TENANT_DISK_MAX` | — | **Linux.** Per-tenant hard filesystem quota (bytes with optional `K`/`M`/`G`/`T`), applied to the tenant uid via `quotactl` — the kernel backstop against package Go writing past its plan by bypassing `app.Save`. Distinct from the per-org soft `storage_limit_bytes`: set it comfortably ABOVE any plan so the app-layer "over limit" error fires first. Requires the filesystem holding `MT_ROOT` to be mounted with user quotas (`usrquota`); a quota-less fs leaves the tenant unbounded with a warning. Unset ⇒ no kernel quota. |
| `MT_MAIL_PORTS_ENABLED` | — | `true` boots the mail router: IMAPS/SMTPS demuxed to orgs by TLS SNI (`<slug>.<base>`), the MX routed by RCPT TO via the `org_mail_domains` registry. Requires a TLS source — enabled-but-certless is a boot error. |
| `MT_MAIL_TLS_CERT`, `MT_MAIL_TLS_KEY` | falls back to `MT_TLS_CERT`/`MT_TLS_KEY` | Wildcard cert the router terminates mail TLS with. Tenants never hold this key. |
| `MT_IMAPS_ADDR`, `MT_SMTPS_ADDR`, `MT_MX_ADDR` | `:993`, `:465`, `:25` | Mail listener addresses; the literal value `off` disables that one listener. |
| `MT_MX_HOSTNAME` | `MT_BASE_DOMAIN` | Identity the `:25` greeting announces and the HELO name toward tenants. |
| `MT_SCAFFOLD_ROOT` | — | Enables the **trusted builder**: an operator-provisioned workspace scaffold root (`package-versions.json`, `scripts/link-members.ts`, …; bootstrap is its source of truth). Set, package sets are built into shared artifacts under `<root>/builds/<recipe-hash>/`. **Required to provision or deploy** — unset, the router still serves orgs whose artifacts were built earlier but refuses any new build. |
| `MT_BUILDER_MAX_CONCURRENT` | `1` | Cap on simultaneously-running build jobs; excess queue. Builds are minutes of CPU-saturating work — the queue is the capacity seam. |
| `MT_BUILDER_UID` | — | **Linux.** Dedicated host uid build jobs run as (outside the tenant uid window). Unset ⇒ jobs run unconfined. |
| `MT_BUILDER_CGROUP_ROOT` | — | **Linux.** cgroup v2 dir to place build jobs under. |
| `MT_BUILDER_MEMORY_MAX`, `MT_BUILDER_PIDS_MAX`, `MT_BUILDER_CPU_MAX` | — | **Linux.** Per-job cgroup limits, same value syntax as the `MT_TENANT_*` counterparts. |

None of these reach a tenant process: children are spawned with an explicitly
constructed environment (see the security section).

## Artifact-backed orgs & the deploy protocol

Every org is artifact-backed. A lockfile always includes the app shell
(`"tinycld": "<version>"`); the trusted builder fetches every member from its
registry spec, computes the recipe hash, runs the `pkgbuild` pipeline in a
confined job, and commits the runtime tree to `<root>/builds/<recipe-hash>/`.
The org row stores that hash (`orgs.recipe_hash`); at load the manager points
the org's `pb_hooks`/`pb_migrations`/`pb_public` into the artifact (atomic
symlink swap), boots the artifact's **own dual-mode binary**, and feeds the
capability wiring from the artifact's staged
`manifests/<slug>/manifest.json`. Two orgs with the same set share the entry
by construction; an hourly sweep removes artifacts no org's current or
previous build references.

`POST /api/orgs` with **no** lockfile copies the operator-editable template
(`PUT /api/settings/default-lockfile`) — the default set is just a warm cache
entry, so provisioning costs seconds. A lockfile that omits the app shell is
rejected: there is no package set that isn't built.

Deploys (operator `POST /api/orgs/{slug}/deploy`, or the tenant itself over
its **control socket** — `ctl.sock` in the org's 0700 socket dir, router-bound
and tenant-dialed, so identity is the filesystem):

1. `POST /v1/build {lockfile}` — build or cache-hit the artifact and wait.
   The org keeps serving its old build; novel sets pay their minutes here.
2. Tenant snapshots its DB to `.deploy/backup.db`, runs its down migrations,
   then `POST /v1/deploy {lockfile, jobId}` — 202 once the proposal is
   recorded and the org row repointed; the tenant waits to be killed.
3. Router evicts and respawns; the readiness handshake is the commit/revert
   decision. Failure restores the snapshot, repoints the previous build,
   respawns it, and records the reason in `.runtime/deploy-result.json`
   (shape owned by core's `tenantcfg`) plus the `deployments` row.

Per-org deploys are serialized (busy → 409) and rate-limited (→ 429).

### Mail domains

Inbound MX routing is keyed by the `org_mail_domains` registry — which org owns
a recipient domain. The provisioning API takes a **slug**, not the underlying
orgs relation id:

```sh
curl -X POST .../api/orgs/acme/mail-domains -d '{"domain":"acme-corp.com"}'   # 201
curl      -X GET    .../api/orgs/acme/mail-domains                            # {"domains":[…]}
curl      -X DELETE .../api/orgs/acme/mail-domains/acme-corp.com              # 204
```

Superuser-only, like every other provisioning route: a domain claim is
exclusive (unique index), so letting an org claim its own would let it deny
that domain to every other org — and claiming a sibling's domain would steal
its mail. Input is lowercased before the write (the collection's pattern
*rejects* uppercase rather than normalizing, since the relay compares
case-folded against canonical storage). Removal is scoped to the owning org, so
a mistyped slug cannot release someone else's domain.

This governs **inbound MX (`:25`) only**. IMAPS/SMTPS still demux by TLS SNI on
`<slug>.<base>`, so an org receiving mail at `@acme-corp.com` still connects its
clients to `acme.tinycld.org`. Pointing the domain's MX record at
`MT_MX_HOSTNAME` is the customer's DNS work — registering the domain here does
not do it.

## Tenant security boundary

**The boundary is the OS process.** Each org runs in its own artifact binary
process (the app shell's dual-mode `main` in tenant mode), and on Linux that
process is confined: its own uid (so another org's
`pb_data` is unreadable by the kernel's own rules), its own mount and PID
namespaces, and its own cgroup. The build-artifact store (`<root>/builds/`) is
bind-mounted read-only at its real absolute path, because the org's live
`pb_hooks`/`pb_migrations`/`pb_public` are absolute symlinks into its committed
artifact — a naive `chroot` to the org directory would break every one of
them. Committed artifacts are owned by root (reclaimed from the build-job uid
at commit time), so a build job running package-author code cannot tamper with
another org's already-committed binary.

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

- **Reserved subdomains:** `validSlug` rejects `admin` and `www`
  (`provisioning.go`), matching what the front router claims for the control
  plane / apex org-finder page.
- **peerVersions solver:** the authoritative `peerVersions` gate runs inside
  `builder.Build` — the confined job re-checks every resolved member's ranges
  against the freshly-fetched manifests before it commits an artifact, so a
  `CreateOrg`/deploy of an incompatible set fails the build. A required peer
  missing from the set and an unparsable range are violations too (fail
  closed). The tenant's hosted Packages UI runs an advisory pre-flight solve
  from its `pkg_registry` manifests first (the build re-checks it).
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
`core.ServeEvent.Listener` verbatim when set, which is how the tenant binary
serves on a unix socket without any fork change.

When `BuildServeMux` lands upstream, drop the `replace` and require that release.
