//go:build linux

package orgmanager

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// LinuxConfinement configures the OS-level tenant boundary. Zero values disable
// the corresponding mechanism, so a partially-configured host degrades to a
// weaker boundary rather than refusing to boot — but see NewLinuxSpawner, which
// warns loudly when that happens.
type LinuxConfinement struct {
	// UIDBase and UIDRange define the per-tenant uid/gid window. Each org is
	// mapped to a distinct uid in [UIDBase, UIDBase+UIDRange) so one tenant's
	// files are unreadable by another at the kernel level. 0 disables uid
	// separation (every tenant runs as the host user).
	UIDBase  int
	UIDRange int

	// CgroupRoot is the cgroup v2 directory the host owns; each tenant gets a
	// child cgroup under it. Empty disables cgroup placement.
	CgroupRoot string

	// Limits are the cgroup v2 limits written into each tenant's cgroup
	// before the child is placed in it. Zero fields write nothing; a
	// CgroupRoot with no limits at all draws a NewSpawner warning, since the
	// group then constrains nothing.
	Limits TenantLimits
}

// linuxSpawner runs each tenant confined: its own uid, its own mount and PID
// namespaces, and a cgroup. This is the security boundary the whole
// per-process model exists to establish.
type linuxSpawner struct {
	conf LinuxConfinement
	uids *uidAllocator
}

// tenantUID returns the org's host uid, adopting the ownership its org dir
// already carries so a restarted host does not reallocate underneath files
// chownTree wrote under the previous uid.
func (s *linuxSpawner) tenantUID(slug, orgDir string) (int, error) {
	if st, err := os.Stat(orgDir); err == nil {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok && sys.Uid != 0 {
			s.uids.adopt(slug, int(sys.Uid))
		}
	}
	return s.uids.uidFor(slug)
}

// NewSpawner returns the Linux tenant spawner, reading confinement settings
// from the environment.
func NewSpawner(log *slog.Logger) Spawner {
	conf := LinuxConfinement{
		UIDBase:    envInt("MT_TENANT_UID_BASE", 0),
		UIDRange:   envInt("MT_TENANT_UID_RANGE", 0),
		CgroupRoot: os.Getenv("MT_CGROUP_ROOT"),
		Limits:     TenantLimitsFromEnv(log),
	}

	if os.Geteuid() != 0 {
		log.Warn("serve-multi is not running as root: tenant processes cannot be given " +
			"separate uids or mount namespaces. Tenants are NOT confined.")
	} else if conf.UIDBase == 0 || conf.UIDRange == 0 {
		log.Warn("MT_TENANT_UID_BASE/MT_TENANT_UID_RANGE unset: every tenant runs as the " +
			"host user and can read every other org's data. Tenants are NOT confined.")
	}
	if msg := cgroupLimitsWarning(conf.CgroupRoot, conf.Limits); msg != "" {
		log.Warn(msg)
	}

	return &linuxSpawner{conf: conf, uids: newUIDAllocator(conf.UIDBase, conf.UIDRange)}
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func (s *linuxSpawner) Spawn(ctx context.Context, req SpawnRequest, log *slog.Logger) (Process, error) {
	// Every confinement mechanism below needs root: namespace creation wants
	// CAP_SYS_ADMIN in the parent user namespace, and the uid window maps
	// arbitrary host uids, which an unprivileged user namespace cannot.
	// NewSpawner already warned that a non-root host runs tenants UNCONFINED;
	// gating here is what makes that degraded mode actually work instead of
	// every spawn failing clone(2) with EPERM — every org 503ing.
	root := os.Geteuid() == 0

	// The child remounts its own namespace before serving: Go cannot run code
	// between fork and exec, so the confinement steps that must happen inside
	// the new mount namespace are done by serve-org itself at startup. See
	// cmd/serve-org's --confine-packages handling.
	req.ConfinePackages = root && s.conf.UIDBase > 0 && s.conf.UIDRange > 0

	cmd := buildCmd(req, log)

	attr := cmd.SysProcAttr // buildCmd already set Setpgid

	// CLONE_NEWNS gives the child its own mount table so the bind mounts below
	// are invisible to the host and to every other tenant. CLONE_NEWPID hides
	// other processes: the tenant cannot signal or inspect anything outside
	// itself, and killing its PID 1 reaps the whole tree.
	if root {
		attr.Cloneflags |= syscall.CLONE_NEWNS | syscall.CLONE_NEWPID
		attr.Unshareflags |= syscall.CLONE_NEWNS
	}

	if root && s.conf.UIDBase > 0 && s.conf.UIDRange > 0 {
		// A uid the allocator cannot hand out safely means the boundary cannot
		// be established, so the tenant must not start: booting it inside
		// another org's uid is the one outcome worse than being unavailable.
		uid, err := s.tenantUID(req.Slug, req.OrgDir)
		if err != nil {
			return nil, fmt.Errorf("assign tenant uid for %s: %w", req.Slug, err)
		}
		// The child has to remount the package store read-only inside its own
		// mount namespace (see cmd/serve-org --confine-packages), and mount(2)
		// needs CAP_SYS_ADMIN over that namespace — which a process that has
		// setuid'd to a plain host uid does not hold. A single-uid user
		// namespace squares that circle: inside it the child is uid 0 with
		// full capabilities over the namespaces it owns, while on the host it
		// is the tenant uid and the kernel enforces file separation against
		// every other tenant exactly as before.
		attr.Cloneflags |= syscall.CLONE_NEWUSER
		attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: uid, Size: 1}}
		attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: uid, Size: 1}}
		attr.GidMappingsEnableSetgroups = true
		attr.Credential = &syscall.Credential{
			Uid: 0, // userns uid 0 == host uid `uid`
			Gid: 0,
		}
		if err := chownTree(req.OrgDir, uid); err != nil {
			return nil, fmt.Errorf("chown org dir for %s: %w", req.Slug, err)
		}
		// The child creates the socket itself, so its per-org directory must
		// be writable by the tenant uid — but ONLY that directory. Its parent
		// holds every org's socket dir and must stay the host's: a tenant
		// owning the shared parent could unlink and rebind sibling sockets.
		// The parent is healed explicitly because pre-per-org layouts chowned
		// it to whichever tenant spawned last.
		sockDir := filepath.Dir(req.SocketPath)
		if err := os.Chown(sockDir, uid, uid); err != nil {
			return nil, fmt.Errorf("chown socket dir for %s: %w", req.Slug, err)
		}
		if err := os.Chmod(sockDir, 0o700); err != nil {
			return nil, fmt.Errorf("chmod socket dir for %s: %w", req.Slug, err)
		}
		parent := filepath.Dir(sockDir)
		if err := os.Chown(parent, 0, 0); err != nil {
			return nil, fmt.Errorf("chown socket parent dir for %s: %w", req.Slug, err)
		}
		if err := os.Chmod(parent, 0o711); err != nil {
			return nil, fmt.Errorf("chmod socket parent dir for %s: %w", req.Slug, err)
		}
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start tenant %s: %w", req.Slug, err)
	}

	if s.conf.CgroupRoot != "" {
		if err := placeInCgroup(s.conf.CgroupRoot, req.Slug, cmd.Process.Pid, s.conf.Limits); err != nil {
			log.Warn("could not place tenant in cgroup — it runs with NO resource limits",
				"slug", req.Slug, "error", err)
		}
	}

	return &execProcess{cmd: cmd}, nil
}

