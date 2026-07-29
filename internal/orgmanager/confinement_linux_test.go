//go:build linux

package orgmanager

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"tinycld.org/multi-org/internal/store"
)

// These tests assert the OS-level boundary this whole architecture exists to
// establish. They require Linux (namespaces) and root (uid switching, mounts),
// so they cannot run on a developer's macOS box — they are the reason the
// project needs Linux CI.
//
// Run: sudo go test ./internal/orgmanager/ -run TestConfinement -v

func requireConfinementEnv(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("confinement tests require root (uid switching, mount namespaces)")
	}
}

// newTestLinuxSpawner builds the real spawner and records it so tests can ask
// which uid an org was actually allocated, rather than recomputing the mapping
// (which would re-encode the allocator's logic into the test that guards it).
func newTestLinuxSpawner(conf LinuxConfinement) *linuxSpawner {
	s := &linuxSpawner{conf: conf, uids: newUIDAllocator(conf.UIDBase, conf.UIDRange)}
	lastTestSpawner = s
	return s
}

var lastTestSpawner *linuxSpawner

// assignedUID reports the uid the spawner allocated for a slug that has already
// been spawned.
func assignedUID(t *testing.T, slug string) int {
	t.Helper()
	if lastTestSpawner == nil {
		t.Fatal("no spawner recorded; call newConfinedManager first")
	}
	lastTestSpawner.uids.mu.Lock()
	defer lastTestSpawner.uids.mu.Unlock()
	uid, ok := lastTestSpawner.uids.bySlug[slug]
	if !ok {
		t.Fatalf("no uid allocated for %q", slug)
	}
	return uid
}

// newConfinedManager wires the real Linux spawner with uid separation enabled.
func newConfinedManager(t *testing.T, orgs map[string]string) *OrgManager {
	t.Helper()
	requireConfinementEnv(t)
	bin := buildTenantBinary(t)
	root := t.TempDir()

	// t.TempDir() and its parent are 0700 and owned by root (these tests run
	// as root); the tenant uids must be able to traverse down to their own
	// org dirs beneath it. Traversal-only, matching a production root.
	for _, p := range []string{root, filepath.Dir(root)} {
		if err := os.Chmod(p, 0o711); err != nil {
			t.Fatal(err)
		}
	}

	s := store.New(root)
	lookup := map[string]OrgRecord{}
	for slug, hook := range orgs {
		pkg := "@tinycld/" + slug
		if err := s.Publish(pkg, "1.0.0", map[string][]byte{
			"server/main.pb.js": []byte(hook),
		}); err != nil {
			t.Fatal(err)
		}
		lookup[slug] = OrgRecord{
			Slug:     slug,
			Status:   "active",
			Lockfile: []byte(fmt.Sprintf(`{%q:"1.0.0"}`, pkg)),
		}
	}

	mgr := New(Config{
		Root:  root,
		Store: s,
		Spawner: newTestLinuxSpawner(LinuxConfinement{
			UIDBase:  60000,
			UIDRange: 500,
		}),
		TenantBinary: bin,
		Logger:       quietLogger(),
		HooksPool:    2,
		LookupOrg:    stubLookup(lookup),
	})
	t.Cleanup(mgr.Shutdown)
	return mgr
}

// The headline exploit: a sandboxed hook could ATTACH another org's SQLite file
// and read its secrets. Under per-process confinement that open must fail at
// the OS layer, not because a SQL statement was blocklisted.
func TestConfinement_CannotAttachAnotherOrgsDatabase(t *testing.T) {
	requireConfinementEnv(t)

	victimDB := ""
	mgr := newConfinedManager(t, map[string]string{
		"victim": `routerAdd('GET','/ping',(e)=>e.json(200,{ok:true}))`,
		"attacker": `routerAdd('GET','/attack',(e)=>{
			try {
				$app.db().newQuery("ATTACH DATABASE '" + e.request.url.query().get('path') + "' AS stolen").execute()
				e.json(200, {attached: true})
			} catch (err) {
				e.json(200, {attached: false, error: String(err)})
			}
		})`,
	})

	// Boot the victim so its database exists on disk.
	if _, err := mgr.Get(context.Background(), "victim"); err != nil {
		t.Fatalf("victim: %v", err)
	}
	victimDB = filepath.Join(mgr.cfg.Root, "pb_orgs", "victim", "pb_data", "data.db")
	if _, err := os.Stat(victimDB); err != nil {
		t.Fatalf("victim db not found: %v", err)
	}

	attacker, err := mgr.Get(context.Background(), "attacker")
	if err != nil {
		t.Fatalf("attacker: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/attack?path="+victimDB, nil)
	attacker.Mux().ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, `"attached":true`) {
		t.Fatalf("CROSS-ORG BREACH: attacker attached the victim's database: %s", body)
	}
	if !strings.Contains(body, `"attached":false`) {
		t.Fatalf("unexpected response from the attack probe: %s", body)
	}
}

