//go:build linux

package orgmanager

import (
	"os"
	"path/filepath"
	"testing"
)

// chownTree must prune subtrees the tenant uid already owns. Re-walking
// pb_data/storage — hundreds of thousands of files for a big org — on every
// cold start burns the whole spawnTimeout before cmd.Start is even reached,
// crash-looping an org that can never finish a boot.
//
// Runs as the current (non-root) uid, so everything below the root is already
// "the tenant's own": nothing but the root's owner-only gate may be touched.
// (First-spawn behavior — root-owned files get chowned and narrowed — is
// exercised end-to-end by the root-gated TestConfinement_* suite.)
func TestChownTree_PrunesTenantOwnedSubtrees(t *testing.T) {
	root := t.TempDir()
	storage := filepath.Join(root, "pb_data", "storage")
	if err := os.MkdirAll(storage, 0o755); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(storage, "blob.bin")
	if err := os.WriteFile(blob, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := chownTree(root, os.Getuid()); err != nil {
		t.Fatal(err)
	}

	// The root's owner-only mode is what keeps pruned subtrees unreadable to
	// sibling tenant uids; it must be re-enforced on every spawn.
	st, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("org dir mode = %o, want 0700 — the root gate must always be enforced", st.Mode().Perm())
	}

	// Tenant-owned entries below the root must be left alone: their modes
	// unchanged is the observable proof the walk pruned instead of descending.
	for path, want := range map[string]os.FileMode{
		filepath.Join(root, "pb_data"): 0o755,
		blob:                           0o644,
	} {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != want {
			t.Fatalf("%s mode = %o, want %o — chownTree descended into a tenant-owned subtree",
				path, st.Mode().Perm(), want)
		}
	}
}
