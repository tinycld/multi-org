// Package builder is the multi-org trusted builder (DESIGN-org-package-agency
// §7 step 2): it turns a requested package set into a shared, content-addressed
// build artifact a tenant can boot from, using the same pkgbuild pipeline the
// single-tenant app installs with.
//
// # Trust argument
//
// Artifacts are shared BY HASH across orgs (D3/D4), so an artifact's key must
// be honest even though the build itself executes package-author code (pnpm
// lifecycle scripts, Metro, member Go). The invariant this package maintains:
// every recipe-identity fact is established by THIS trusted process before the
// job runs —
//
//   - tarball integrities are hashed here, from bytes fetched here;
//   - member names/slugs/versions/peerVersions come from manifest evaluation
//     through the injected evaluator (serve-multi wires the rlimited
//     subprocess evaluator);
//   - the recipe hash and RecipeFile are computed and written here.
//
// The confined job only consumes pre-fetched trees and produces the runtime
// tree; nothing it reports can relabel the artifact. A malicious package can
// therefore only taint artifacts whose recipe includes it — and every org
// resolving that recipe chose to run that package in-process anyway.
//
// # Scaffold source
//
// The workspace-root scaffold extras (scripts/link-members.ts,
// tinycld.packages.ts, tests/, .npmrc, package-versions.json) come from
// Config.ScaffoldRoot, an operator-provisioned workspace root (bootstrap is
// their source of truth). This couples every build to one scaffold revision —
// in particular package-versions.json, which is a recipe-hash input — and is
// the pragmatic v1 stance; per-core-version scaffolds are a named follow-up,
// not something to drift into.
package builder

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"tinycld.org/core/pkgbuild"
	"tinycld.org/multi-org/internal/manifesteval"
)

// Config configures a Builder. Root is required; zero values elsewhere take
// the documented defaults.
type Config struct {
	// Root is the multi-org data root (MT_ROOT). Artifacts commit to
	// <Root>/builds; jobs stage under <Root>/builder (same filesystem, so the
	// commit rename is atomic).
	Root string

	// ScaffoldRoot is the workspace-root scaffold source (see package doc).
	ScaffoldRoot string

	// BinaryName is the server binary's name inside the artifact. Default
	// "tinycld".
	BinaryName string

	// PnpmStoreDir is the shared pnpm content-addressable store build
	// workspaces point at, so repeated builds hardlink instead of re-download.
	// Default <Root>/builder/pnpm-store.
	PnpmStoreDir string

	// MaxConcurrent caps simultaneously-running build jobs. Default 1: builds
	// are minutes of CPU-saturating work; the queue, not parallelism, is the
	// capacity seam (design §6 "builder capacity").
	MaxConcurrent int

	// Runner executes jobs. Default InProcessRunner{} (unconfined — correct
	// for dev hosts and tests; production Linux wires the confined runner).
	Runner Runner

	// EvalManifest evaluates manifest source to JSON. Default
	// manifesteval.EvalJSON; serve-multi wires its subprocess evaluator so a
	// hostile manifest dies rlimited instead of OOMing the router.
	EvalManifest func(src, name string) ([]byte, error)

	// Toolchain identifies the build toolchain for recipe hashing. Zero means
	// detect from PATH on first use.
	Toolchain pkgbuild.Toolchain

	// AllowDirSpecs additionally accepts absolute local-directory specs
	// (npm pack <dir>). Operator tooling and the e2e build from sibling
	// checkouts this way. The ROUTER must never set it: a tenant-proposed dir
	// spec would pack arbitrary host paths into an org-readable artifact.
	AllowDirSpecs bool

	// NpmRegistry overrides the registry `npm pack` fetches specs from
	// (e2e/operator seam: the hosted-deploy e2e serves sibling checkouts from
	// a local registry). It applies ONLY to member-spec fetches — the build
	// workspace's own pnpm install keeps its normal registry resolution, so a
	// test registry that carries just the members doesn't break the
	// third-party dependency graph. Empty = npm's default resolution.
	NpmRegistry string

	// pack fetches one spec (test seam; nil means `npm pack`).
	pack packFn
}

// Builder owns the artifact store and the build queue. Safe for concurrent
// use.
type Builder struct {
	cfg   Config
	store *Store
	group singleflight.Group

	sem chan struct{}

	tcOnce sync.Once
	tc     pkgbuild.Toolchain
	tcErr  error

	now func() time.Time
}

