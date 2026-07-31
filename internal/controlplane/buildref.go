package controlplane

import (
	"fmt"
	"os"
	"path/filepath"

	"tinycld.org/core/pkgbuild"
	"tinycld.org/multi-org/internal/builder"
	"tinycld.org/multi-org/internal/lockfile"
	"tinycld.org/multi-org/internal/orgmanager"
)

// BuildResolver adapts the builder's artifact store to the manager's
// ResolveBuild hook: recipe hash → committed artifact dir, its dual-mode
// binary, and the member list with Dir pointing at each staged manifest —
// which is what lets the existing capability hooks (CardDAVSources etc.) read
// an artifact member exactly like a store package. The base member is
// excluded: it ships no manifest and contributes no feature capability.
func BuildResolver(builds *builder.Store, binaryName string) func(recipeHash string) (orgmanager.BuildRef, error) {
	return func(recipeHash string) (orgmanager.BuildRef, error) {
		recipe, err := builds.ReadRecipe(recipeHash)
		if err != nil {
			return orgmanager.BuildRef{}, fmt.Errorf("read recipe %s: %w", recipeHash, err)
		}
		dir, err := builds.Dir(recipeHash)
		if err != nil {
			return orgmanager.BuildRef{}, err
		}
		binary := filepath.Join(dir, binaryName)
		if _, err := os.Stat(binary); err != nil {
			return orgmanager.BuildRef{}, fmt.Errorf("artifact %s has no %s binary: %w", recipeHash, binaryName, err)
		}
		var pkgs []lockfile.ResolvedPackage
		for _, m := range recipe.Members {
			if m.Slug == pkgbuild.BaseMemberSlug {
				continue
			}
			pkgs = append(pkgs, lockfile.ResolvedPackage{
				Name:    m.Name,
				Version: m.Version,
				Dir:     filepath.Join(dir, builder.MemberManifestsDir, m.Slug),
			})
		}
		return orgmanager.BuildRef{Dir: dir, Binary: binary, Packages: pkgs}, nil
	}
}
