//go:build linux

package builder

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// These tests assert the builder's per-job OS boundary — the same class of
// guarantee as orgmanager's confinement tests, for the process that executes
// package-author build code. Linux + root only.
//
// Run: sudo go test ./internal/builder/ -run TestConfinement -v

// testBuilderUID is a uid outside any plausible tenant window; the job's
// files must land host-owned by it.
const testBuilderUID = 89117

func requireConfinementEnv(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("confinement tests require root (uid switching, mount namespaces)")
	}
}

func TestConfinementBuilderJob_RunsAsBuilderUIDInFreshNamespaces(t *testing.T) {
	requireConfinementEnv(t)

	spec := helperSpec(t)
	sink := &recordingSink{}
	r := helperRunner(t, "uid")
	r.Confinement = JobConfinement{UID: testBuilderUID}
	if err := r.Run(context.Background(), spec, sink); err != nil {
		t.Fatal(err)
	}

	// Inside its namespaces the job is pid 1 (CLONE_NEWPID) and uid 0 (the
	// single-uid user namespace) — same shape as a confined tenant.
	found := false
	for _, l := range sink.logs {
		if l == "identity pid=1 uid=0" {
			found = true
		}
	}
	if !found {
		t.Fatalf("child identity logs = %v, want identity pid=1 uid=0", sink.logs)
	}

	// On the HOST, everything the job wrote belongs to the builder uid — the
	// mapping that separates build-executed code from every tenant's files.
	st, err := os.Stat(filepath.Join(spec.ArtifactDir, "marker"))
	if err != nil {
		t.Fatal(err)
	}
	if uid := st.Sys().(*syscall.Stat_t).Uid; uid != testBuilderUID {
		t.Fatalf("artifact owned by uid %d, want %d", uid, testBuilderUID)
	}
}

func TestConfinementBuilderJob_MemberTreesReadableByJobUID(t *testing.T) {
	requireConfinementEnv(t)

	spec := helperSpec(t)
	// A parent-created 0700 fetch tree, as resolve leaves it before the
	// confined runner opens it for the job uid.
	fetchRoot := filepath.Join(t.TempDir(), "fetch")
	memberDir := filepath.Join(fetchRoot, "m0", "package")
	if err := os.MkdirAll(memberDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memberDir, "package.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec.MemberDirs = map[string]string{"tinycld": memberDir}
	spec.PnpmStoreDir = filepath.Join(t.TempDir(), "pnpm-store")

	r := helperRunner(t, "uid")
	r.Confinement = JobConfinement{UID: testBuilderUID}
	if err := r.Run(context.Background(), spec, &recordingSink{}); err != nil {
		t.Fatal(err)
	}

	for path, wantMode := range map[string]os.FileMode{
		memberDir: 0o755,
		filepath.Join(memberDir, "package.json"): 0o644,
	} {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != wantMode {
			t.Errorf("%s mode = %v, want %v", path, st.Mode().Perm(), wantMode)
		}
	}
	st, err := os.Stat(spec.PnpmStoreDir)
	if err != nil {
		t.Fatal(err)
	}
	if uid := st.Sys().(*syscall.Stat_t).Uid; uid != testBuilderUID {
		t.Fatalf("pnpm store owned by uid %d, want %d", uid, testBuilderUID)
	}
}
