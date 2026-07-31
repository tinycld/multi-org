package builder

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"tinycld.org/core/pkgbuild"
)

// PackageRef is one requested workspace member, by npm fetch spec —
// "name@version" for registry packages, a git spec for third-party forks, or
// (tests and operator tooling only) a local directory. The slug, npm name,
// resolved version and peerVersions are DISCOVERED from the fetched bytes,
// never trusted from the caller.
type PackageRef struct {
	Spec string
}

// resolvedInput is everything resolve() establishes about a requested set,
// with every recipe-identity fact computed on the trusted side:
//
//   - integrities are sha256 sums THIS process took of the fetched tarballs;
//   - names/slugs/versions/peerVersions come from evaluating each package's
//     manifest via the injected evaluator (serve-multi wires the rlimited
//     subprocess evaluator, the same boundary publish-time eval uses);
//   - the recipe hash is computed here from those facts.
//
// The confined build job later consumes the pre-fetched trees but can no
// longer influence what key the artifact is stored under.
type resolvedInput struct {
	Manifest     pkgbuild.RebuildManifest
	Members      []pkgbuild.ResolvedMember
	PeerVersions map[string]map[string]string // by slug
	Dirs         map[string]string            // slug → extracted package dir
	Manifests    map[string]json.RawMessage   // slug → evaluated manifest JSON (base has none)
	Overrides    map[string]string
	RecipeHash   string

	// fetchRoot owns every extracted dir; removed when the build finishes.
	fetchRoot string
}

func (r *resolvedInput) cleanup() {
	if r != nil && r.fetchRoot != "" {
		_ = os.RemoveAll(r.fetchRoot)
	}
}

// packFn fetches one spec into workDir and returns the extracted package
// directory plus the tarball path. The default (npmPack) shells `npm pack`;
// tests inject a writer of fixture trees. The INTEGRITY is deliberately not
// part of this seam's contract — resolve hashes the tarball itself, so a pack
// implementation cannot vouch for its own bytes.
type packFn func(spec, workDir string) (extractedDir, tarball string, err error)

// npmPackWith returns the production packFn: `npm pack <spec>` in workDir,
// untarring the result there and returning the extracted package/ dir plus
// the tarball path. registry (Config.NpmRegistry) overrides the fetch
// registry for the spec when non-empty. For a registry spec this executes
// nothing of the package's own (npm downloads the published tarball); git and
// directory specs run the package's prepare/prepack scripts, which is why
// production resolution of those belongs inside the per-job confinement (see
// the confined runner) — the router only passes registry specs today.
func npmPackWith(registry string) packFn {
	return func(spec, workDir string) (string, string, error) {
		args := []string{"pack", spec}
		if registry != "" {
			args = append(args, "--registry", registry)
		}
		return npmPackArgs(spec, workDir, args)
	}
}

func npmPackArgs(spec, workDir string, args []string) (string, string, error) {
	if _, err := pkgbuild.RunCmd(workDir, "npm", args...); err != nil {
		return "", "", fmt.Errorf("npm pack %s: %w", spec, err)
	}
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return "", "", err
	}
	var tgz string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tgz") {
			tgz = filepath.Join(workDir, e.Name())
			break
		}
	}
	if tgz == "" {
		return "", "", fmt.Errorf("no .tgz after npm pack %s", spec)
	}
	if _, err := pkgbuild.RunCmd(workDir, "tar", "xzf", tgz); err != nil {
		return "", "", fmt.Errorf("untar %s: %w", tgz, err)
	}
	// npm pack always extracts into a subdirectory named "package".
	return filepath.Join(workDir, "package"), tgz, nil
}

// manifestFacts is the subset of an evaluated manifest resolve needs.
type manifestFacts struct {
	Name         string            `json:"name"`
	Slug         string            `json:"slug"`
	Version      string            `json:"version"`
	PeerVersions map[string]string `json:"peerVersions"`
}

