package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"tinycld.org/core/pkgbuild"
	"tinycld.org/core/tenantcfg"
	"tinycld.org/multi-org/internal/builder"
	"tinycld.org/multi-org/internal/lockfile"
)

// Deployer executes the router side of the deploy protocol (design D6): build
// (or cache-hit) the proposed set's artifact, record the deployment, repoint
// the org row, evict, respawn, and commit — or restore the tenant's snapshot,
// repoint the previous build, and record the failure durably in
// .runtime/deploy-result.json.
//
// Per-org deploys are SERIALIZED (an org stays busy from proposal until its
// respawn settles) and rate-limited: tenants can propose churn the load
// singleflight does not cover.
// ArtifactBuilder is the builder surface the deployer consumes
// (*builder.Builder satisfies it; tests inject fakes). ResolveSpec backs the
// control socket's /v1/resolve — the hosted tenant has no toolchain, so spec
// metadata resolution happens on the router's trusted side.
type ArtifactBuilder interface {
	Build(ctx context.Context, refs []builder.PackageRef, sink pkgbuild.ProgressSink) (builder.Result, error)
	ResolveSpec(spec string) (builder.ResolvedSpec, error)
	// NpmRegistry is the builder's registry override ("" = npm default);
	// /v1/versions discovers through it so version listings come from the
	// same universe member fetches resolve against.
	NpmRegistry() string
}

type Deployer struct {
	app    core.App
	root   string
	builds ArtifactBuilder
	evict  EvictFunc
	// verify boots the org through the manager (same hook as provisioning).
	// nil commits without a boot check — test wiring only; production always
	// passes one, since readiness IS the commit/revert decision.
	verify VerifyFunc
	log    *slog.Logger

	// minInterval is the per-org floor between accepted proposals.
	minInterval time.Duration

	// finishing tracks the detached finish() goroutines. finish outlives the
	// Deploy call that spawned it (the readiness handshake IS the async tail),
	// and its last acts are writes to app — so anything that tears the app
	// down must wait for it first, or those writes hit a closed DB and panic
	// inside PocketBase's save path. Wait() is that barrier.
	finishing sync.WaitGroup

	mu       sync.Mutex
	busy     map[string]bool
	last     map[string]time.Time
	inflight map[string]int // recipe hash → deploys between Build and settle

	// readMin is the per-org sustained-rate floor for accepted /v1/build,
	// /v1/resolve and /v1/versions calls — one refilled token per readMin,
	// bucketed with readBurst of headroom. These are read-ish (a build is a
	// cache hit or shares the builder's singleflight; resolve/versions shell
	// out to npm/git), so they are NOT serialized behind the deploy mutex —
	// doing so would break the warm-build-while-serving flow (a tenant builds
	// its next set while the org keeps serving). The sustained rate stops a
	// tenant hammering the router's inline toolchain into disk/fd/process
	// exhaustion; the burst exists because ONE legitimate UI interaction is
	// itself a same-second sequence (the Packages screen's mount fires a
	// /v1/versions per registry row, and clicking Install fires /v1/resolve
	// right behind them — a strict 1/s floor failed the install's resolve).
	// The builder's own MaxConcurrent semaphore caps genuine build concurrency.
	readMin    time.Duration
	readBurst  float64
	readTokens map[string]float64
	lastRead   map[string]time.Time
}

// Typed refusals the control-socket handler maps onto status codes.
var (
	errDeployBusy        = errors.New("a deploy is already in progress for this org")
	errDeployRateLimited = errors.New("deploys are rate-limited for this org; retry shortly")
	errReadRateLimited   = errors.New("too many requests for this org; retry shortly")
)

// maxLockfileEntries caps how many packages one lockfile may name. A 1 MiB body
// (the control-socket read limit) admits ~10^5 minimal entries; refsFor then
// runs a regexp per entry and the builder resolves each, so an unbounded set is
// a cheap way to burn router CPU. No real org set approaches this — the base
// shell plus every shipped feature is well under 256.
const maxLockfileEntries = 256