// Each tenant must run as its own uid, so the kernel enforces file separation
// even for capabilities we have not thought of.
func TestConfinement_TenantsRunAsDistinctNonRootUIDs(t *testing.T) {
	requireConfinementEnv(t)

	mgr := newConfinedManager(t, map[string]string{
		"alpha": `routerAdd('GET','/ping',(e)=>e.json(200,{ok:true}))`,
	})

	inst, err := mgr.Get(context.Background(), "alpha")
	if err != nil {
		t.Fatal(err)
	}

	// The child runs in its own user namespace, where it is uid 0 so it can
	// mount its package store read-only. What matters is the uid the HOST
	// kernel checks file access against, which is what stat on /proc/<pid>
	// reports — /proc/<pid>/status's Uid: line is namespace-relative and would
	// read 0 even when confinement is working perfectly.
	st, err := os.Stat(fmt.Sprintf("/proc/%d", inst.proc.Pid()))
	if err != nil {
		t.Fatalf("stat child proc: %v", err)
	}
	hostUID := st.Sys().(*syscall.Stat_t).Uid
	if hostUID == 0 {
		t.Fatal("tenant is running as host root")
	}
	if want := uint32(assignedUID(t, "alpha")); hostUID != want {
		t.Fatalf("tenant host uid = %d, want %d", hostUID, want)
	}
}

// The package store is shared and immutable; a tenant must not be able to
// rewrite code another org will execute.
//
// The probe targets a real store file — the hook source another org would load
// — from inside the tenant's own mount namespace, and asserts BOTH layers that
// are supposed to stop it:
//
//   - ownership: the store is root's, the tenant is not;
//   - the read-only bind mount confinePackages establishes, which is the layer
//     that still holds against a process that has defeated the first.
//
// The second probe runs as root deliberately. As the tenant uid the write is
// refused on ownership alone, so that assertion stays green with the mount
// removed — which is exactly how the old version of this test certified a
// confinement it never exercised.
func TestConfinement_PackageStoreIsReadOnly(t *testing.T) {
	requireConfinementEnv(t)

	mgr := newConfinedManager(t, map[string]string{
		"writer": `routerAdd('GET','/ping',(e)=>e.json(200,{ok:true}))`,
	})

	inst, err := mgr.Get(context.Background(), "writer")
	if err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(mgr.cfg.Root, "packages", "@tinycld", "writer", "1.0.0", "server", "main.pb.js")
	original, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("package store layout changed; probe target missing: %v", err)
	}

	uid := assignedUID(t, "writer")
	script := fmt.Sprintf("echo pwned > %s", target)

	if err := runInMountNS(t, inst.proc.Pid(), uid, script); err == nil {
		t.Error("tenant uid rewrote a package-store file that other orgs execute")
	}
	if err := runInMountNS(t, inst.proc.Pid(), 0, script); err == nil {
		t.Error("the package store is writable inside the tenant's mount namespace: " +
			"the read-only bind mount is not in effect")
	}

	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(original) {
		t.Fatalf("package store file was modified: %s", body)
	}
}

// runInMountNS runs a shell command inside the given pid's mount namespace as
// the given uid — i.e. against exactly the filesystem view the tenant process
// sees. uid 0 probes the mount itself rather than file ownership.
func runInMountNS(t *testing.T, pid, uid int, script string) error {
	t.Helper()
	cmd := exec.Command("nsenter", "--mount=/proc/"+strconv.Itoa(pid)+"/ns/mnt",
		"--setuid", strconv.Itoa(uid), "--setgid", strconv.Itoa(uid),
		"/bin/sh", "-c", script)
	out, err := cmd.CombinedOutput()
	if err != nil && strings.Contains(string(out), "executable file not found") {
		t.Skipf("nsenter unavailable: %s", out)
	}
	return err
}