// resolve fetches every requested spec, establishes each member's identity
// facts, and computes the recipe hash. It refuses (rather than degrades) on
// anything that would make the hash a lie: a missing base member, duplicate
// slugs, or a package that is not a tinycld workspace member at all.
func (b *Builder) resolve(refs []PackageRef) (*resolvedInput, error) {
	if len(refs) == 0 {
		return nil, fmt.Errorf("resolve: empty package set")
	}
	fetchRoot, err := os.MkdirTemp(b.workDir(), "fetch-*")
	if err != nil {
		return nil, err
	}
	res := &resolvedInput{
		PeerVersions: map[string]map[string]string{},
		Dirs:         map[string]string{},
		Manifests:    map[string]json.RawMessage{},
		fetchRoot:    fetchRoot,
	}
	ok := false
	defer func() {
		if !ok {
			res.cleanup()
		}
	}()

	for i, ref := range refs {
		if err := b.validateSpec(ref.Spec); err != nil {
			return nil, fmt.Errorf("resolve %s: %w", ref.Spec, err)
		}
		workDir := filepath.Join(fetchRoot, fmt.Sprintf("m%d", i))
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			return nil, err
		}
		extracted, tarball, err := b.cfg.pack(ref.Spec, workDir)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", ref.Spec, err)
		}
		integrity, err := sha256OfFile(tarball)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: hash tarball: %w", ref.Spec, err)
		}
		member, peers, manifestJSON, err := b.identifyMember(extracted)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", ref.Spec, err)
		}
		member.Integrity = integrity
		if _, dup := res.Dirs[member.Slug]; dup {
			return nil, fmt.Errorf("resolve: two specs resolve to member %q", member.Slug)
		}
		res.Dirs[member.Slug] = extracted
		if manifestJSON != nil {
			res.Manifests[member.Slug] = manifestJSON
		}
		res.Members = append(res.Members, member)
		if len(peers) > 0 {
			res.PeerVersions[member.Slug] = peers
		}
		res.Manifest.Members = append(res.Manifest.Members, pkgbuild.MemberSpec{
			Slug:    member.Slug,
			Version: member.Version,
			Spec:    ref.Spec,
		})
	}

	if _, hasBase := res.Dirs[pkgbuild.BaseMemberSlug]; !hasBase {
		return nil, fmt.Errorf("resolve: no base (%s) member in the set — every build needs the app shell", pkgbuild.BaseMemberSlug)
	}

	res.Overrides, err = pkgbuild.ReadOverrides(b.cfg.ScaffoldRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve: scaffold overrides: %w", err)
	}
	tc, err := b.toolchain()
	if err != nil {
		return nil, err
	}
	res.RecipeHash, err = pkgbuild.RecipeHash(res.Members, res.Overrides, tc)
	if err != nil {
		return nil, fmt.Errorf("resolve: %w", err)
	}
	res.Manifest.BuildID = buildIDFor(res.RecipeHash)
	ok = true
	return res, nil
}

// validateSpec applies pkgbuild's spec validation, with the one builder-local
// extension Config.AllowDirSpecs grants: an absolute path to an existing
// directory (operator/e2e builds from sibling checkouts).
func (b *Builder) validateSpec(spec string) error {
	if b.cfg.AllowDirSpecs && filepath.IsAbs(spec) {
		if info, err := os.Stat(spec); err == nil && info.IsDir() {
			return nil
		}
	}
	return pkgbuild.ValidatePackageSpec(spec)
}

// identifyMember establishes one fetched package's identity. A package with a
// manifest source is a feature member — its facts come from the evaluated
// manifest, mirroring pkgbuild's readBuildMembers — and the evaluated JSON is
// returned whole, so the artifact can carry it for the router's capability
// wiring (manifests/<slug>/manifest.json). A package without one is the base
// (app shell) iff it nests core/package.json named @tinycld/core; its version
// is CORE's, recorded under the name peer ranges constrain, and it has no
// manifest JSON.
func (b *Builder) identifyMember(dir string) (pkgbuild.ResolvedMember, map[string]string, json.RawMessage, error) {
	src, name := readManifestSource(dir)
	if src == nil {
		member, peers, err := identifyBase(dir)
		return member, peers, nil, err
	}
	data, err := b.cfg.EvalManifest(string(src), name)
	if err != nil {
		return pkgbuild.ResolvedMember{}, nil, nil, fmt.Errorf("evaluate %s: %w", name, err)
	}
	var facts manifestFacts
	if err := json.Unmarshal(data, &facts); err != nil {
		return pkgbuild.ResolvedMember{}, nil, nil, fmt.Errorf("parse evaluated %s: %w", name, err)
	}
	switch {
	case facts.Slug == "":
		return pkgbuild.ResolvedMember{}, nil, nil, fmt.Errorf("%s declares no slug", name)
	case !pkgbuild.SlugPattern.MatchString(facts.Slug):
		return pkgbuild.ResolvedMember{}, nil, nil, fmt.Errorf("%s declares invalid slug %q", name, facts.Slug)
	case facts.Slug == pkgbuild.BaseMemberSlug:
		return pkgbuild.ResolvedMember{}, nil, nil, fmt.Errorf("%s claims the reserved base slug %q", name, pkgbuild.BaseMemberSlug)
	case facts.Name == "" || facts.Version == "":
		return pkgbuild.ResolvedMember{}, nil, nil, fmt.Errorf("%s declares no name/version", name)
	}
	// Name is the member's NPM identity from its package.json — the manifest's
	// `name` is a display string ("Mail"). An earlier revision recorded the
	// display name here, silently breaking ResolvedMember.Name's stated
	// contract (the lockfile key, the tenant's version-discovery source);
	// pkgbuild.readBuildMembers makes the same read so the two hosts' recipe
	// facts cannot diverge for the same bytes.
	npmName := pkgbuild.PackageJSONName(filepath.Join(dir, "package.json"))
	if npmName == "" {
		return pkgbuild.ResolvedMember{}, nil, nil, fmt.Errorf("member %s has no readable package.json name — cannot establish its npm identity", facts.Slug)
	}
	return pkgbuild.ResolvedMember{
		Slug:    facts.Slug,
		Name:    npmName,
		Version: facts.Version,
	}, facts.PeerVersions, json.RawMessage(data), nil
}

