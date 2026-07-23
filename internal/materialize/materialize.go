// Package materialize assembles an org's flat pb_hooks and pb_public directories
// from resolved store packages using symlinks (disk dedup). The stock jsvm plugin
// and static server see ordinary directories.
package materialize

import (
	"io/fs"
	"os"
	"path/filepath"

	"tinycld.org/multitenant/internal/lockfile"
)

// Materialize (re)builds <orgDir>/pb_hooks and <orgDir>/pb_public from resolved
// packages. Existing hooks/public dirs are cleared first, making it idempotent.
func Materialize(orgDir string, resolved []lockfile.ResolvedPackage) error {
	hooksDir := filepath.Join(orgDir, "pb_hooks")
	publicDir := filepath.Join(orgDir, "pb_public")

	for _, d := range []string{hooksDir, publicDir} {
		if err := os.RemoveAll(d); err != nil {
			return err
		}
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

	for _, pkg := range resolved {
		if err := linkServerHooks(filepath.Join(pkg.Dir, "server"), hooksDir); err != nil {
			return err
		}
		if err := linkClientDist(filepath.Join(pkg.Dir, "client", "dist"), publicDir); err != nil {
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
