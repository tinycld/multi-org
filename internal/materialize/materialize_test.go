package materialize

import (
	"os"
	"path/filepath"
	"testing"
)

func writeArtifact(t *testing.T, index string) string {
	t.Helper()
	artifact := t.TempDir()
	for _, name := range []string{"pb_hooks", "pb_public", "pb_migrations"} {
		if err := os.MkdirAll(filepath.Join(artifact, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(artifact, "pb_public", "index.html"), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	return artifact
}

// An org's live names point straight into the committed build tree, and the
// transition from a pre-artifact layout (real directories, as CreateOrg-era
// roots carried) is a one-time replacement.
func TestMaterializeBuild_PointsLiveNamesAtArtifact(t *testing.T) {
	artifact := writeArtifact(t, "<html>built</html>")

	// Start from real directories, as an org dir predating its first
	// materialization would carry.
	orgDir := filepath.Join(t.TempDir(), "acme")
	for _, name := range []string{"pb_data", "pb_hooks", "pb_public", "pb_migrations"} {
		if err := os.MkdirAll(filepath.Join(orgDir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(orgDir, "pb_public", "stale.html"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeBuild(orgDir, artifact); err != nil {
		t.Fatal(err)
	}

	idx, err := os.ReadFile(filepath.Join(orgDir, "pb_public", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if string(idx) != "<html>built</html>" {
		t.Fatalf("live pb_public reads %q, want the artifact's tree", idx)
	}
	target, err := os.Readlink(filepath.Join(orgDir, "pb_hooks"))
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Join(artifact, "pb_hooks") {
		t.Fatalf("pb_hooks -> %q, want absolute artifact path", target)
	}
}

// A deploy repoints a RUNNING tenant's directories before evicting it, so the
// swap from one artifact to another must be atomic: a concurrent reader sees
// the old tree or the new one, never a missing live name.
func TestMaterializeBuild_RepointIsIdempotentAndAtomic(t *testing.T) {
	oldArtifact := writeArtifact(t, "<html>old</html>")
	newArtifact := writeArtifact(t, "<html>new</html>")

	orgDir := filepath.Join(t.TempDir(), "acme")
	if err := MaterializeBuild(orgDir, oldArtifact); err != nil {
		t.Fatal(err)
	}
	// Re-materializing the same artifact is a no-op repoint.
	if err := MaterializeBuild(orgDir, oldArtifact); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeBuild(orgDir, newArtifact); err != nil {
		t.Fatal(err)
	}

	idx, err := os.ReadFile(filepath.Join(orgDir, "pb_public", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if string(idx) != "<html>new</html>" {
		t.Fatalf("live pb_public reads %q after repoint, want the new artifact", idx)
	}
	// The swap staging name must not linger.
	if _, err := os.Lstat(filepath.Join(orgDir, "pb_public.swap")); !os.IsNotExist(err) {
		t.Fatalf("pb_public.swap left behind (err=%v)", err)
	}
}
