// Package materialize assembles an org's flat pb_hooks and pb_public directories
// from resolved store packages using symlinks (disk dedup). The stock jsvm plugin
// and static server see ordinary directories.
package materialize

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"tinycld.org/multi-org/internal/lockfile"
)

// Materialize (re)builds <orgDir>/pb_hooks, pb_public, and pb_migrations from
// resolved packages. Idempotent.
//
// Each of the three names is a SYMLINK to an a/b generation directory beside
// it, and a rebuild constructs the inactive generation then renames a fresh
// symlink over the live name — an atomic swap. It must never clear the live
// tree in place: Deploy re-materializes a RUNNING tenant before evicting it,
// and manager.load re-materializes while an evicted predecessor may still be
// draining — a tenant serving static assets out of pb_public the whole time.
// A concurrent reader sees the old tree or the new one, never a half-built
// one. (Directories themselves cannot be atomically replaced by rename; a
// symlink can.)
func Materialize(orgDir string, resolved []lockfile.ResolvedPackage) error {
	liveDirs := []string{"pb_hooks", "pb_public", "pb_migrations"}

	if err := os.MkdirAll(orgDir, 0o755); err != nil {
		return err
	}

	hooksDir, err := stageDir(orgDir, "pb_hooks")
	if err != nil {
		return err
	}
	publicDir, err := stageDir(orgDir, "pb_public")
	if err != nil {
		return err
	}
	// Source uses the hyphenated "pb-migrations" (matching tinycld packages);
	// the tenant app reads the underscore "pb_migrations".
	migrationsDir, err := stageDir(orgDir, "pb_migrations")
	if err != nil {
		return err
	}
	built := map[string]string{
		"pb_hooks":      hooksDir,
		"pb_public":     publicDir,
		"pb_migrations": migrationsDir,
	}

	// migrationOwners maps a materialized migration filename to the package that
	// contributed it, so a same-named migration from two packages is a hard error
	// (matching tinycld's single-tenant generator guarantee) rather than a silent
	// last-wins clobber.
	migrationOwners := map[string]string{}

	for _, pkg := range resolved {
		// Hooks come from pb-hooks/ (the tinycld convention: *.pb.ts/*.pb.js).
		// A legacy server/ dir is also linked for packages that predate pb-hooks.
		if err := linkServerHooks(filepath.Join(pkg.Dir, "pb-hooks"), hooksDir); err != nil {
			return err
		}
		if err := linkServerHooks(filepath.Join(pkg.Dir, "server"), hooksDir); err != nil {
			return err
		}
		if err := linkClientDist(filepath.Join(pkg.Dir, "client", "dist"), publicDir); err != nil {
			return err
		}
		if err := linkMigrations(filepath.Join(pkg.Dir, "pb-migrations"), migrationsDir, pkg.Name, migrationOwners); err != nil {
			return err
		}
	}

	// The whole tree built cleanly — swap each name onto its new generation.
	for _, n := range liveDirs {
		if err := swapIn(orgDir, n, built[n]); err != nil {
			return fmt.Errorf("swap %s: %w", n, err)
		}
	}
	return nil
}

// MaterializeBuild points <orgDir>/pb_hooks, pb_public, and pb_migrations at a
// committed build artifact's trees (DESIGN-org-package-agency D4: the org holds
// a pointer into builds/<recipe-hash>/). Same atomic-swap contract as
// Materialize — a concurrent reader sees the old tree or the artifact, never a
// half-state — but no generation is staged: the artifact IS the tree, shared
// by hash across orgs, so the live names become symlinks straight into it.
// Absolute targets, deliberately: the artifact lives outside the org dir, and
// confinement must expose it read-only at this same absolute path (exactly the
// store-farm rule).
//
// Leftover store-era generation directories are pruned down to the newest one,
// which an evicted predecessor may still be draining from; it is reclaimed by
// the next call here.
func MaterializeBuild(orgDir, artifactDir string) error {
	if err := os.MkdirAll(orgDir, 0o755); err != nil {
		return err
	}
	for _, name := range []string{"pb_hooks", "pb_public", "pb_migrations"} {
		if err := pointAt(orgDir, name, filepath.Join(artifactDir, name)); err != nil {
			return fmt.Errorf("swap %s: %w", name, err)
		}
		if err := pruneGenerations(orgDir, name, maxGeneration(orgDir, name)); err != nil {
			return err
		}
	}
	return nil
}