// liveDeployers tracks every Deployer built against a given app so a caller
// tearing that app down can drain the detached finish() goroutines first (see
// WaitForAppDeploys). Production wires one Deployer per app and never tears an
// app down mid-process, so this exists for the teardown ordering tests need.
var (
	liveDeployersMu sync.Mutex
	liveDeployers   = map[core.App][]*Deployer{}
)

// WaitForAppDeploys drains the in-flight deploy tails of every Deployer built
// against app. Call it before closing/resetting an app: finish() writes rows
// through app after the evict callback fires, so tearing the app down on the
// evict signal alone races those writes into a closed database.
func WaitForAppDeploys(app core.App) {
	liveDeployersMu.Lock()
	ds := append([]*Deployer(nil), liveDeployers[app]...)
	liveDeployersMu.Unlock()
	for _, d := range ds {
		d.WaitForDeploys()
	}
	liveDeployersMu.Lock()
	delete(liveDeployers, app)
	liveDeployersMu.Unlock()
}

func newDeployer(app core.App, root string, b ArtifactBuilder, evict EvictFunc, verify VerifyFunc, log *slog.Logger) *Deployer {
	if log == nil {
		log = slog.Default()
	}
	d := &Deployer{
		app:         app,
		root:        root,
		builds:      b,
		evict:       evict,
		verify:      verify,
		log:         log,
		minInterval: 30 * time.Second,
		readMin:     time.Second,
		readBurst:   10,
		busy:        map[string]bool{},
		last:        map[string]time.Time{},
		inflight:    map[string]int{},
		readTokens:  map[string]float64{},
		lastRead:    map[string]time.Time{},
	}
	liveDeployersMu.Lock()
	liveDeployers[app] = append(liveDeployers[app], d)
	liveDeployersMu.Unlock()
	return d
}

// beginRead throttles the read-ish control endpoints (/v1/build, /v1/resolve,
// /v1/versions) per-org with a token bucket: readBurst tokens of headroom,
// refilled at one per readMin. Separate from begin (the deploy slot) on
// purpose — these must NOT serialize behind the deploy mutex, or a tenant
// warming its next build while serving would deadlock against its own
// in-flight deploy.
func (d *Deployer) beginRead(slug string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	tokens := d.readBurst // first sight of an org starts with a full bucket
	if last, seen := d.lastRead[slug]; seen {
		tokens = d.readTokens[slug] + now.Sub(last).Seconds()/d.readMin.Seconds()
		if tokens > d.readBurst {
			tokens = d.readBurst
		}
	}
	d.lastRead[slug] = now
	if tokens < 1 {
		d.readTokens[slug] = tokens
		return errReadRateLimited
	}
	d.readTokens[slug] = tokens - 1
	return nil
}

// begin claims the org's deploy slot: one at a time, not too often.
func (d *Deployer) begin(slug string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.busy[slug] {
		return errDeployBusy
	}
	if since := time.Since(d.last[slug]); since < d.minInterval {
		return fmt.Errorf("%w (%s left)", errDeployRateLimited, (d.minInterval - since).Round(time.Second))
	}
	d.busy[slug] = true
	d.last[slug] = time.Now()
	return nil
}

func (d *Deployer) end(slug string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.busy, slug)
}

// trackHash marks a recipe as deploy-in-flight so the GC sweep counts it live
// during the window between Build returning (possibly a cache hit on an OLD
// artifact, whose BuiltAt grace has long expired) and the org row pointing at
// it.
func (d *Deployer) trackHash(hash string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.inflight[hash]++
}

func (d *Deployer) untrackHash(hash string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.inflight[hash] <= 1 {
		delete(d.inflight, hash)
	} else {
		d.inflight[hash]--
	}
}

// LiveRecipeHashes is the GC liveness policy (builder.Store.Sweep's callback
// input): every org's current build, every org's previous build (its revert
// target), and every deploy in flight.
func (d *Deployer) LiveRecipeHashes() (map[string]bool, error) {
	live := map[string]bool{}
	recs, err := d.app.FindAllRecords("orgs")
	if err != nil {
		return nil, err
	}
	for _, rec := range recs {
		for _, field := range []string{"recipe_hash", "prev_recipe_hash"} {
			if h := rec.GetString(field); h != "" {
				live[h] = true
			}
		}
	}
	d.mu.Lock()
	for h := range d.inflight {
		live[h] = true
	}
	d.mu.Unlock()
	return live, nil
}