// chownTree gives the tenant uid exclusive ownership of its own directory
// subtree.
//
// Ownership alone does not isolate anything: a pb_data left at the default
// 0644 is readable by every other tenant uid on the host, which is precisely
// the cross-org `ATTACH DATABASE` read this boundary exists to close. So the
// modes are narrowed to owner-only as we go. Symlinks are skipped — the mode
// on a symlink is meaningless and chmod would follow it into the shared
// package store.
func chownTree(root string, uid int) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if err := os.Lchown(path, uid, uid); err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		mode := os.FileMode(0o600)
		if info.IsDir() {
			mode = 0o700
		}
		return os.Chmod(path, mode)
	})
}

// placeInCgroup moves the child into its own cgroup v2 group, writing the
// configured limits BEFORE the pid so the tenant never runs unlimited inside
// its group. A limit that cannot be written fails the whole placement — the
// caller warns and the tenant runs outside the cgroup entirely, which is the
// same enforcement (none) stated honestly, rather than a group that looks
// confining and is not.
func placeInCgroup(root, slug string, pid int, lim TenantLimits) error {
	// cgroup v2 only materializes a controller's interface files in a child
	// after the parent delegates it via cgroup.subtree_control. Best-effort:
	// on a host where the operator pre-enabled the controllers this is a
	// no-op or harmless error, and a genuine gap surfaces as the specific
	// limit write failing below. Legal because the root dir holds only
	// tenant-* children, never member processes.
	if lim.Any() {
		var ctrls []byte
		for _, c := range []struct{ limit, name string }{
			{lim.MemoryMax, "+memory"},
			{lim.PidsMax, "+pids"},
			{lim.CPUMax, "+cpu"},
		} {
			if c.limit != "" {
				if len(ctrls) > 0 {
					ctrls = append(ctrls, ' ')
				}
				ctrls = append(ctrls, c.name...)
			}
		}
		_ = os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), ctrls, 0o644)
	}

	dir := filepath.Join(root, "tenant-"+slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range []struct{ name, val string }{
		{"memory.max", lim.MemoryMax},
		{"pids.max", lim.PidsMax},
		{"cpu.max", lim.CPUMax},
	} {
		if f.val == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, f.name), []byte(f.val), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", f.name, err)
		}
	}
	return os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644)
}