// New returns a Builder over cfg.Root. It creates no directories and detects
// no toolchain until the first Build — a router that never builds pays
// nothing.
func New(cfg Config) (*Builder, error) {
	if cfg.Root == "" {
		return nil, fmt.Errorf("builder: Config.Root is required")
	}
	if cfg.ScaffoldRoot == "" {
		return nil, fmt.Errorf("builder: Config.ScaffoldRoot is required")
	}
	if cfg.BinaryName == "" {
		cfg.BinaryName = "tinycld"
	}
	if cfg.PnpmStoreDir == "" {
		cfg.PnpmStoreDir = filepath.Join(cfg.Root, "builder", "pnpm-store")
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 1
	}
	if cfg.Runner == nil {
		cfg.Runner = InProcessRunner{}
	}
	if cfg.EvalManifest == nil {
		cfg.EvalManifest = manifesteval.EvalJSON
	}
	if cfg.pack == nil {
		cfg.pack = npmPackWith(cfg.NpmRegistry)
	}
	return &Builder{
		cfg:   cfg,
		store: NewStore(filepath.Join(cfg.Root, "builds")),
		sem:   make(chan struct{}, cfg.MaxConcurrent),
		now:   time.Now,
	}, nil
}

// Store exposes the artifact store (the manager resolves an org's artifact
// dir through it; GC sweeps it).
func (b *Builder) Store() *Store { return b.store }

// NpmRegistry reports the configured registry override ("" = npm's default).
// The control socket's /v1/versions discovers versions through the same
// registry member fetches resolve from, so a registry-fronted deployment
// (the hosted e2e, an air-gapped mirror) sees a consistent package universe.
func (b *Builder) NpmRegistry() string { return b.cfg.NpmRegistry }

// Result is a completed (or cache-satisfied) build.
type Result struct {
	RecipeHash string
	Dir        string
	Members    []pkgbuild.ResolvedMember
	Cached     bool
}

// Build resolves refs, returns the cached artifact when the recipe is already
// built, and otherwise runs one build job. Concurrent calls for the same
// requested set share a single resolve+build (singleflight on the sorted spec
// list); two different sets that resolve to the same recipe are reconciled by
// the store's idempotent Commit. Job concurrency is capped by
// Config.MaxConcurrent; excess jobs queue on the semaphore.
//
// sink receives assemble/pipeline progress; nil is allowed.
func (b *Builder) Build(ctx context.Context, refs []PackageRef, sink pkgbuild.ProgressSink) (Result, error) {
	if sink == nil {
		sink = pkgbuild.NopSink()
	}
	v, err, _ := b.group.Do(canonicalKey(refs), func() (any, error) {
		return b.buildOne(ctx, refs, sink)
	})
	if err != nil {
		return Result{}, err
	}
	return v.(Result), nil
}

