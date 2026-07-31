package orgmanager

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"tinycld.org/multi-org/internal/lockfile"
)

// Helpers to fabricate committed build artifacts the way the trusted builder
// lays them out (design D4): buildsRoot/<hash>/ carrying the flattened
// pb_hooks/pb_migrations/pb_public trees, the org's tenant binary at
// <dir>/tinycld, and one manifests/<slug>/manifest.json per feature member.
// Real-binary tests place the app-shell dual-mode build inside the artifact
// because production resolves the binary from the artifact itself
// (BuildRef.Binary = <dir>/tinycld) and confinement mounts the builds root
// read-only around it.

// artifactMember is one feature member staged into an artifact's manifests/.
type artifactMember struct {
	Slug    string
	Name    string // defaults to "@tinycld/<slug>"
	Version string // defaults to "1.0.0"
}

// artifactSpec describes one fabricated artifact.
type artifactSpec struct {
	// Hash names the artifact directory under the builds root; fixtures also
	// use it as the org's recipe hash.
	Hash string
	// Binary is a real tenant executable to place into the artifact as
	// "tinycld" (hard-linked, falling back to a copy). Empty writes a stub —
	// enough for fake-spawner tests, which never exec it.
	Binary string
	// Files maps artifact-relative paths (e.g. "pb_hooks/main.pb.js") to
	// contents. The builder commits flattened runtime trees, so tests write
	// the final layout directly.
	Files map[string]string
	// Members are the feature members staged under manifests/.
	Members []artifactMember
}

// buildArtifact lays the artifact out on disk and returns the BuildRef a
// production ResolveBuild would hand the manager for it.
func buildArtifact(t *testing.T, buildsRoot string, spec artifactSpec) BuildRef {
	t.Helper()
	dir := filepath.Join(buildsRoot, spec.Hash)
	for _, name := range []string{"pb_hooks", "pb_public", "pb_migrations"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for rel, body := range spec.Files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	bin := filepath.Join(dir, "tinycld")
	if spec.Binary == "" {
		if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	} else if err := os.Link(spec.Binary, bin); err != nil {
		body, err := os.ReadFile(spec.Binary)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bin, body, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	var pkgs []lockfile.ResolvedPackage
	for _, m := range spec.Members {
		if m.Name == "" {
			m.Name = "@tinycld/" + m.Slug
		}
		if m.Version == "" {
			m.Version = "1.0.0"
		}
		mdir := filepath.Join(dir, "manifests", m.Slug)
		if err := os.MkdirAll(mdir, 0o755); err != nil {
			t.Fatal(err)
		}
		manifest := fmt.Sprintf(`{"slug":%q,"name":%q,"version":%q}`, m.Slug, m.Name, m.Version)
		if err := os.WriteFile(filepath.Join(mdir, "manifest.json"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
		pkgs = append(pkgs, lockfile.ResolvedPackage{Name: m.Name, Version: m.Version, Dir: mdir})
	}

	return BuildRef{Dir: dir, Binary: bin, Packages: pkgs}
}

// resolveBuilds is a ResolveBuild over a fixed recipe-hash table.
func resolveBuilds(refs map[string]BuildRef) func(string) (BuildRef, error) {
	return func(recipeHash string) (BuildRef, error) {
		ref, ok := refs[recipeHash]
		if !ok {
			return BuildRef{}, fmt.Errorf("unknown build %q", recipeHash)
		}
		return ref, nil
	}
}

// buildsRootFor is where these fixtures commit artifacts under a manager root.
func buildsRootFor(root string) string { return filepath.Join(root, "builds") }
