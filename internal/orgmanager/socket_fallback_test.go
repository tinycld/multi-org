package orgmanager

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The fallback socket directory is only reached when MT_ROOT is deep enough to
// overrun sun_path, but when it is reached the router (running as root) chowns
// and chmods it — and os.MkdirAll succeeds on a path that already resolves to a
// directory, following symlinks the whole way. Its location is derived solely
// from MT_ROOT, so it is predictable to anyone who can read the unit file.
//
// A local unprivileged user who pre-creates that path as a symlink therefore
// gets whatever it points at chowned to a tenant uid. Refusing to adopt a
// directory we did not create is what closes it.

func TestSecureRuntimeDir_RejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ")
	}
	base := t.TempDir()

	// Stand in for the attacker's target (/etc in the real exploit).
	target := filepath.Join(base, "sensitive")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}

	planted := filepath.Join(base, "mt-deadbeef")
	if err := os.Symlink(target, planted); err != nil {
		t.Fatal(err)
	}

	if err := secureRuntimeDir(planted, 0o711); err == nil {
		t.Fatal("adopted a symlinked directory: the spawner would then chown " +
			"the symlink target to a tenant uid")
	}
}

func TestSecureRuntimeDir_RejectsForeignOwnedDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: every directory appears self-owned")
	}
	base := t.TempDir()
	planted := filepath.Join(base, "mt-deadbeef")
	if err := os.MkdirAll(planted, 0o777); err != nil {
		t.Fatal(err)
	}
	// MkdirAll's mode is masked by umask, so set the hostile mode explicitly —
	// otherwise this lands at 0755 and the test asserts nothing.
	if err := os.Chmod(planted, 0o777); err != nil {
		t.Fatal(err)
	}

	// A directory anyone can write to is not one we can trust to hold
	// rendezvous sockets, regardless of who owns it.
	if err := secureRuntimeDir(planted, 0o711); err == nil {
		t.Fatal("adopted a world-writable directory as the socket parent")
	}
}

func TestSecureRuntimeDir_CreatesAndTightens(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "mt-fresh")

	if err := secureRuntimeDir(dir, 0o711); err != nil {
		t.Fatalf("secureRuntimeDir on a fresh path: %v", err)
	}
	st, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&os.ModeSymlink != 0 {
		t.Fatal("created a symlink")
	}
	if perm := st.Mode().Perm(); perm != 0o711 {
		t.Fatalf("perm = %o, want 711", perm)
	}

	// Idempotent: it runs on every spawn.
	if err := secureRuntimeDir(dir, 0o711); err != nil {
		t.Fatalf("second call: %v", err)
	}
}

// A pre-existing directory with loose permissions must be tightened rather than
// accepted as-is — an operator upgrading from a build that created it 0755
// should not keep the weaker mode.
func TestSecureRuntimeDir_TightensLoosePermissions(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "mt-loose")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := secureRuntimeDir(dir, 0o711); err != nil {
		t.Fatalf("secureRuntimeDir: %v", err)
	}
	st, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o711 {
		t.Fatalf("perm = %o, want 711 (loose mode was not tightened)", perm)
	}
}