// IsArtifactSet reports whether a lockfile names an artifact-built package
// set. The discriminator is the base member: a buildable set must include the
// app shell (the builder refuses a set without it), and a legacy store set
// never does — store lockfiles carry store package names like @tinycld/core.
func IsArtifactSet(lock map[string]string) bool {
	_, ok := lock[pkgbuild.BaseMemberSlug]
	return ok
}

// refsFor turns an org lockfile into builder refs. Name and version are
// validated separately BEFORE concatenation into an npm spec (the
// pkg_version_change lesson: constrain the tokens, not just the joined
// string), and sorted so equal sets share the builder's singleflight key.
func refsFor(lock map[string]string) ([]builder.PackageRef, error) {
	if len(lock) == 0 {
		return nil, fmt.Errorf("empty package set")
	}
	if len(lock) > maxLockfileEntries {
		return nil, fmt.Errorf("package set too large: %d entries (max %d)", len(lock), maxLockfileEntries)
	}
	names := make([]string, 0, len(lock))
	for name := range lock {
		names = append(names, name)
	}
	sort.Strings(names)
	refs := make([]builder.PackageRef, 0, len(names))
	for _, name := range names {
		version := lock[name]
		if !pkgbuild.NpmPackagePattern.MatchString(name) {
			return nil, fmt.Errorf("invalid package name %q", name)
		}
		if !pkgbuild.VersionTokenPattern.MatchString(version) {
			return nil, fmt.Errorf("invalid version %q for %s", version, name)
		}
		refs = append(refs, builder.PackageRef{Spec: name + "@" + version})
	}
	return refs, nil
}

// BuildSet builds (or cache-hits) the artifact for a proposed set. This is
// the tenant's warm-up call: novel sets pay minutes here while the org keeps
// serving its old build, so the later Deploy is a cache hit.
func (d *Deployer) BuildSet(ctx context.Context, lock map[string]string) (builder.Result, error) {
	refs, err := refsFor(lock)
	if err != nil {
		return builder.Result{}, err
	}
	return d.builds.Build(ctx, refs, slogSink{log: d.log})
}

// Deploy runs D6 steps 3–5 for an OPERATOR-driven deploy: authoritative
// compat check + build (both inside BuildSet), record the deployment, repoint
// the org row, then asynchronously evict → respawn → commit or revert. It
// returns as soon as the proposal is durably recorded — the caller gets an
// accept/refuse answer, never a half-applied deploy.
//
// An operator deploy has no proposing tenant and took no downs, so its revert
// path NEVER restores .deploy/backup.db: any snapshot present is
// tenant-planted, not this deploy's (M3).
func (d *Deployer) Deploy(ctx context.Context, slug string, next map[string]string, jobID string) (string, error) {
	return d.deploy(ctx, slug, next, jobID, false)
}

// DeployProposed is Deploy for a tenant-proposed set (ctl.sock /v1/deploy).
// The proposing tenant has already run its downs behind a VACUUM INTO
// snapshot (D6 step 2), so a failed deploy restores that snapshot — and only
// that one: the file is fingerprinted at proposal time, and a snapshot
// planted or swapped afterwards refuses to restore (M3).
func (d *Deployer) DeployProposed(ctx context.Context, slug string, next map[string]string, jobID string) (string, error) {
	return d.deploy(ctx, slug, next, jobID, true)
}

