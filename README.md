# multitenant

A single-process, multi-tenant router for [PocketBase](https://pocketbase.io). One
process hosts many organizations: a fronting HTTPS server dispatches by subdomain
to either a **control-plane** PocketBase app (the org/package/deployment registry
+ provisioning API) or a lazily-loaded **tenant** PocketBase app. Each tenant is a
stock PocketBase whose `pb_hooks`/`pb_public` are materialized as symlink farms
from a content-versioned package store, and whose compiled JS hook programs are
**shared across orgs** via a process-wide cache.

> **Private module.** This imports a local PocketBase fork that adds a
> `jsvm.ProgramSource` seam (see [Fork dependency](#fork-dependency)). It is not
> published.

## Architecture

```
                       :443 (wildcard TLS)
                     *.tinycld.org
                          │
        ┌─────────────────▼──────────────────┐
        │ server.Serve → frontrouter          │   subdomain dispatch
        │   admin.*  → control-plane mux       │
        │   <slug>.* → orgmanager.Get(slug)    │
        └───────┬───────────────────┬─────────┘
                │                   │
       ┌────────▼──────┐   ┌────────▼─────────┐
       │ control-plane │   │ OrgManager       │  lazy load + singleflight
       │ PocketBase app│   │  map[slug]*Inst  │  + idle eviction
       │ orgs/packages/│   └────────┬─────────┘
       │ deployments   │            │ per org:
       │ + provisioning│   ┌────────▼─────────┐
       └───────────────┘   │ stock PB app     │
                           │ + jsvm (hooks)   │
                           │ + BuildServeMux  │
        shared, process-wide:                  │
          • store  (content-versioned packages)│
          • progcache (SharedProgramCache →    │
              fork jsvm.ProgramSource)         │
          • materialize (symlink pb_hooks/     │
              pb_public from store+lockfile)   │
```

### Packages

| Package | Responsibility |
|---|---|
| `internal/store` | Immutable, version-addressed package store (`<root>/packages/<name>/<version>/`). |
| `internal/lockfile` | Per-org `{name: version}` lockfile; parse + resolve against the store. |
| `internal/materialize` | Symlink-farm an org's `pb_hooks` (from `server/`) and `pb_public` (from `client/dist/`) from resolved packages. |
| `internal/progcache` | `SharedProgramCache` implementing the fork's `jsvm.ProgramSource` — identical hook source across orgs shares one compiled `*goja.Program`. |
| `internal/controlplane` | Control-plane PocketBase app: `orgs`/`packages`/`deployments` schema, `Provisioner` (create/deploy/suspend/resume/archive/publish), HTTP routes, and the DB-backed `OrgLookup`. |
| `internal/orgmanager` | Lazy per-org app loader: materialize → bootstrap stock PB + jsvm (with shared cache) → `BuildServeMux`. Singleflight-collapsed loads, `Evict`, idle-eviction sweeper. |
| `internal/frontrouter` | Plain `http.Handler`: `Host` → subdomain → control-plane / org / apex-redirect. |
| `internal/server` | Single `http.Server` + wildcard autocert TLS + graceful shutdown. |
| `cmd/serve-multi` | Wires it all together. |

## Running

```sh
go build ./cmd/serve-multi
MT_ROOT=./mt_data MT_BASE_DOMAIN=tinycld.org MT_ADDR=:443 ./serve-multi
```

Env: `MT_ROOT` (data root, default `./mt_data`), `MT_BASE_DOMAIN` (default
`tinycld.org`), `MT_ADDR` (default `:443`).

## Tenant JS security boundary

Tenant hooks and migrations run under the fork's jsvm **Sandboxed** mode
(`jsvm.Config{Sandboxed: true}`): a deny-by-default allowlist that withholds
the host-reaching bindings — `$os` (exec / env / raw filesystem), `$http`
(outbound HTTP), `$filesystem`, and `$filepath` — from both the hook and
migration runtimes; neuters `process.env` / `process.argv` so the host
environment (e.g. `MT_SUPERUSER_PASSWORD`) is unreachable; withholds
`$apis.static` (a raw directory-serving primitive whose root is caller-chosen);
denies file-based `require()` (only native modules like `process`/`console`/
`buffer` load); and restricts `$template` to `loadString` (no `loadFiles`/
`loadFS`). Both the runtime load path (`orgmanager.load`) and the provision
path (`controlplane.bootstrapTenantOnce`) enable it, and both use
`jsvm.Register` (returning the error) rather than `MustRegister`, so a hook or
migration that throws at load fails only that one org's load/provisioning
instead of panicking the shared process.

> **⚠️ This is blast-radius reduction, NOT attacker containment.** It is the
> WordPress `disable_functions` / `open_basedir` tier: it raises the bar but
> does not hold against a determined hostile author in a shared process.
>
> **Demonstrated in-process bypass (open):** `$app.db()` hands tenant JS a raw
> SQL surface over the shared connection. A sandboxed hook can run
> `ATTACH DATABASE '<other-org>/data.db'` inside a transaction and read another
> org's secrets (and create arbitrary `.db` files) — arbitrary host-file
> read/write, reaching the same headline threat the binding allowlist set out
> to close. Our SQLite driver (modernc) exposes **no authorizer API**, so this
> class cannot be cleanly contained in-process; `$app` similarly exposes
> `newFilesystem`, `createBackup`/`restoreBackup`, etc. The recurring pattern —
> each audit finding another capability re-entering through a *kept* surface —
> is the signal that **allowlisting the full stock `$app`/DB API against a
> hostile author in one shared process is the wrong altitude.**
>
> Also unaddressed in-process: sobek engine escapes, and CPU / memory /
> wall-clock DoS (no resource limits).

**The actual security boundary is OS-level per-process isolation, and it is now
the required next deliverable** (not an optional hardening). Each org's app must
run in its own process confined to its own directory (per-uid + a filesystem
namespace/chroot so `ATTACH '<abs path>'` physically fails at the OS layer),
with cgroup CPU/memory limits and a scrubbed environment. That layer confines
filesystem, DB, resource, and *unknown-future* vectors uniformly — which
in-process allowlisting provably cannot. The `GetOrg → http.Handler` seam in
`frontrouter` is where a reverse-proxy-to-subprocess drops in without reworking
dispatch. **Until it lands, do not treat tenant authors as untrusted in
production.**

## Operator prerequisites & known gaps

This module composes correctly at the transport, caching, and provisioning layers
(the end-to-end test boots two isolated orgs that serve their own JS hook routes
and share the program cache). The following must be handled before it hosts a real
tenant. They are deliberately out of the initial implementation scope.

### 1. Control-plane superuser (required to use the provisioning API)

Every provisioning route (`POST /api/orgs`, `/deploy`, etc.) is guarded by
`apis.RequireSuperuserAuth()`. A fresh `mt_data` has **no superuser**, so nothing
can call them. Before provisioning any org, create one against the control-plane
data dir, e.g. with a PocketBase superuser command against
`<MT_ROOT>/pb_control/pb_data`, or add an env-driven bootstrap step to
`cmd/serve-multi` (e.g. create a superuser from `MT_ADMIN_EMAIL`/`MT_ADMIN_PASSWORD`
on first boot). **Not yet wired.**

### 2. Tenant application schema (where tenant collections come from)

`materialize` wires a package's `server/*.pb.js` → `pb_hooks` and
`client/dist/**` → `pb_public`. It does **not** populate a tenant's
`pb_migrations`, and `CreateOrg` creates that dir empty. So a freshly provisioned
tenant boots as a stock PocketBase with hooks and static assets but **no
application collections**. Deciding how tenant schema is provisioned — a third
materialize link step from a package `pb_migrations/`, or JS migrations shipped
some other way — is an open design decision. **Not yet implemented.**

### 3. Wildcard TLS

`server.Serve` uses autocert with a permissive `HostPolicy`. A wildcard
`*.MT_BASE_DOMAIN` certificate **cannot** be issued via HTTP-01; operators must
supply a DNS-01 solver or a pre-issued wildcard cert. The autocert cache lives at
`./pb_control/autocert`.

### Minor / cleanup

- **Store naming:** `store.ContentHash` / the `packages.content_hash` field / the
  "content-addressed" wording are vestigial — the store resolves by
  `<name>/<version>`, and `content_hash`/`manifest` are never populated by
  `PublishPackage`. Either wire content hashing or drop the vocabulary.
- **Reserved subdomains:** `validSlug` accepts `admin` and `www`, but the front
  router routes those to the control plane / apex redirect — so an org created
  with such a slug would be unreachable. Add the router's reserved labels to the
  slug-rejection set.
- **Integration test:** the `CreateOrg → OrgLookup → orgmanager.load` chain (a real
  control-plane record driving a real org boot) is verified by hand and covered
  transitively, but has no single dedicated test; the e2e test uses a stub lookup.
- **peerVersions solver:** `lockfile.Resolve` only checks that referenced versions
  exist in the store. The full `peerVersions` compatibility solver (spec §7) is a
  follow-on the `CreateOrg`/`Deploy` path can call before materializing.

## Fork dependency

`go.mod` has:

```
replace github.com/pocketbase/pocketbase => /Users/nas/code/vendor/pocketbase
```

The router builds against a local PocketBase fork checked out on a branch that
adds two seams:

- **`jsvm.ProgramSource`** — the optional hook cache the `SharedProgramCache`
  implements (the cross-org memory-sharing win; proven by
  `TestE2E_SecondOrgAddsNoNewPrograms`).
- **`apis.BuildServeMux`** — builds a per-app `http.Handler` without starting a
  server, so the router can mount one mux per org.

`ProgramSource` is being upstreamed as its own PR; `BuildServeMux` is a planned
second PR. When both land in an upstream PocketBase release, drop the `replace`
and require that release — the router's use of PocketBase is otherwise a plain
library import.
