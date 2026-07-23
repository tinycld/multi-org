// Package materialize assembles an org's flat pb_hooks and pb_public directories
// from resolved store packages using symlinks (disk dedup). The stock jsvm plugin
// and static server see ordinary directories.
package materialize

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"tinycld.org/multi-org/internal/lockfile"
)

// Materialize (re)builds <orgDir>/pb_hooks, pb_public, and pb_migrations from
// resolved packages. Existing dirs are cleared first, making it idempotent.
func Materialize(orgDir string, resolved []lockfile.ResolvedPackage) error {
	hooksDir := filepath.Join(orgDir, "pb_hooks")
	publicDir := filepath.Join(orgDir, "pb_public")
	// Source uses the hyphenated "pb-migrations" (matching tinycld packages);
	// the tenant app reads the underscore "pb_migrations".
	migrationsDir := filepath.Join(orgDir, "pb_migrations")

	for _, d := range []string{hooksDir, publicDir, migrationsDir} {
		if err := os.RemoveAll(d); err != nil {
			return err
		}
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

	// migrationOwners maps a materialized migration filename to the package that
	// contributed it, so a same-named migration from two packages is a hard error
	// (matching tinycld's single-tenant generator guarantee) rather than a silent
	// last-wins clobber.
	migrationOwners := map[string]string{}

	for _, pkg := range resolved {
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