func (d *Deployer) deploy(ctx context.Context, slug string, next map[string]string, jobID string, proposed bool) (string, error) {
	if err := d.begin(slug); err != nil {
		return "", err
	}
	settled := false
	defer func() {
		if !settled {
			d.end(slug)
		}
	}()

	rec, err := d.app.FindFirstRecordByData("orgs", "slug", slug)
	if err != nil || rec == nil {
		return "", fmt.Errorf("org %q not found", slug)
	}
	if s := rec.GetString("status"); s != "active" {
		return "", fmt.Errorf("cannot deploy to org %q in status %q", slug, s)
	}

	lfBytes, err := lockfile.OrgLockfile(next).Marshal()
	if err != nil {
		return "", err
	}

	res, err := d.BuildSet(ctx, next)
	if err != nil {
		d.recordDeployment(rec.Id, lfBytes, "", jobID, "failed", err)
		return "", err
	}
	d.trackHash(res.RecipeHash)
	defer func() {
		if !settled {
			d.untrackHash(res.RecipeHash)
		}
	}()

	dep, err := d.recordDeployment(rec.Id, lfBytes, res.RecipeHash, jobID, "proposed", nil)
	if err != nil {
		return "", err
	}

	// The revert target, captured before the repoint. Held in memory for the
	// duration of finish(); a router crash mid-deploy leaves the org row on
	// the new build with the deployment row still "proposed" — the new boot
	// applies its ups and serves, and the stranded row is the operator's
	// audit breadcrumb.
	prev := orgBuildState{
		lockfile:   rec.GetString("lockfile"),
		recipeHash: rec.GetString("recipe_hash"),
		prevHash:   rec.GetString("prev_recipe_hash"),
	}
	rec.Set("lockfile", string(lfBytes))
	rec.Set("prev_recipe_hash", prev.recipeHash)
	rec.Set("recipe_hash", res.RecipeHash)
	if err := d.app.Save(rec); err != nil {
		return "", fmt.Errorf("repoint org row: %w", err)
	}

	// The snapshot a failed TENANT-PROPOSED deploy may restore is the one on
	// disk NOW, at proposal time — fingerprinted so the restore refuses any
	// file swapped in later. Operator deploys get the zero ref: restore-never.
	// (The proposing tenant stays alive until the eviction and could rewrite
	// its snapshot in place — but the snapshot is its own database, which it
	// can write directly anyway; the binding exists so no OTHER deploy's
	// revert ever adopts a tenant-planted file.)
	var snap snapshotRef
	if proposed {
		snap = captureSnapshot(filepath.Join(d.root, "pb_orgs", slug))
	}

	settled = true
	d.finishing.Add(1)
	go func() {
		defer d.finishing.Done()
		d.finish(slug, dep, prev, res.RecipeHash, jobID, snap)
	}()
	return res.RecipeHash, nil
}

// WaitForDeploys blocks until every in-flight finish() goroutine has returned.
// finish's tail writes deployment/org rows through app, so a caller that is
// about to tear the app down (tests) must drain first — otherwise those writes
// land on a closed database and panic inside PocketBase's save path.
func (d *Deployer) WaitForDeploys() { d.finishing.Wait() }

// orgBuildState is the org row subset finish() restores on a failed deploy.
type orgBuildState struct {
	lockfile   string
	recipeHash string
	prevHash   string
}