func identifyBase(dir string) (pkgbuild.ResolvedMember, map[string]string, error) {
	corePkg := filepath.Join(dir, "core", "package.json")
	raw, err := os.ReadFile(corePkg)
	if err != nil {
		return pkgbuild.ResolvedMember{}, nil, fmt.Errorf("package has neither a manifest nor a nested core/package.json — not a tinycld workspace member")
	}
	var pkg struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return pkgbuild.ResolvedMember{}, nil, fmt.Errorf("parse %s: %w", corePkg, err)
	}
	if pkg.Name != pkgbuild.CorePackageKey || pkg.Version == "" {
		return pkgbuild.ResolvedMember{}, nil, fmt.Errorf("nested core package is %q@%q, want %s — not the app shell", pkg.Name, pkg.Version, pkgbuild.CorePackageKey)
	}
	return pkgbuild.ResolvedMember{
		Slug:    pkgbuild.BaseMemberSlug,
		Name:    pkgbuild.CorePackageKey,
		Version: pkg.Version,
	}, nil, nil
}

// readManifestSource returns the package's manifest source and filename,
// preferring manifest.ts then manifest.js (the controlplane publish order).
func readManifestSource(dir string) ([]byte, string) {
	for _, name := range []string{"manifest.ts", "manifest.js"} {
		if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			return b, name
		}
	}
	return nil, ""
}

// checkPeers runs the peerVersions solver over the resolved set — the
// authoritative pre-build gate (D6 step 3). The key space mirrors
// VerifyTargetPeerVersions exactly (registry slugs, plus CorePackageKey for
// the base) so this gate and the job's post-assemble on-disk re-check can
// never disagree about the same set.
func checkPeers(res *resolvedInput) error {
	resolved := make(map[string]string, len(res.Members)+1)
	peersBySlug := make(map[string]map[string]string, len(res.PeerVersions))
	for _, m := range res.Members {
		regSlug := pkgbuild.MemberSlugToRegistry(m.Slug)
		resolved[regSlug] = m.Version
		if peers := res.PeerVersions[m.Slug]; len(peers) > 0 {
			peersBySlug[regSlug] = peers
		}
	}
	if coreVer, ok := resolved[pkgbuild.BaseRegistrySlug]; ok {
		resolved[pkgbuild.CorePackageKey] = coreVer
	}
	return pkgbuild.ViolationsError(pkgbuild.SolveCompat(resolved, peersBySlug))
}

// buildIDFor derives the deterministic job id for a recipe — hash-prefixed so
// concurrent job dirs and progress logs are attributable to their recipe.
func buildIDFor(recipeHash string) string {
	hex := strings.TrimPrefix(recipeHash, "sha256:")
	if len(hex) > 12 {
		hex = hex[:12]
	}
	return "recipe-" + hex
}

// canonicalKey is the singleflight key for a requested set: the sorted spec
// list. Two callers asking for the same set share one resolve+build regardless
// of spec order.
func canonicalKey(refs []PackageRef) string {
	specs := make([]string, 0, len(refs))
	for _, r := range refs {
		specs = append(specs, r.Spec)
	}
	sort.Strings(specs)
	return strings.Join(specs, "\n")
}

func sha256OfFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}
