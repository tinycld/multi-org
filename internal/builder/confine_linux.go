//go:build linux

package builder

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// confineJobCmd applies the Linux job jail to cmd before it starts: fresh
// mount and PID namespaces, and a single-uid user namespace mapping container
// 0 to the dedicated builder uid — the same trick the tenant spawner uses, and
// for the same reason: inside the namespace the job holds the capabilities its
// own namespaces need, while on the host it is an unprivileged uid the kernel
// separates from every tenant.
//
// Degraded mode mirrors the tenant spawner: without root or a configured uid
// the job runs unconfined, loudly. Returns whether confinement was applied.
func confineJobCmd(cmd *exec.Cmd, jobDir string, spec JobSpec, conf JobConfinement, log *slog.Logger) (bool, error) {
	root := os.Geteuid() == 0
	if !root {
		log.Warn("serve-multi is not running as root: build jobs run UNCONFINED " +
			"(package build scripts execute as the host user)")
		return false, nil
	}
	if conf.UID == 0 {
		log.Warn("MT_BUILDER_UID unset: build jobs run as root in namespaces only — " +
			"package build scripts are NOT uid-separated from tenants")
	}

	attr := cmd.SysProcAttr
	attr.Cloneflags |= syscall.CLONE_NEWNS | syscall.CLONE_NEWPID
	attr.Unshareflags |= syscall.CLONE_NEWNS

	if conf.UID > 0 {
		attr.Cloneflags |= syscall.CLONE_NEWUSER
		attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: conf.UID, Size: 1}}
		attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: conf.UID, Size: 1}}
		attr.GidMappingsEnableSetgroups = true
		attr.Credential = &syscall.Credential{Uid: 0, Gid: 0}

		// The job dir is the child's writable world (workspace, artifact, HOME,
		// TMPDIR); everything in it so far is parent-created, so a plain chown
		// of the few existing entries suffices — the child creates the rest.
		if err := chownShallow(jobDir, conf.UID); err != nil {
			return false, fmt.Errorf("chown job dir: %w", err)
		}
		// The pre-fetched member trees and their parents were created 0700 by
		// the parent; the job uid only needs to READ them (package bytes are
		// public — the same bytes npm serves anyone).
		for _, dir := range spec.MemberDirs {
			if err := chmodTreeWorldReadable(dir); err != nil {
				return false, fmt.Errorf("open member tree %s: %w", dir, err)
			}
			for _, parent := range []string{filepath.Dir(dir), filepath.Dir(filepath.Dir(dir))} {
				if err := os.Chmod(parent, 0o755); err != nil {
					return false, fmt.Errorf("open fetch dir %s: %w", parent, err)
				}
			}
		}
		// The shared pnpm store persists across jobs (that is its point), so
		// it must belong to the job uid.
		if err := os.MkdirAll(spec.PnpmStoreDir, 0o755); err != nil {
			return false, err
		}
		if err := chownShallow(spec.PnpmStoreDir, conf.UID); err != nil {
			return false, fmt.Errorf("chown pnpm store: %w", err)
		}
	}
	return true, nil
}

// chownShallow chowns path and its immediate children — the parent-created
// skeleton of a job dir. Deep trees under it (the pnpm store's contents, a
// prior job's leftovers) are already owned by the job uid, which is the only
// writer under these roots.
func chownShallow(path string, uid int) error {
	if err := os.Chown(path, uid, uid); err != nil {
		return err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.Lchown(filepath.Join(path, e.Name()), uid, uid); err != nil {
			return err
		}
	}
	return nil
}

// placeJobInCgroup mirrors the tenant spawner's cgroup placement for one build
// job, writing limits BEFORE the pid so the job never runs unlimited inside
// its group. The group dir is per-job and removed by the kernel's rmdir once
// empty; a failed placement is the caller's warning — the job then runs
// outside any cgroup, stated honestly.
//
// A limit the kernel rejects does NOT abort placement. Bailing out on the
// first bad write is how one unparsable value (MT_BUILDER_CPU_MAX as a bare
// core count) used to strip the memory and pids caps too: the pid write never
// ran, so the job escaped its cgroup entirely. Partial confinement beats none,
// so the rejected limits are collected and reported while the pid still goes
// in. cgrouplimits should reject a bad value long before it reaches here; this
// is the backstop for the ones only the kernel can refuse.
func placeJobInCgroup(conf JobConfinement, buildID string, pid int) error {
	root := conf.CgroupRoot
	limits := []struct{ name, val, ctrl string }{
		{"memory.max", conf.Limits.MemoryMax, "+memory"},
		{"pids.max", conf.Limits.PidsMax, "+pids"},
		{"cpu.max", conf.Limits.CPUMax, "+cpu"},
	}
	var ctrls []byte
	for _, l := range limits {
		if l.val != "" {
			if len(ctrls) > 0 {
				ctrls = append(ctrls, ' ')
			}
			ctrls = append(ctrls, l.ctrl...)
		}
	}
	if len(ctrls) > 0 {
		_ = os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), ctrls, 0o644)
	}

	dir := filepath.Join(root, "builder-"+buildID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var rejected []string
	for _, l := range limits {
		if l.val == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, l.name), []byte(l.val), 0o644); err != nil {
			rejected = append(rejected, fmt.Sprintf("%s=%q (%v)", l.name, l.val, err))
		}
	}
	// The pid write is what actually confines the job, so it happens whatever
	// the limits did. Its failure is the only one that means "not confined".
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return fmt.Errorf("write cgroup.procs: %w", err)
	}
	if len(rejected) > 0 {
		return fmt.Errorf("job placed, but the kernel rejected %d limit(s), which are UNLIMITED: %s",
			len(rejected), strings.Join(rejected, "; "))
	}
	return nil
}