// finish is the async tail of a deploy: the readiness handshake of the
// respawned org is the commit/revert decision (D6 steps 4–5). snap is the
// snapshot fingerprint captured at proposal — the zero ref for operator
// deploys, which never restore.
func (d *Deployer) finish(slug string, dep *core.Record, prev orgBuildState, recipeHash, jobID string, snap snapshotRef) {
	defer d.end(slug)
	defer d.untrackHash(recipeHash)

	orgDir := filepath.Join(d.root, "pb_orgs", slug)
	d.evict(slug)

	var bootErr error
	if d.verify != nil {
		ctx, cancel := context.WithTimeout(context.Background(), tenantVerifyTimeout)
		defer cancel()
		bootErr = d.verify(ctx, slug)
	}

	if bootErr == nil {
		d.setDeploymentStatus(dep, "committed", nil)
		dropSnapshot(orgDir, d.log)
		d.writeDeployResult(orgDir, tenantcfg.DeployResult{
			JobID:       jobID,
			Status:      tenantcfg.DeployCommitted,
			RecipeHash:  recipeHash,
			CompletedAt: time.Now().UTC(),
		})
		d.log.Info("deploy committed", "slug", slug, "recipeHash", recipeHash)
		return
	}

	d.log.Error("deploy failed; reverting", "slug", slug, "recipeHash", recipeHash, "error", bootErr)

	// Every failure converges on the same restore path (the snapshot precedes
	// the downs — D6): repoint the previous build, restore the tenant's
	// snapshot if one exists, respawn, and record the outcome durably.
	if rec, err := d.app.FindFirstRecordByData("orgs", "slug", slug); err == nil && rec != nil {
		rec.Set("lockfile", prev.lockfile)
		rec.Set("recipe_hash", prev.recipeHash)
		rec.Set("prev_recipe_hash", prev.prevHash)
		if err := d.app.Save(rec); err != nil {
			d.log.Error("could not repoint org row back after failed deploy", "slug", slug, "error", err)
		}
	} else {
		d.log.Error("could not load org row for revert", "slug", slug, "error", err)
	}
	if err := restoreSnapshot(orgDir, snap); err != nil {
		d.log.Error("could not restore pre-deploy database snapshot", "slug", slug, "error", err)
	}
	d.setDeploymentStatus(dep, "reverted", bootErr)
	d.writeDeployResult(orgDir, tenantcfg.DeployResult{
		JobID:       jobID,
		Status:      tenantcfg.DeployReverted,
		Error:       bootErr.Error(),
		RecipeHash:  prev.recipeHash,
		CompletedAt: time.Now().UTC(),
	})

	// Respawn the previous build so the org is serving again, not waiting for
	// its next request's cold start. Best effort — the readiness handshake
	// logs its own failure.
	d.evict(slug)
	if d.verify != nil {
		ctx, cancel := context.WithTimeout(context.Background(), tenantVerifyTimeout)
		defer cancel()
		if err := d.verify(ctx, slug); err != nil {
			d.log.Error("respawn of previous build failed after revert", "slug", slug, "error", err)
		}
	}
}

func (d *Deployer) recordDeployment(orgID string, lfBytes []byte, recipeHash, jobID, status string, cause error) (*core.Record, error) {
	col, err := d.app.FindCollectionByNameOrId("deployments")
	if err != nil {
		return nil, fmt.Errorf("find deployments collection: %w", err)
	}
	rec := core.NewRecord(col)
	rec.Set("org", orgID)
	rec.Set("lockfile", string(lfBytes))
	rec.Set("recipe_hash", recipeHash)
	rec.Set("status", status)
	rec.Set("job_id", jobID)
	if cause != nil {
		rec.Set("error", cause.Error())
	}
	if err := d.app.Save(rec); err != nil {
		if status == "failed" {
			// Recording a failed build is best-effort; the caller already has
			// the real error to return.
			d.log.Error("could not record failed deployment", "error", err)
			return nil, nil
		}
		return nil, fmt.Errorf("record deployment: %w", err)
	}
	return rec, nil
}

func (d *Deployer) setDeploymentStatus(dep *core.Record, status string, cause error) {
	if dep == nil {
		return
	}
	dep.Set("status", status)
	if cause != nil {
		dep.Set("error", cause.Error())
	}
	if err := d.app.Save(dep); err != nil {
		d.log.Error("could not update deployment status", "status", status, "error", err)
	}
}

// writeDeployResult durably records the outcome where the (re)spawned tenant
// reads it (tenantcfg owns the shape — one ABI definition).
func (d *Deployer) writeDeployResult(orgDir string, res tenantcfg.DeployResult) {
	body, err := json.Marshal(res)
	if err != nil {
		d.log.Error("could not encode deploy result", "error", err)
		return
	}
	// Hardened beneath-write (tenantcfg owns it): a hostile tenant that owns
	// its .runtime tree cannot redirect this root-authored write via a symlink.
	if _, err := tenantcfg.WriteRuntimeFile(orgDir, filepath.Base(tenantcfg.DeployResultPath(orgDir)), body, 0o644); err != nil {
		d.log.Error("could not write deploy result", "error", err)
	}
}

// snapshotPath is where the tenant leaves its pre-deploy VACUUM INTO snapshot
// (D6 step 2) before proposing.
func snapshotPath(orgDir string) string {
	return filepath.Join(orgDir, ".deploy", "backup.db")
}