// buildOne is the singleflight body: resolve → compat gate → cache check →
// (queued) job → commit.
func (b *Builder) buildOne(ctx context.Context, refs []PackageRef, sink pkgbuild.ProgressSink) (Result, error) {
	if err := os.MkdirAll(b.workDir(), 0o755); err != nil {
		return Result{}, err
	}
	sink.Progress("Resolving packages", 1, fmt.Sprintf("%d specs", len(refs)))
	res, err := b.resolve(refs)
	if err != nil {
		return Result{}, err
	}
	defer res.cleanup()

	if err := checkPeers(res); err != nil {
		return Result{}, err
	}

	result := Result{RecipeHash: res.RecipeHash, Members: res.Members}
	if ok, err := b.store.Has(res.RecipeHash); err != nil {
		return Result{}, err
	} else if ok {
		result.Dir, _ = b.store.Dir(res.RecipeHash)
		result.Cached = true
		sink.Logf("recipe %s already built — cache hit", res.RecipeHash)
		return result, nil
	}

	select {
	case b.sem <- struct{}{}:
		defer func() { <-b.sem }()
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}

	jobDir, err := os.MkdirTemp(b.workDir(), res.Manifest.BuildID+"-*")
	if err != nil {
		return Result{}, err
	}
	spec := JobSpec{
		BuildID:      res.Manifest.BuildID,
		BuildDir:     filepath.Join(jobDir, "build"),
		ArtifactDir:  filepath.Join(jobDir, "artifact"),
		ScaffoldRoot: b.cfg.ScaffoldRoot,
		PnpmStoreDir: b.cfg.PnpmStoreDir,
		BinaryName:   b.cfg.BinaryName,
		Manifest:     res.Manifest,
		MemberDirs:   res.Dirs,
		Integrities:  integritiesBySlug(res.Members),
	}
	if err := b.cfg.Runner.Run(ctx, spec, sink); err != nil {
		// Keep the failed job dir for post-mortem; the next successful build
		// of any recipe does not depend on it and an operator can remove it.
		return Result{}, fmt.Errorf("build %s (job dir kept at %s): %w", res.RecipeHash, jobDir, err)
	}

	if err := writeMemberManifests(spec.ArtifactDir, res.Manifests); err != nil {
		return Result{}, err
	}
	// Index the native OTA bundles the job staged, so the org's tenant can
	// serve them from /api/app/update (DESIGN §6 "Native OTA per org"). The
	// facts are derived HERE, parent-side, by reading and hashing the staged
	// bytes — never taken from the confined job's report — for the same reason
	// members and integrities are: a job runs package-author code, and the
	// bundle hash is the client's whole integrity guarantee. runtimeVersion
	// comes from the base member dir the parent itself fetched.
	runtimeVersion := runtimeVersionFromBase(res.Dirs[pkgbuild.BaseMemberSlug])
	bundles, err := scanNativeBundles(spec.ArtifactDir, spec.BuildID, runtimeVersion)
	if err != nil {
		return Result{}, fmt.Errorf("index native bundles for %s: %w", res.RecipeHash, err)
	}
	if len(bundles) > 0 {
		sink.Logf("native OTA bundles indexed: %d (runtime version %s)", len(bundles), runtimeVersion)
	}

	tc, _ := b.toolchain()
	if err := writeRecipeFile(spec.ArtifactDir, Recipe{
		RecipeHash:     res.RecipeHash,
		BuildID:        spec.BuildID,
		BuiltAt:        b.now().UTC(),
		Toolchain:      tc,
		Members:        res.Members,
		Overrides:      res.Overrides,
		RuntimeVersion: runtimeVersion,
		Bundles:        bundles,
	}); err != nil {
		return Result{}, err
	}
	dir, err := b.store.Commit(res.RecipeHash, spec.ArtifactDir)
	if err != nil {
		return Result{}, err
	}
	if err := removeJobDir(jobDir); err != nil {
		sink.Logf("could not remove finished job dir %s: %v", jobDir, err)
	}
	result.Dir = dir
	sink.Progress("Build complete", 100, res.RecipeHash)
	return result, nil
}

func (b *Builder) workDir() string {
	return filepath.Join(b.cfg.Root, "builder")
}

// removeJobDir deletes a finished job's work tree. The job runs with
// HOME=jobDir, so its Go module cache lands under it — and the go tool writes
// module dirs read-only, which makes a plain RemoveAll fail with EPERM on
// every module entry when the router doesn't run as root (macOS dev hosts),
// silently leaking a multi-GB tree per build. Unlinking needs write permission
// on the PARENT directory, so restoring owner-write on directories is
// sufficient; file modes are irrelevant to removal.
func removeJobDir(jobDir string) error {
	_ = filepath.WalkDir(jobDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if info, infoErr := d.Info(); infoErr == nil && info.Mode().Perm()&0o200 == 0 {
			_ = os.Chmod(path, info.Mode().Perm()|0o200)
		}
		return nil
	})
	return os.RemoveAll(jobDir)
}

// toolchain returns the recipe toolchain, detecting from PATH once when the
// config left it zero.
func (b *Builder) toolchain() (pkgbuild.Toolchain, error) {
	if b.cfg.Toolchain != (pkgbuild.Toolchain{}) {
		return b.cfg.Toolchain, nil
	}
	b.tcOnce.Do(func() {
		b.tc, b.tcErr = pkgbuild.DetectToolchain(nil)
	})
	if b.tcErr != nil {
		return pkgbuild.Toolchain{}, fmt.Errorf("detect toolchain: %w", b.tcErr)
	}
	return b.tc, nil
}

func integritiesBySlug(members []pkgbuild.ResolvedMember) map[string]string {
	out := make(map[string]string, len(members))
	for _, m := range members {
		out[m.Slug] = m.Integrity
	}
	return out
}

func writeRecipeFile(artifactDir string, r Recipe) error {
	b, err := json.MarshalIndent(r, "", "    ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(artifactDir, RecipeFile), append(b, '\n'), 0o644)
}

// writeMemberManifests stages manifests/<slug>/manifest.json for every feature
// member, parent-side (see MemberManifestsDir). Like RecipeFile these are
// identity facts the confined job must not author.
func writeMemberManifests(artifactDir string, manifests map[string]json.RawMessage) error {
	for slug, raw := range manifests {
		dir := filepath.Join(artifactDir, MemberManifestsDir, slug)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o644); err != nil {
			return err
		}
	}
	return nil
}
