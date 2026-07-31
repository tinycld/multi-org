package builder

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"tinycld.org/core/pkgbuild"
)

// Single-spec metadata resolution for the per-org control socket (§7 step 4).
// A hosted tenant runs no toolchain — no npm, no node — so the Packages UI's
// pre-flight facts (what is this spec, what version, what does it require,
// which migrations does it ship) come from the router's trusted side, through
// exactly the machinery resolve() already uses for whole-set builds: npm pack
// into a scratch dir, tarball hashed by THIS process, manifest facts via the
// injected rlimited evaluator. Nothing from the fetched bytes is executed
// here beyond that evaluator.

// ResolvedSpec is what the tenant learns about one fetch spec.
type ResolvedSpec struct {
	// Slug is the registry slug ("todo", or "core" for the base member).
	Slug string `json:"slug"`
	// Name is the npm package name — the org lockfile key for this member.
	Name string `json:"name"`
	// Version is the resolved semver (for the base: CORE's version).
	Version string `json:"version"`
	// Integrity is this process's sha256 of the fetched tarball.
	Integrity string `json:"integrity"`
	// PeerVersions are the manifest's enforced requirements (empty for base).
	PeerVersions map[string]string `json:"peerVersions,omitempty"`
	// Manifest is the evaluated manifest JSON (nil for the base member).
	Manifest json.RawMessage `json:"manifest,omitempty"`
	// Migrations are the migration file basenames this version ships — the
	// per-version contribution D6 needs so the tenant can compute the
	// down/up delta between two resolved sets (drop reports, uninstall downs).
	Migrations []string `json:"migrations"`
}

// ResolveSpec fetches ONE spec and returns its identity + contribution facts.
// The fetched tree is discarded — this is metadata resolution, not a build.
func (b *Builder) ResolveSpec(spec string) (ResolvedSpec, error) {
	if err := b.validateSpec(spec); err != nil {
		return ResolvedSpec{}, err
	}
	if err := os.MkdirAll(b.workDir(), 0o755); err != nil {
		return ResolvedSpec{}, err
	}
	workDir, err := os.MkdirTemp(b.workDir(), "resolve-*")
	if err != nil {
		return ResolvedSpec{}, err
	}
	defer os.RemoveAll(workDir)

	extracted, tarball, err := b.cfg.pack(spec, workDir)
	if err != nil {
		return ResolvedSpec{}, fmt.Errorf("resolve %s: %w", spec, err)
	}
	integrity, err := sha256OfFile(tarball)
	if err != nil {
		return ResolvedSpec{}, fmt.Errorf("resolve %s: hash tarball: %w", spec, err)
	}
	member, peers, manifestJSON, err := b.identifyMember(extracted)
	if err != nil {
		return ResolvedSpec{}, fmt.Errorf("resolve %s: %w", spec, err)
	}
	migrations, err := memberMigrationBasenames(extracted, manifestJSON)
	if err != nil {
		return ResolvedSpec{}, fmt.Errorf("resolve %s: %w", spec, err)
	}
	return ResolvedSpec{
		Slug:         pkgbuild.MemberSlugToRegistry(member.Slug),
		Name:         member.Name,
		Version:      member.Version,
		Integrity:    integrity,
		PeerVersions: peers,
		Manifest:     manifestJSON,
		Migrations:   migrations,
	}, nil
}

// memberMigrationBasenames lists the migration files a fetched member ships.
// The base member's migrations sit at the fixed nested path
// core/server/pb_migrations (it carries no manifest — the same special case
// as coreserver's targetMigrationsDir); a feature member declares its
// directory through manifest.migrations.directory. No migrations dir (or no
// declaration) is a valid empty contribution.
func memberMigrationBasenames(extractedDir string, manifestJSON json.RawMessage) ([]string, error) {
	var migDir string
	if manifestJSON == nil {
		migDir = filepath.Join("core", "server", "pb_migrations")
	} else {
		var m struct {
			Migrations struct {
				Directory string `json:"directory"`
			} `json:"migrations"`
		}
		if err := json.Unmarshal(manifestJSON, &m); err != nil {
			return nil, fmt.Errorf("parse manifest migrations block: %w", err)
		}
		migDir = m.Migrations.Directory
		if migDir == "" {
			return []string{}, nil
		}
		// The directory is manifest-author input used in path construction:
		// resolve it and refuse an escape from the extracted tree.
		joined := filepath.Join(extractedDir, migDir)
		rel, err := filepath.Rel(extractedDir, joined)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("manifest migrations directory %q escapes the package", migDir)
		}
		migDir = rel
	}
	entries, err := os.ReadDir(filepath.Join(extractedDir, migDir))
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	out := []string{}
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// ArtifactMigrationBasenames lists a committed artifact's pb_migrations
// basenames — the "new set" side of the tenant's down/up delta, returned on
// /v1/build so the proposing tenant never has to know the store layout.
func ArtifactMigrationBasenames(artifactDir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(artifactDir, "pb_migrations"))
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	out := []string{}
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}
