//go:build linux

package builder

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"tinycld.org/multi-org/internal/cgrouplimits"
)

// These tests must be named TestConfinement* — the CI job that runs the suite
// as root filters with `-run TestConfinement`, and anything outside that prefix
// silently skips (the unprivileged pass cannot write cgroups). A test that only
// ever skips is how the bug this file covers reached production.
func requireCgroupTestEnv(t *testing.T) string {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("cgroup placement tests require root")
	}
	const cgfs = "/sys/fs/cgroup"
	if _, err := os.Stat(filepath.Join(cgfs, "cgroup.subtree_control")); err != nil {
		t.Skipf("cgroup v2 unified hierarchy not available: %v", err)
	}
	_ = os.WriteFile(filepath.Join(cgfs, "cgroup.subtree_control"), []byte("+memory +pids +cpu"), 0o644)

	root := filepath.Join(cgfs, fmt.Sprintf("mt-builder-test-%d-%s", os.Getpid(), t.Name()))
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("create test cgroup root: %v", err)
	}
	return root
}

func startSleeper(t *testing.T) int {
	t.Helper()
	sleeper := exec.Command("sleep", "60")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sleeper.Process.Kill()
		_ = sleeper.Wait()
	})
	return sleeper.Process.Pid
}

// The build-job counterpart of orgmanager's TestConfinement_CgroupLimitsApplied:
// configured builder limits must actually reach the kernel. Readback asserts
// the kernel's canonical form, so this cannot pass on bytes merely landing in a
// file the kernel rejected.
func TestConfinementBuilderJob_CgroupLimitsApplied(t *testing.T) {
	root := requireCgroupTestEnv(t)
	const buildID = "recipe-test"
	t.Cleanup(func() {
		_ = os.Remove(filepath.Join(root, "builder-"+buildID))
		_ = os.Remove(root)
	})
	pid := startSleeper(t)

	conf := JobConfinement{
		CgroupRoot: root,
		Limits:     cgrouplimits.Limits{MemoryMax: "64M", PidsMax: "64", CPUMax: "50000 100000"},
	}
	if err := placeJobInCgroup(conf, buildID, pid); err != nil {
		t.Fatalf("placeJobInCgroup: %v", err)
	}

	dir := filepath.Join(root, "builder-"+buildID)
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return strings.TrimSpace(string(b))
	}
	if got := read("memory.max"); got != "67108864" {
		t.Errorf("memory.max = %q, want 67108864 (64M canonicalized by the kernel)", got)
	}
	if got := read("pids.max"); got != "64" {
		t.Errorf("pids.max = %q, want 64", got)
	}
	if got := read("cpu.max"); got != "50000 100000" {
		t.Errorf("cpu.max = %q, want \"50000 100000\"", got)
	}
	if got := read("cgroup.procs"); got != strconv.Itoa(pid) {
		t.Errorf("cgroup.procs = %q, want %d — the pid must land in the LIMITED group", got, pid)
	}
}

// The exact shape of the production failure: a cpu.max payload the kernel
// rejects must not cost the job its memory and pids caps. Placement reports the
// rejected limit but still puts the pid in the group, so the build stays
// confined by whatever the kernel did accept.
func TestConfinementBuilderJob_RejectedLimitStillPlacesPid(t *testing.T) {
	root := requireCgroupTestEnv(t)
	const buildID = "recipe-badcpu"
	t.Cleanup(func() {
		_ = os.Remove(filepath.Join(root, "builder-"+buildID))
		_ = os.Remove(root)
	})
	pid := startSleeper(t)

	// "3" is a bare core count — what MT_BUILDER_CPU_MAX used to reach the
	// kernel as. cpu.max wants "<quota> <period>", so the kernel returns EINVAL.
	conf := JobConfinement{
		CgroupRoot: root,
		Limits:     cgrouplimits.Limits{MemoryMax: "64M", PidsMax: "64", CPUMax: "3"},
	}
	err := placeJobInCgroup(conf, buildID, pid)
	if err == nil {
		t.Fatal("a kernel-rejected limit must be reported, not swallowed")
	}
	if !strings.Contains(err.Error(), "cpu.max") {
		t.Errorf("error must name the rejected limit, got: %v", err)
	}

	dir := filepath.Join(root, "builder-"+buildID)
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return strings.TrimSpace(string(b))
	}
	if got := read("cgroup.procs"); got != strconv.Itoa(pid) {
		t.Fatalf("cgroup.procs = %q, want %d — one bad limit must NOT let the job escape its cgroup",
			got, pid)
	}
	if got := read("memory.max"); got != "67108864" {
		t.Errorf("memory.max = %q — the accepted limits must survive a rejected sibling", got)
	}
	if got := read("pids.max"); got != "64" {
		t.Errorf("pids.max = %q — the accepted limits must survive a rejected sibling", got)
	}
}