// Each org's socket must live in its own directory owned by that tenant's uid
// alone, with the parent run dir staying root-owned. A shared socket directory
// chowned to whichever tenant spawned last hands that tenant every other org's
// rendezvous point: it can unlink a sibling's socket and bind its own in the
// same place to intercept that org's traffic.
func TestConfinement_SocketDirIsPerOrg(t *testing.T) {
	requireConfinementEnv(t)

	mgr := newConfinedManager(t, map[string]string{
		"alpha": `routerAdd('GET','/ping',(e)=>e.json(200,{ok:true}))`,
		"beta":  `routerAdd('GET','/ping',(e)=>e.json(200,{ok:true}))`,
	})

	alpha, err := mgr.Get(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("alpha: %v", err)
	}
	beta, err := mgr.Get(context.Background(), "beta")
	if err != nil {
		t.Fatalf("beta: %v", err)
	}

	// Distinctness is the allocator's contract, not a property of these two
	// particular slugs — assert it directly.
	uidAlpha := assignedUID(t, "alpha")
	uidBeta := assignedUID(t, "beta")
	if uidAlpha == uidBeta {
		t.Fatalf("alpha and beta share uid %d; the tenant boundary is gone for that pair", uidAlpha)
	}

	alphaDir := filepath.Dir(alpha.sockPath)
	betaDir := filepath.Dir(beta.sockPath)
	if alphaDir == betaDir {
		t.Fatalf("both tenants share the socket directory %s", alphaDir)
	}

	// The directory holding all the per-org socket dirs must remain root's:
	// whoever owns it can rename or unlink any org's socket dir wholesale.
	parent := filepath.Dir(alphaDir)
	pst, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	if uid := pst.Sys().(*syscall.Stat_t).Uid; uid != 0 {
		t.Fatalf("socket parent dir %s is owned by uid %d, want root", parent, uid)
	}
	if perm := pst.Mode().Perm(); perm&0o022 != 0 {
		t.Fatalf("socket parent dir %s is group/other-writable: %o", parent, perm)
	}

	ast, err := os.Stat(alphaDir)
	if err != nil {
		t.Fatal(err)
	}
	if uid := ast.Sys().(*syscall.Stat_t).Uid; uid != uint32(uidAlpha) {
		t.Fatalf("alpha's socket dir %s is owned by uid %d, want %d", alphaDir, uid, uidAlpha)
	}
	if perm := ast.Mode().Perm(); perm != 0o700 {
		t.Fatalf("alpha's socket dir %s has mode %o, want 0700", alphaDir, perm)
	}

	// And the kernel must actually enforce it: as beta's uid, alpha's socket
	// can be neither seen nor removed.
	if err := runAsUID(uidBeta, "test -e "+alpha.sockPath); err == nil {
		t.Fatalf("tenant beta (uid %d) can stat alpha's socket %s", uidBeta, alpha.sockPath)
	}
	_ = runAsUID(uidBeta, "rm -f "+alpha.sockPath)
	if _, err := os.Stat(alpha.sockPath); err != nil {
		t.Fatalf("tenant beta (uid %d) unlinked alpha's socket: %v", uidBeta, err)
	}
}

// runAsUID runs a shell command under the given uid/gid, the way a hostile
// tenant process would execute.
func runAsUID(uid int, script string) error {
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(uid)},
	}
	return cmd.Run()
}

// The child must not be able to read the host's environment.
func TestConfinement_ChildEnvironmentHoldsNoHostSecrets(t *testing.T) {
	requireConfinementEnv(t)
	t.Setenv("MT_SUPERUSER_PASSWORD", "super-secret-value")
	t.Setenv("MT_TLS_KEY", "/etc/tls/private.key")

	mgr := newConfinedManager(t, map[string]string{
		"probe": `routerAdd('GET','/ping',(e)=>e.json(200,{ok:true}))`,
	})

	inst, err := mgr.Get(context.Background(), "probe")
	if err != nil {
		t.Fatal(err)
	}

	environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", inst.proc.Pid()))
	if err != nil {
		t.Fatalf("read child environ: %v", err)
	}
	for _, secret := range []string{"super-secret-value", "/etc/tls/private.key", "MT_SUPERUSER", "MT_TLS"} {
		if strings.Contains(string(environ), secret) {
			t.Fatalf("host secret %q leaked into the tenant environment", secret)
		}
	}
}

// TestConfinement_CgroupLimitsApplied proves configured tenant limits reach
// the kernel: placeInCgroup writes memory.max / pids.max / cpu.max into the
// tenant's cgroup BEFORE placing the pid, and the kernel accepts each value.
// Readback asserts the kernel's canonical form (64M reads back as bytes), so
// this cannot pass on bytes merely landing in a file the kernel rejected.
func TestConfinement_CgroupLimitsApplied(t *testing.T) {
	requireConfinementEnv(t)
	const cgfs = "/sys/fs/cgroup"
	if _, err := os.Stat(filepath.Join(cgfs, "cgroup.subtree_control")); err != nil {
		t.Skipf("cgroup v2 unified hierarchy not available: %v", err)
	}
	// Delegate the needed controllers to our test root. Best-effort: on the
	// CI runner they are enabled already, and a genuine gap fails the limit
	// writes below with a precise error.
	_ = os.WriteFile(filepath.Join(cgfs, "cgroup.subtree_control"), []byte("+memory +pids +cpu"), 0o644)

	root := filepath.Join(cgfs, fmt.Sprintf("mt-limits-test-%d", os.Getpid()))
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("create test cgroup root: %v", err)
	}
	t.Cleanup(func() {
		// LIFO: the sleeper's cleanup below runs first, so by now the group
		// is empty and rmdir succeeds.
		_ = os.Remove(filepath.Join(root, "tenant-acme"))
		_ = os.Remove(root)
	})

	sleeper := exec.Command("sleep", "60")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sleeper.Process.Kill()
		_ = sleeper.Wait()
	})

	lim := TenantLimits{MemoryMax: "64M", PidsMax: "64", CPUMax: "50000 100000"}
	if err := placeInCgroup(root, "acme", sleeper.Process.Pid, lim); err != nil {
		t.Fatalf("placeInCgroup: %v", err)
	}

	dir := filepath.Join(root, "tenant-acme")
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
	if got := read("cgroup.procs"); got != strconv.Itoa(sleeper.Process.Pid) {
		t.Errorf("cgroup.procs = %q, want %d — the pid must land in the LIMITED group", got, sleeper.Process.Pid)
	}
}