// snapshotRef fingerprints the pre-deploy snapshot as it existed at proposal
// time, so the restore path can refuse any file that is not that one (M3).
// The zero value means "no snapshot may be restored" — the operator-deploy
// case, and the proposed-with-no-snapshot case.
type snapshotRef struct {
	exists  bool
	ino     uint64
	size    int64
	modTime time.Time
}

// captureSnapshot fingerprints .deploy/backup.db via Lstat: a symlink or any
// other irregular file is treated as absent, so it can never be adopted.
func captureSnapshot(orgDir string) snapshotRef {
	fi, err := os.Lstat(snapshotPath(orgDir))
	if err != nil || !fi.Mode().IsRegular() {
		return snapshotRef{}
	}
	ref := snapshotRef{exists: true, size: fi.Size(), modTime: fi.ModTime()}
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		ref.ino = sys.Ino
	}
	return ref
}

// restoreSnapshot puts the tenant's pre-deploy snapshot back as the live
// database. The tenant process is dead by the time this runs, so the files
// are cold. The WAL sidecars must go with it — SQLite would replay
// post-snapshot frames over the restored file and silently undo the restore
// (the same lesson as World A's rollbackFn).
//
// Only the snapshot fingerprinted at proposal restores (M3): a zero snap —
// operator deploy, or nothing on disk when the tenant proposed — is a no-op
// even when a backup.db exists, and a file that no longer matches the
// fingerprint (planted after the proposal, or not a regular file) is refused.
func restoreSnapshot(orgDir string, snap snapshotRef) error {
	backup := snapshotPath(orgDir)
	if !snap.exists {
		return nil
	}
	fi, err := os.Lstat(backup)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("refusing snapshot restore: %s is not a regular file", backup)
	}
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || sys.Ino != snap.ino || fi.Size() != snap.size || !fi.ModTime().Equal(snap.modTime) {
		return fmt.Errorf("refusing snapshot restore: %s is not the file fingerprinted at proposal", backup)
	}
	dbPath := filepath.Join(orgDir, "pb_data", "data.db")
	if err := os.Rename(backup, dbPath); err != nil {
		return fmt.Errorf("restore snapshot: %w", err)
	}
	for _, sidecar := range []string{dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Remove(sidecar); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", sidecar, err)
		}
	}
	return nil
}

// dropSnapshot discards the pre-deploy snapshot after a committed deploy (D6
// step 4: "commit the org row, drop the snapshot").
func dropSnapshot(orgDir string, log *slog.Logger) {
	if err := os.Remove(snapshotPath(orgDir)); err != nil && !os.IsNotExist(err) {
		log.Warn("could not drop pre-deploy snapshot", "error", err)
	}
}

// slogSink adapts the router logger to pkgbuild's ProgressSink for build jobs
// it runs on behalf of deploys.
type slogSink struct {
	log *slog.Logger
}

func (s slogSink) Progress(step string, percent int, message string) {
	s.log.Info("build progress", "step", step, "percent", percent, "message", message)
}

func (s slogSink) Logf(format string, args ...any) {
	s.log.Debug("build log", "line", fmt.Sprintf(format, args...))
}

