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

	mu       sync.Mutex
	busy     map[string]bool
	last     map[string]time.Time
	inflight map[string]int // recipe hash → deploys between Build and settle
}

// Typed refusals the control-socket handler maps onto status codes.
var (
	errDeployBusy        = errors.New("a deploy is already in progress for this org")
	errDeployRateLimited = errors.New("deploys are rate-limited for this org; retry shortly")
)

func newDeployer(app core.App, root string, b ArtifactBuilder, evict EvictFunc, verify VerifyFunc, log *slog.Logger) *Deployer {
	if log == nil {
		log = slog.Default()
	}
	return &Deployer{
		app:         app,
		root:        root,
		builds:      b,
		evict:       evict,
		verify:      verify,
		log:         log,
		minInterval: 30 * time.Second,
		busy:        map[string]bool{},
		last:        map[string]time.Time{},
		inflight:    map[string]int{},
	}
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

// Deploy runs D6 steps 3–5 for one org: authoritative compat check + build
// (both inside BuildSet), record the proposed deployment, repoint the org
// row, then asynchronously evict → respawn → commit or revert. It returns as
// soon as the proposal is durably recorded — the caller (a tenant that
// already ran its downs and is waiting to be killed, or the operator API)
// gets an accept/refuse answer, never a half-applied deploy.
func (d *Deployer) Deploy(ctx context.Context, slug string, next map[string]string, jobID string) (string, error) {
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

	settled = true
	go d.finish(slug, dep, prev, res.RecipeHash, jobID)
	return res.RecipeHash, nil
}

// orgBuildState is the org row subset finish() restores on a failed deploy.
type orgBuildState struct {
	lockfile   string
	recipeHash string
	prevHash   string
}

// finish is the async tail of a deploy: the readiness handshake of the
// respawned org is the commit/revert decision (D6 steps 4–5).
func (d *Deployer) finish(slug string, dep *core.Record, prev orgBuildState, recipeHash, jobID string) {
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
	if err := restoreSnapshot(orgDir); err != nil {
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
	if err := os.MkdirAll(filepath.Join(orgDir, ".runtime"), 0o755); err != nil {
		d.log.Error("could not create .runtime for deploy result", "error", err)
		return
	}
	body, err := json.Marshal(res)
	if err != nil {
		d.log.Error("could not encode deploy result", "error", err)
		return
	}
	if err := os.WriteFile(tenantcfg.DeployResultPath(orgDir), body, 0o644); err != nil {
		d.log.Error("could not write deploy result", "error", err)
	}
}

// snapshotPath is where the tenant leaves its pre-deploy VACUUM INTO snapshot
// (D6 step 2) before proposing.
func snapshotPath(orgDir string) string {
	return filepath.Join(orgDir, ".deploy", "backup.db")
}

// restoreSnapshot puts the tenant's pre-deploy snapshot back as the live
// database. The tenant process is dead by the time this runs, so the files
// are cold. The WAL sidecars must go with it — SQLite would replay
// post-snapshot frames over the restored file and silently undo the restore
// (the same lesson as World A's rollbackFn). No snapshot is a no-op: an
// operator-driven deploy has no proposing tenant and took no downs, so the
// database was never mutated ahead of the swap.
func restoreSnapshot(orgDir string) error {
	backup := snapshotPath(orgDir)
	if _, err := os.Stat(backup); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
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
		return body.Spec, true
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/build", func(w http.ResponseWriter, r *http.Request) {
		p, ok := decode(w, r)
		if !ok {
			return
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
		spec, ok := decodeSpec(w, r)
		if !ok {
			return
		}
		source, versions, fetchErr := pkgbuild.VersionsForSpec(spec)
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
		recipeHash, err := d.Deploy(context.Background(), slug, p.Lockfile, p.JobID)
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
