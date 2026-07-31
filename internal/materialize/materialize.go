// Package materialize points an org's live pb_hooks, pb_public, and
// pb_migrations names at a committed build artifact's trees. The stock jsvm
// plugin and static server see ordinary directories through the symlinks.
package materialize

import (
	"fmt"
	"os"
	"path/filepath"
)

// MaterializeBuild points <orgDir>/pb_hooks, pb_public, and pb_migrations at a
// committed build artifact's trees (DESIGN-org-package-agency D4: the org holds
// a pointer into builds/<recipe-hash>/). The swap is atomic — a concurrent
// reader sees the old tree or the artifact, never a half-state: Deploy
// repoints a RUNNING tenant before evicting it, and an evicted predecessor may
// still be draining while its successor loads. The artifact IS the tree,
// shared by hash across orgs, so the live names become symlinks straight into
// it. Absolute targets, deliberately: the artifact lives outside the org dir,
// and confinement must expose it read-only at this same absolute path.
func MaterializeBuild(orgDir, artifactDir string) error {
	if err := os.MkdirAll(orgDir, 0o755); err != nil {
		return err
	}
	for _, name := range []string{"pb_hooks", "pb_public", "pb_migrations"} {
		if err := pointAt(orgDir, name, filepath.Join(artifactDir, name)); err != nil {
			return fmt.Errorf("swap %s: %w", name, err)
		}
	}
	return nil
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

	// A live path that is still a REAL directory (a pre-artifact org layout)
	// cannot be renamed over. Clearing it here is a one-time upgrade cost, not
	// the per-deploy behavior.
	if st, err := os.Lstat(live); err == nil && st.Mode()&os.ModeSymlink == 0 {
		if err := os.RemoveAll(live); err != nil {
			return err
		}
	}
	return os.Rename(tmp, live)
}
