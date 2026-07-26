//go:build linux

package orgmanager

import (
	"context"
	"fmt"
	"hash/fnv"
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
}

// tenantUID maps a slug into the configured uid window.
//
// Hashing rather than allocating sequentially keeps the mapping stable across
// host restarts with no persisted state, which matters because the tenant's
// pb_data files on disk are owned by that uid. Collisions are possible and
// benign for isolation between *most* pairs but not all — with a range well
// above the org count they are rare, and an operator wanting guaranteed
// distinctness should widen UIDRange.
func tenantUID(slug string, base, span int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(slug))
	return base + int(h.Sum32()%uint32(span))
}

// linuxSpawner runs each tenant confined: its own uid, its own mount and PID
// namespaces, and a cgroup. This is the security boundary the whole
// per-process model exists to establish.
type linuxSpawner struct {
	conf LinuxConfinement
}

// NewSpawner returns the Linux tenant spawner, reading confinement settings
// from the environment.
func NewSpawner(log *slog.Logger) Spawner {
	conf := LinuxConfinement{
		UIDBase:    envInt("MT_TENANT_UID_BASE", 0),
		UIDRange:   envInt("MT_TENANT_UID_RANGE", 0),
		CgroupRoot: os.Getenv("MT_CGROUP_ROOT"),
	}

	if os.Geteuid() != 0 {
		log.Warn("serve-multi is not running as root: tenant processes cannot be given " +
			"separate uids or mount namespaces. Tenants are NOT confined.")
	} else if conf.UIDBase == 0 || conf.UIDRange == 0 {
		log.Warn("MT_TENANT_UID_BASE/MT_TENANT_UID_RANGE unset: every tenant runs as the " +
			"host user and can read every other org's data. Tenants are NOT confined.")
	}

	return &linuxSpawner{conf: conf}
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
	// The child remounts its own namespace before serving: Go cannot run code
	// between fork and exec, so the confinement steps that must happen inside
	// the new mount namespace are done by serve-org itself at startup. See
	// cmd/serve-org's --confine-packages handling.
	req.ConfinePackages = s.conf.UIDBase > 0 && s.conf.UIDRange > 0

	cmd := buildCmd(ctx, req, log)

	attr := cmd.SysProcAttr // buildCmd already set Setpgid

	// CLONE_NEWNS gives the child its own mount table so the bind mounts below
	// are invisible to the host and to every other tenant. CLONE_NEWPID hides
	// other processes: the tenant cannot signal or inspect anything outside
	// itself, and killing its PID 1 reaps the whole tree.
	attr.Cloneflags |= syscall.CLONE_NEWNS | syscall.CLONE_NEWPID
	attr.Unshareflags |= syscall.CLONE_NEWNS

	if s.conf.UIDBase > 0 && s.conf.UIDRange > 0 {
		uid := tenantUID(req.Slug, s.conf.UIDBase, s.conf.UIDRange)
		attr.Credential = &syscall.Credential{
			Uid: uint32(uid),
			Gid: uint32(uid),
		}
		if err := chownTree(req.OrgDir, uid); err != nil {
			return nil, fmt.Errorf("chown org dir for %s: %w", req.Slug, err)
		}
		// The child creates the socket after dropping privileges, so the
		// directory holding it must be writable by the tenant uid.
		if err := os.Chown(filepath.Dir(req.SocketPath), uid, uid); err != nil {
			return nil, fmt.Errorf("chown socket dir for %s: %w", req.Slug, err)
		}
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start tenant %s: %w", req.Slug, err)
	}

	if s.conf.CgroupRoot != "" {
		if err := placeInCgroup(s.conf.CgroupRoot, req.Slug, cmd.Process.Pid); err != nil {
			log.Warn("could not place tenant in cgroup", "slug", req.Slug, "error", err)
		}
	}

	return &execProcess{cmd: cmd}, nil
}

// chownTree gives the tenant uid ownership of its own directory subtree.
func chownTree(root string, uid int) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, uid, uid)
	})
}

// placeInCgroup moves the child into its own cgroup v2 group. Limits
// themselves are an operator concern for now (out of scope), but creating the
// group means they can be applied without restarting the tenant.
func placeInCgroup(root, slug string, pid int) error {
	dir := filepath.Join(root, "tenant-"+slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644)
}