// jobIDPattern bounds the tenant-minted job id that rides through the deploy
// protocol into the deployments row and deploy-result.json.
var jobIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{0,64}$`)

// Handler serves the per-org control socket (design D1). The socket's 0700
// tenant-uid directory is the authentication; the handler only ever acts on
// the slug it was constructed for, so a compromised tenant can only propose
// changes to itself — precisely the authority being delegated.
//
// The surface is a versioned protocol (§6's router↔tenant ABI):
//
//	POST /v1/build  {"lockfile": {...}}
//	    Build or cache-hit the artifact and wait for it. The org keeps
//	    serving its old build; novel sets pay their minutes here, so the
//	    later deploy is a cache hit.
//	    → 200 {"recipeHash", "cached", "migrations"} — migrations is the
//	    artifact's pb_migrations basenames, the "new set" side of the
//	    tenant's down/up delta (D6: the delta between two resolved sets is
//	    the down/up list).
//
//	POST /v1/deploy {"lockfile": {...}, "jobId": "..."}
//	    Propose the set (D6 step 2's "sends {next lockfile} and waits to be
//	    killed"). → 202 {"recipeHash"} once the proposal is recorded and the
//	    org row repointed; the eviction follows asynchronously.
//
//	POST /v1/resolve {"spec": "..."}
//	    Trusted-side metadata resolution of one fetch spec (npm pack +
//	    tarball hash + rlimited manifest eval): slug, npm name, version,
//	    peerVersions, evaluated manifest, migration basenames. The hosted
//	    Packages UI's pre-flight — a confined tenant has no toolchain.
//	    → 200 builder.ResolvedSpec.
//
//	POST /v1/versions {"spec": "..."}
//	    Available-version discovery for a stored source spec (npm view /
//	    git tags, 60s-cached). → 200 {"source", "versions", "error"}.
//
//	GET /v1/state
//	    The org's current lockfile + recipe hash from the org row — the
//	    authoritative current set a proposal is computed against. The tenant
//	    cannot reconstruct this from its recipe alone: the base lockfile
//	    entry is the app shell's npm version, while the recipe records the
//	    base under @tinycld/core at CORE's version (the key peer ranges
//	    constrain). → 200 {"lockfile", "recipeHash"}.
//
// Resolution authority note: a tenant may resolve/install arbitrary specs by
// design — that is exactly the agency being delegated (D1) — so these calls
// need no authorization beyond the socket itself; spec shape is validated
// before any subprocess sees it.
func (d *Deployer) Handler(slug string) http.Handler {
	type proposal struct {
		Lockfile map[string]string `json:"lockfile"`
		JobID    string            `json:"jobId"`
	}
	decode := func(w http.ResponseWriter, r *http.Request) (proposal, bool) {
		var p proposal
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
			writeCtlJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
			return p, false
		}
		if len(p.Lockfile) == 0 {
			writeCtlJSON(w, http.StatusBadRequest, map[string]any{"error": "lockfile is required"})
			return p, false
		}
		if !jobIDPattern.MatchString(p.JobID) {
			writeCtlJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid jobId"})
			return p, false
		}
		return p, true
	}

	// decodeSpec bounds and shape-validates a single fetch spec before any
	// subprocess (npm/git) sees it — same stance as refsFor: constrain the
	// token, don't sanitize.
	//
	// REGISTRY SPECS ONLY. /v1/resolve and /v1/versions run their subprocess
	// (`npm pack`, `npm view`, `git ls-remote`) inline in the ROUTER process,
	// outside every confinement. `npm pack` on a git spec runs the package's
	// prepare/prepack scripts, and `git ls-remote` on a URL spec is an SSRF
	// with the router's credentials — both as root. A confined tenant may
	// legitimately propose only npm-published packages anyway (the org
	// lockfile is {npm name: version} with nowhere to carry git provenance —
	// the same gate coreserver/pkg_hosted enforces tenant-side), so refuse
	// anything that is not a registry spec here, before the subprocess.
	decodeSpec := func(w http.ResponseWriter, r *http.Request) (string, bool) {
		var body struct {
			Spec string `json:"spec"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
			writeCtlJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
			return "", false
		}
		if err := pkgbuild.ValidatePackageSpec(body.Spec); err != nil {
			writeCtlJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return "", false
		}
		if src, _ := pkgbuild.ClassifySpec(body.Spec); src != pkgbuild.SourceNpm {
			writeCtlJSON(w, http.StatusBadRequest, map[string]any{"error": "only npm-published packages may be resolved here; git and URL specs are not supported"})
			return "", false
		}
		return body.Spec, true
	}

	// throttle applies the per-org read-endpoint rate limit, answering 429 when
	// the caller is inside the floor. Returns false when the request should not
	// proceed.
	throttle := func(w http.ResponseWriter) bool {
		if err := d.beginRead(slug); err != nil {
			writeCtlJSON(w, http.StatusTooManyRequests, map[string]any{"error": err.Error()})
			return false
		}
		return true
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/build", func(w http.ResponseWriter, r *http.Request) {
		if !throttle(w) {
			return
		}
		p, ok := decode(w, r)
		if !ok {
			return
		}
		// A novel build is MINUTES of synchronous work, but the ctl-socket
		// server's slow-loris write deadline (controlsock.go, 30s) is armed
		// per-request — by the time the build finishes, the deadline has long
		// passed and the response write EOFs the tenant mid-install. Extend
		// this one request's deadline to the build budget AFTER the (bounded,
		// already-decoded) body is consumed; every other route keeps the tight
		// default. Mirrors the tenant's own 90-minute build client budget.
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(90 * time.Minute)); err != nil {
			d.log.Warn("could not extend /v1/build write deadline; a long build may EOF the proposer", "slug", slug, "error", err)
		}
		// Deliberately not r.Context(): the build is shared via singleflight,
		// and one proposer hanging up must not cancel it for the org's retry.
		// The builder's own job timeout bounds it.
		res, err := d.BuildSet(context.Background(), p.Lockfile)
		if err != nil {
			writeCtlJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		migrations, err := builder.ArtifactMigrationBasenames(res.Dir)
		if err != nil {
			// The artifact committed; failing to enumerate its migrations must
			// not report the build as failed, but the tenant CANNOT compute its
			// down delta without the list — surface that explicitly.
			writeCtlJSON(w, http.StatusInternalServerError, map[string]any{
				"error": fmt.Sprintf("artifact built (%s) but its migrations could not be listed: %v", res.RecipeHash, err),
			})
			return
		}
		writeCtlJSON(w, http.StatusOK, map[string]any{
			"recipeHash": res.RecipeHash,
			"cached":     res.Cached,
			"migrations": migrations,
		})
	})
	mux.HandleFunc("POST /v1/resolve", func(w http.ResponseWriter, r *http.Request) {
		if !throttle(w) {
			return
		}
		spec, ok := decodeSpec(w, r)
		if !ok {
			return
		}
		res, err := d.builds.ResolveSpec(spec)
		if err != nil {
			writeCtlJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeCtlJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("GET /v1/state", func(w http.ResponseWriter, r *http.Request) {
		rec, err := d.app.FindFirstRecordByData("orgs", "slug", slug)
		if err != nil || rec == nil {
			writeCtlJSON(w, http.StatusNotFound, map[string]any{"error": "org not found"})
			return
		}
		lock := lockfile.OrgLockfile{}
		if raw := rec.GetString("lockfile"); raw != "" {
			lock, err = lockfile.Parse([]byte(raw))
			if err != nil {
				writeCtlJSON(w, http.StatusInternalServerError, map[string]any{"error": "org lockfile unreadable: " + err.Error()})
				return
			}
		}
		writeCtlJSON(w, http.StatusOK, map[string]any{
			"lockfile":   lock,
			"recipeHash": rec.GetString("recipe_hash"),
		})
	})
	mux.HandleFunc("POST /v1/versions", func(w http.ResponseWriter, r *http.Request) {
		if !throttle(w) {
			return
		}
		spec, ok := decodeSpec(w, r)
		if !ok {
			return
		}
		source, versions, fetchErr := pkgbuild.VersionsForSpecVia(spec, d.builds.NpmRegistry())
		if versions == nil {
			versions = []string{}
		}
		writeCtlJSON(w, http.StatusOK, map[string]any{
			"source":   source,
			"versions": versions,
			"error":    fetchErr,
		})
	})
	mux.HandleFunc("POST /v1/deploy", func(w http.ResponseWriter, r *http.Request) {
		p, ok := decode(w, r)
		if !ok {
			return
		}
		recipeHash, err := d.DeployProposed(context.Background(), slug, p.Lockfile, p.JobID)
		switch {
		case errors.Is(err, errDeployBusy):
			writeCtlJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		case errors.Is(err, errDeployRateLimited):
			writeCtlJSON(w, http.StatusTooManyRequests, map[string]any{"error": err.Error()})
		case err != nil:
			writeCtlJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		default:
			writeCtlJSON(w, http.StatusAccepted, map[string]any{"recipeHash": recipeHash})
		}
	})
	return mux
}

func writeCtlJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