// stageDir creates and returns a FRESH generation directory to build into
// (one higher than any on disk), leaving every existing generation — including
// the live one — untouched while a tenant reads it.
func stageDir(orgDir, name string) (string, error) {
	dir := filepath.Join(orgDir, fmt.Sprintf("%s.gen%d", name, maxGeneration(orgDir, name)+1))
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// maxGeneration returns the highest generation number present for name, or 0.
func maxGeneration(orgDir, name string) int {
	entries, err := os.ReadDir(orgDir)
	if err != nil {
		return 0
	}
	most := 0
	for _, e := range entries {
		if n, ok := parseGeneration(e.Name(), name); ok && n > most {
			most = n
		}
	}
	return most
}

func parseGeneration(entry, name string) (int, bool) {
	rest, ok := strings.CutPrefix(entry, name+".gen")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// swapIn atomically points the live name at the freshly built generation, then
// garbage-collects all but the two newest generations.
//
// The IMMEDIATELY PREVIOUS generation is deliberately kept: a reader that
// resolved the symlink just before the swap is still walking that tree, and
// deleting it re-opens the went-dark window one path-resolution step later.
// It is reclaimed one deploy from now, by which point any such lookup has long
// finished.
func swapIn(orgDir, name, builtDir string) error {
	// Relative target so the org dir can be moved or bind-mounted wholesale.
	if err := pointAt(orgDir, name, filepath.Base(builtDir)); err != nil {
		return err
	}
	return pruneGenerations(orgDir, name, maxGeneration(orgDir, name)-1)
}

// pointAt atomically repoints the live name at target: build the symlink
// beside it, then rename over.
func pointAt(orgDir, name, target string) error {
	live := filepath.Join(orgDir, name)

	tmp := live + ".swap"
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}

	// A live path that is still a REAL directory (the pre-symlink layout, or a
	// first materialize racing nothing) cannot be renamed over. Clearing it
	// here is a one-time upgrade cost, not the per-deploy behavior.
	if st, err := os.Lstat(live); err == nil && st.Mode()&os.ModeSymlink == 0 {
		if err := os.RemoveAll(live); err != nil {
			return err
		}
	}
	return os.Rename(tmp, live)
}

// pruneGenerations removes every generation directory for name older than
// keepFrom.
func pruneGenerations(orgDir, name string, keepFrom int) error {
	entries, err := os.ReadDir(orgDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if n, ok := parseGeneration(e.Name(), name); ok && n < keepFrom {
			if err := os.RemoveAll(filepath.Join(orgDir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// linkServerHooks symlinks each file from the package's server dir into the flat
// hooks dir. Later packages overwrite earlier same-named files (last wins),
// matching lockfile order.
func linkServerHooks(srcServerDir, hooksDir string) error {
	entries, err := os.ReadDir(srcServerDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // package has no server side
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		src := filepath.Join(srcServerDir, e.Name())
		dst := filepath.Join(hooksDir, e.Name())
		_ = os.Remove(dst)
		if err := os.Symlink(src, dst); err != nil {
			return err
		}
	}
	return nil
}

// linkMigrations symlinks each top-level file from the package's pb-migrations
// dir into the flat pb_migrations dir. Migration filenames are timestamp-prefixed
// and globally unique by convention, so a collision across packages is a real bug
// — reject it (owners tracks the first contributor of each filename).
func linkMigrations(srcMigrationsDir, migrationsDir, pkgName string, owners map[string]string) error {
	entries, err := os.ReadDir(srcMigrationsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // package has no migrations
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if prev, ok := owners[name]; ok {
			return fmt.Errorf("migration filename collision: %q contributed by both %q and %q", name, prev, pkgName)
		}
		owners[name] = pkgName
		src := filepath.Join(srcMigrationsDir, name)
		dst := filepath.Join(migrationsDir, name)
		_ = os.Remove(dst)
		if err := os.Symlink(src, dst); err != nil {
			return err
		}
	}
	return nil
}

// linkClientDist mirrors the package's client/dist tree into publicDir, creating
// directories and symlinking files (preserving nested structure).
func linkClientDist(srcDistDir, publicDir string) error {
	info, err := os.Stat(srcDistDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // package has no client side
		}
		return err
	}
	if !info.IsDir() {
		return nil
	}
	return filepath.WalkDir(srcDistDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDistDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		dst := filepath.Join(publicDir, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		_ = os.Remove(dst)
		return os.Symlink(path, dst)
	})
}
