# Handoff — Multi-Tenant PocketBase

**Date:** 2026-07-22
**Goal:** Make one PocketBase process host many organizations — each org its own
SQLite DB, client JS bundle, and server-side JS handlers — sharing versioned code
where identical, isolated where not.

This documents everything built, where it lives, what's pushed vs. local, and
what remains. **Nothing here is on a public/shared remote except one clean PR
branch** (see [Git state](#git-state)).

---

## TL;DR — current state

The system is **built, wired, and proven end-to-end**: an automated test boots two
isolated orgs that each serve their own JS hook route and share compiled programs
in memory. It is **not yet operator-runnable** — three prerequisites are documented
but unimplemented (superuser bootstrap, tenant schema source, wildcard TLS).

Three bodies of work:

1. **PocketBase fork seams** (`~/code/vendor/pocketbase`) — two additive
   extension points. One is a clean PR branch pushed to your fork; the other is
   staged for a second PR.
2. **Workspace version bump** — PocketBase `v0.38.1 → v0.39.8` across all 8 tinycld
   Go modules. Local commits, unpushed.
3. **The multitenant router** (`~/code/multitenant`) — a new private Go module, 19
   commits, 8 packages, all green + race-clean. Local-only, no remote.

Design spec + implementation plans live at
`~/code/vendor/pocketbase/docs/superpowers/{specs,plans}/2026-07-22-*.md`.

---

## 1. PocketBase fork seams

**Repo:** `~/code/vendor/pocketbase` (a clone of `pocketbase/pocketbase`;
`origin` = upstream, `fork` = `git@github.com:nathanstitt/pocketbase.git`).

Two narrow, backward-compatible, nil-default extension points were added so a
separate (closed) router can embed and multiplex PocketBase apps. Both are
**opt-in — existing single-app behavior is byte-for-byte unchanged.**

### Seam A — `jsvm.ProgramSource` (PR #1, pushed)

An optional hook on `jsvm.Config` letting an embedder share compiled goja programs
across plugin instances:

```go
type ProgramSource interface {
    Compile(name, src string, strict bool) (*goja.Program, error)
}
```

Nil (default) → compile directly with goja. Set → route all hook-file + callback
compilation through it. Hook files compile sloppy (matching `RunScript`);
callbacks compile strict (matching the prior `MustCompile(..., true)` sites).

- **Branch:** `feat/jsvm-programsource` (commit `6644c6af`, based on the released
  tag **`v0.39.8`** + one commit). **Pushed to `fork`** — clean, PR-ready.
- **Files:** `plugins/jsvm/{program_source.go, jsvm.go, binds.go}` + tests.
- The PR body is ready — see [Loose ends](#loose-ends) for where it is.

### Seam B — `apis.BuildServeMux` (PR #2, staged)

Builds an app's `http.Handler` (router + CORS + admin UI + `OnServe`) **without**
starting a server, so an embedder that owns its own `http.Server` can build one mux
per app. `apis.Serve` was refactored to share the base-router construction; its
behavior is unchanged.

- Lives on `feat/jsvm-programsource-buildservemux` (older, v0.38.1-era base — also
  pushed to `fork`, but **stale/bloated**; do not PR as-is).
- **Rebuild it clean off `v0.39.8`** for the real PR #2 (same approach as PR #1:
  branch off `cc4e8570`/`v0.39.8`, apply only the two `apis/serve.go` +
  `build_serve_mux_test.go` changes, no docs).

### Integration branch (what the router actually builds against)

- **Branch:** `feat/multitenant-fork` (commit `2c4bcc31`) = `feat/jsvm-programsource`
  + the two `BuildServeMux` commits cherry-picked. **This is the currently
  checked-out branch**, and the multitenant module's `go.mod replace` points at
  this working tree. Local-only; not for upstream.

**Fork delivery plan (spec D12):** submit both seams as upstream PRs. If accepted,
the downstream `replace` is dropped and everything becomes a plain library import.
Until then, the router builds against the local fork via `replace`.

---

## 2. Workspace version bump: v0.38.1 → v0.39.8

**Why:** the fork seams are based on `v0.39.8`; the tinycld workspace pinned
`v0.38.1`. Bumping aligns everything so the router's `replace` has no version skew.

**Scope:** all 8 Go modules — `tinycld/core/server` + `tinycld/server` (app shell,
same repo) and the 6 feature siblings (`mail/calendar/contacts/drive/text/calc`,
each its own repo).

**Result:** clean bump — **zero source changes**, only `go.mod`/`go.sum`. Every
module builds, tests pass, `go vet` clean; the assembled workspace builds.

- **Each repo is on branch `chore/bump-pocketbase-v0.39.8`** with one commit
  (`tinycld` HEAD `8fff4e4`). **None pushed.**
- **Tooling gotcha (important for future bumps):** feature-sibling modules can't be
  `go mod tidy`'d standalone — they resolve `tinycld.org/core` only via `go.work`
  (untracked, per-developer). Recipe used: `go mod edit -require ...@v0.39.8` →
  temp-add `replace tinycld.org/core => ../../tinycld/core/server` → `GOWORK=off go
  mod tidy` → drop the temp replace. The app shell (`tinycld/server`) additionally
  can't be `GOWORK=off` tidied (it imports generated `tinycld.org/packages/*`); its
  go.mod require was edited directly and validated by the assembled build. Its
  committed `replace` path is `../core/server` (do not let a bump tool rewrite it to
  `../../tinycld/core/server`).

**To ship:** push each repo's bump branch (7 pushes / PRs), or fold into a
coordinated release (see the `release` skill). These are independent of the
multitenant work and can land anytime.

---

## 3. The multitenant router (`~/code/multitenant`)

New **private** Go module `tinycld.org/multitenant`, **19 commits, HEAD `a208907`,
branch `main`, no remote, clean tree.** ~1150 LOC production Go across 8 packages,
all tests green and race-clean.

Imports the fork via `go.mod`:
`replace github.com/pocketbase/pocketbase => /Users/nas/code/vendor/pocketbase`
(the fork must be on `feat/multitenant-fork` — it currently is).

### What it does

One process hosts N orgs. A fronting HTTPS server dispatches by subdomain to a
control-plane PocketBase app (registry + provisioning) or a lazily-loaded tenant
app. Each tenant is stock PocketBase with `pb_hooks`/`pb_public` materialized as
symlink farms from a version-addressed package store; compiled JS hook programs are
shared across orgs via a process-wide cache implementing the fork's `ProgramSource`.

### Package map

| Package | What |
|---|---|
| `internal/store` | Immutable version-addressed package store. |
| `internal/lockfile` | Per-org `{name:version}` lockfile; parse + resolve vs. store. |
| `internal/materialize` | Symlink-farm `pb_hooks` (from `server/`) + `pb_public` (from `client/dist/`). |
| `internal/progcache` | `SharedProgramCache` → the fork's `jsvm.ProgramSource`. |
| `internal/controlplane` | Control-plane app: `orgs/packages/deployments` schema, `Provisioner`, HTTP routes, `OrgLookup`. |
| `internal/orgmanager` | Lazy per-org app loader: materialize → bootstrap PB+jsvm(shared cache) → `BuildServeMux`; singleflight, `Evict`, idle sweeper. |
| `internal/frontrouter` | `Host` → subdomain dispatch. |
| `internal/server` | Single `http.Server` + wildcard autocert + graceful shutdown. |
| `cmd/serve-multi` | Wires it all together. |

### Proven working (the payoff)

`internal/orgmanager/e2e_test.go` — two orgs boot independently, each serves
`/api/health` **and** its own custom `/whoami` JS-hook route (materialized from the
package + run through the shared cache), and loading the second org with identical
hooks **adds zero new compiled programs** (`TestE2E_SecondOrgAddsNoNewPrograms`).
The cross-org memory-sharing win — the whole reason for Seam A — is verified across
the fork boundary.

### How the work was done

All 13 plan tasks implemented via subagent-driven TDD, each with two-stage review
(spec compliance, then code quality). Reviews caught **five real bugs** beyond
typos, all fixed:

1. jsvm hook files compiled strict instead of sloppy → would break existing
   sloppy-mode `.pb.js` (Plan 1).
2. Unrecoverable stranded `provisioning` org row → made `CreateOrg` resumable.
3. `Shutdown` vs in-flight `load` leaked a bootstrapped app → `closed` guard.
4. Idle sweeper evicted every instance because `lastUsed` was never seeded → seed
   at load + skip zero.
5. `Deploy` swallowed the audit-record write error → propagate.

Plus a final holistic review that found the cross-cutting gaps below.

---

## 4. Known gaps & prerequisites (from the final review)

The architecture composes correctly (the hard seams — `OnServe`-before-
`BuildServeMux`, the JSONField lockfile round-trip, cross-org cache sharing — are
all verified right). These stand between "composes" and "operator can run it." They
are also documented in `README.md`.

**Fixed already:** idle eviction now tracks real request activity (`touch()` wired
into `Get`).

**Prerequisites (must close before hosting a real tenant):**

1. **No control-plane superuser** → the provisioning API (`POST /api/orgs`, all
   superuser-guarded) is unusable on a fresh `mt_data`. Add an env-driven
   `create-superuser` step to `cmd/serve-multi`, or create one manually against
   `<MT_ROOT>/pb_control/pb_data`.
2. **No tenant schema source** → `materialize` wires `pb_hooks`/`pb_public` but not
   `pb_migrations`; `CreateOrg` creates that dir empty. Provisioned tenants boot
   with hooks + assets but **no application collections**. This is an open design
   decision: link a package's `pb_migrations/` as a third materialize step, or ship
   JS migrations another way.
3. **Wildcard TLS** → autocert can't issue `*.MT_BASE_DOMAIN` via HTTP-01; supply a
   DNS-01 solver or a pre-issued wildcard cert.

**Cleanup (non-blocking):** store "content-addressed" naming is vestigial
(`ContentHash`/`content_hash`/`manifest` unused — either wire or drop);
`validSlug` accepts reserved `admin`/`www` slugs the router can't serve; no single
test drives the real `CreateOrg → OrgLookup → load` chain (covered transitively);
`lockfile.Resolve` doesn't run the `peerVersions` solver yet (spec §7 follow-on).

---

## 5. Git state (what's where)

| Repo | Branch | HEAD | Pushed? |
|---|---|---|---|
| `~/code/vendor/pocketbase` | `feat/jsvm-programsource` | `6644c6af` | **Yes** (`fork`) — clean PR #1 |
| `~/code/vendor/pocketbase` | `feat/jsvm-programsource-buildservemux` | (older) | Yes (`fork`) — **stale, don't PR** |
| `~/code/vendor/pocketbase` | `feat/multitenant-fork` | `2c4bcc31` | No — local integration (checked out) |
| `~/code/multitenant` | `main` | `a208907` | **No remote** |
| `~/code/tinycld` (core+shell) | `chore/bump-pocketbase-v0.39.8` | `8fff4e4` | No |
| `mail/calendar/contacts/drive/text/calc` | `chore/bump-pocketbase-v0.39.8` | (each) | No |

⚠️ The multitenant module builds against the fork's **working tree**, which must
stay on `feat/multitenant-fork` for the `replace` to see both seams. If you check
out another fork branch, the router won't compile (missing `BuildServeMux`).

---

## 6. Loose ends / next actions

Pick up any of these independently:

- **PR #1 (ProgramSource):** open it from `nathanstitt/pocketbase:feat/jsvm-programsource`
  against `pocketbase/pocketbase`. The PR body was drafted in the working
  conversation (re-generate if lost — it's a short summary of Seam A + the sloppy-
  mode note + backward-compat statement).
- **PR #2 (BuildServeMux):** rebuild a clean branch off `v0.39.8` (the pushed
  `-buildservemux` branch is stale), then PR.
- **Ship the version bump:** push the 7 `chore/bump-pocketbase-v0.39.8` branches
  (or fold into a coordinated release).
- **Close prerequisites #1–#3** (superuser bootstrap, tenant schema, wildcard TLS)
  to make the router operator-runnable.
- **Delete the stale fork branch** `feat/jsvm-programsource-buildservemux` from the
  remote once PR #2's clean branch exists.
- **Give the multitenant module a remote** if it should be shared/CI'd.

---

## 7. Verify the current state

```sh
# Router builds + all tests green + race-clean (fork must be on feat/multitenant-fork):
cd ~/code/multitenant && go build ./... && go vet ./... && go test ./... -count=1 && go test -race ./...

# The core promise:
go test ./internal/orgmanager/ -run TestE2E -v

# Workspace bump holds:
cd ~/code/tinycld/tinycld/core/server && go test ./...
```
