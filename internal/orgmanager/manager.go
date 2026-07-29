package orgmanager

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/singleflight"

	"tinycld.org/core/caldav"
	"tinycld.org/core/carddav"
	"tinycld.org/core/quota"
	"tinycld.org/core/webdav"
	"tinycld.org/multi-org/internal/davconfig"
	"tinycld.org/multi-org/internal/lockfile"
	"tinycld.org/multi-org/internal/materialize"
	"tinycld.org/multi-org/internal/orgerr"
	"tinycld.org/multi-org/internal/store"
)

// Timeouts governing a tenant's lifecycle. These are package-level rather than
// configurable because they encode reasoning, not preference — see each one.
const (
	// spawnTimeout bounds a cold boot: PocketBase bootstrap, RunAllMigrations,
	// and JS compilation of the whole materialized hook farm. It is generous
	// because every *fast* failure (crash, explicit error) short-circuits via
	// the readiness pipe, so this only fires on a genuine wedge.
	spawnTimeout = 45 * time.Second

	// drainTimeout is how long in-flight requests get before the child is
	// signalled. See OrgInstance.shutdown for why this always elapses in full
	// for an org with an open SSE stream, and why that is acceptable.
	drainTimeout = 10 * time.Second

	// killTimeout is how long the child gets to honour SIGTERM before SIGKILL.
	// It must EXCEED drainTimeout: the child is handed --drain drainTimeout as
	// its own post-SIGTERM graceful budget, and a child holding an open SSE
	// stream legitimately uses all of it (its srv.Shutdown returns only at ctx
	// expiry, then it exits itself). The margin on top is for the exit path
	// after the child's drain — Sentry flush, bootstrap reset — so SIGKILL
	// fires only for a genuinely wedged child, never one still inside the
	// window it was promised.
	killTimeout = drainTimeout + 5*time.Second

	backoffMin = 1 * time.Second
	backoffMax = 30 * time.Second
)

// OrgRecord is the subset of an org's control-plane record the manager needs to
// load it.
type OrgRecord struct {
	Slug     string
	Status   string
	Lockfile []byte

	// DisplayName is the org's human-facing name from the control-plane
	// record. Materialized into .runtime/app.json (orgName) and adopted as the
	// tenant's Settings().Meta.AppName, which is where the client's
	// /api/org-info reads branding from. Empty leaves the tenant's stored
	// AppName alone.
	DisplayName string

	// StorageLimitBytes is the org's ceiling (0 = unlimited), set by the
	// operator on the control-plane record. Materialized into the org's runtime
	// config so the tenant enforces it but cannot change it.
	StorageLimitBytes int64
}

// LookupFunc resolves an org's control-plane record by slug.
type LookupFunc func(slug string) (OrgRecord, bool)

type Config struct {
	Root      string
	Store     *store.PackageStore
	LookupOrg LookupFunc
	HooksPool int
	MaxIdle   time.Duration // 0 => no idle eviction sweeper

	// Spawner starts tenant processes. Production wiring passes NewSpawner();
	// tests substitute an in-process fake.
	Spawner Spawner

	// TenantBinary is the path to the serve-org executable.
	TenantBinary string

	Logger *slog.Logger

	// CardDAVSources returns the CardDAV sources an org's resolved package set
	// contributes (read from each package's `carddav` manifest block). The host
	// resolves them because it already holds the resolved package list; the
	// result is written to the org's runtime dir and read by the tenant, which
	// serves CardDAV itself against its own DB.
	CardDAVSources func(resolved []lockfile.ResolvedPackage) ([]carddav.Source, error)

	// WebDAVSources is CardDAVSources' counterpart for WebDAV trees (read from
	// each package's `webdav` manifest block).
	WebDAVSources func(resolved []lockfile.ResolvedPackage) ([]webdav.Source, error)

	// CalDAVSources is CardDAVSources' counterpart for calendar trees (read from
	// each package's `caldav` manifest block).
	CalDAVSources func(resolved []lockfile.ResolvedPackage) ([]caldav.Source, error)

	// QuotaSources returns the storage-bearing collections an org's resolved
	// package set declares (from each package's `quota` manifest block).
	QuotaSources func(resolved []lockfile.ResolvedPackage) ([]quota.Source, error)

	// PackageSlugs returns the manifest slugs of an org's resolved package
	// set. Written to .runtime/packages.json and read by serve-org, which
	// uses it to gate FEATURE Go registration against the pinned menu the
	// tenant binary links (internal/tenantpkgs) — an org that has not
	// installed a package must not get its hooks or background goroutines.
	// Host-side for the same reason as the DAV sources: the host already
	// holds the resolved list, and the child must not walk the store.
	PackageSlugs func(resolved []lockfile.ResolvedPackage) ([]string, error)

	// Forwarded controls how each tenant's proxy constructs the
	// X-Forwarded-* headers (client IP chain and public scheme). The zero
	// value behaves like proxy mode with the scheme taken from the router's
	// own hop; serve-multi wires it from MT_TLS_MODE.
	Forwarded ForwardedConfig

	// OrgURL returns an org's public URL (scheme + host, no trailing slash),
	// e.g. "https://acme.tinycld.org". Materialized to .runtime/app.json and
	// adopted by the tenant as Settings().Meta.AppURL at boot — the value
	// PocketBase interpolates into {APP_URL} for verification, password-reset
	// and email-change links. The host resolves it because only the host knows
	// MT_BASE_DOMAIN and the TLS mode; without it every tenant's auth emails
	// carry PB's default http://localhost:8090. Nil ⇒ nothing materialized and
	// the tenant's stored settings are left untouched.
	OrgURL func(slug string) string

	// BaseDomain is MT_BASE_DOMAIN, materialized into each org's app.json so
	// the tenant can scope the cross-org switcher cookie to the parent domain.
	// Empty ⇒ tenants set no cross-org cookie.
	BaseDomain string
}

// crashState tracks a slug's consecutive unexpected exits. It lives on the
// manager rather than the instance so it survives the instance being removed
// from the map — which is exactly what a crash does.
type crashState struct {
	consecutive int
	until       time.Time // no spawn attempt before this
}

type OrgManager struct {
	cfg      Config
	group    singleflight.Group
	mu       sync.RWMutex
	orgs     map[string]*OrgInstance
	crashes  map[string]*crashState
	closed   bool
	stop     chan struct{}
	stopOnce sync.Once

	// evictEpoch counts evictions per slug. An in-flight load captures the
	// value before it spawns and re-checks it before publishing: if an Evict
	// landed in between, the instance it just built is already stale and must
	// be torn down rather than published. Without this an Evict racing a cold
	// start matches nothing in `orgs` and is silently dropped, leaving a
	// suspended or redeployed org serving from the pre-evict generation.
	evictEpoch map[string]uint64
}

func nowNanos() int64 { return time.Now().UnixNano() }

func New(cfg Config) *OrgManager {
	if cfg.HooksPool <= 0 {
		cfg.HooksPool = 15
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Spawner == nil {
		cfg.Spawner = NewSpawner(cfg.Logger)
	}
	m := &OrgManager{
		cfg:        cfg,
		orgs:       map[string]*OrgInstance{},
		crashes:    map[string]*crashState{},
		evictEpoch: map[string]uint64{},
		stop:       make(chan struct{}),
	}
	if cfg.MaxIdle > 0 {
		go m.sweep()
	}
	return m
}

// Get returns the org's running instance, spawning it if necessary.
//
// Concurrent first-requests for one slug collapse into a single spawn. The
// caller's ctx cancels only this caller's wait, not the shared spawn: one
// client hanging up must not abort a cold start that others are waiting on.
func (m *OrgManager) Get(ctx context.Context, slug string) (*OrgInstance, error) {
	m.mu.RLock()
	inst, ok := m.orgs[slug]
	m.mu.RUnlock()
	if ok {
		inst.touch(nowNanos())
		return inst, nil
	}

	ch := m.group.DoChan(slug, func() (any, error) {
		// Re-check under the lock: an Evict racing a Get must resolve to one
		// load, not two.
		m.mu.RLock()
		inst, ok := m.orgs[slug]
		m.mu.RUnlock()
		if ok {
			return inst, nil
		}
		// Deliberately NOT the caller's ctx — see the doc comment.
		loadCtx, cancel := context.WithTimeout(context.Background(), spawnTimeout)
		defer cancel()
		return m.load(loadCtx, slug)
	})

	select {
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		inst := res.Val.(*OrgInstance)
		inst.touch(nowNanos())
		return inst, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *OrgManager) load(ctx context.Context, slug string) (*OrgInstance, error) {
	// Captured before any work: anything this load observes about the org
	// (status, lockfile, materialized packages) is only valid so long as no
	// Evict has intervened by the time we publish.
	m.mu.RLock()
	startEpoch := m.evictEpoch[slug]
	m.mu.RUnlock()

	rec, ok := m.cfg.LookupOrg(slug)
	if !ok {
		return nil, fmt.Errorf("%w: %q", orgerr.ErrOrgNotFound, slug)
	}
	if rec.Status != "active" {
		return nil, fmt.Errorf("%w: %q is %s", orgerr.ErrOrgNotActive, slug, rec.Status)
	}

	if wait, ok := m.backoffRemaining(slug); ok {
		return nil, fmt.Errorf("%w: %q is crash-looping, retry in %s",
			orgerr.ErrOrgUnavailable, slug, wait.Round(time.Second))
	}

	orgDir := filepath.Join(m.cfg.Root, "pb_orgs", slug)

	// Before materialize, so the Linux spawner's chownTree walk picks the
	// directory up and hands it to the tenant uid along with everything else.
	if err := ensureTenantDirs(orgDir); err != nil {
		return nil, fmt.Errorf("%w: prepare runtime dirs for %s: %w",
			orgerr.ErrOrgUnavailable, slug, err)
	}

	lf, err := lockfile.Parse(rec.Lockfile)
	if err != nil {
		return nil, fmt.Errorf("lockfile parse for %s: %w", slug, err)
	}
	resolved, err := lf.Resolve(m.cfg.Store)
	if err != nil {
		return nil, fmt.Errorf("lockfile resolve for %s: %w", slug, err)
	}
	if err := materialize.Materialize(orgDir, resolved); err != nil {
		return nil, fmt.Errorf("materialize %s: %w", slug, err)
	}

	davConfig, err := m.writeCardDAVConfig(orgDir, resolved)
	if err != nil {
		return nil, fmt.Errorf("carddav config %s: %w", slug, err)
	}

	caldavConfig, err := m.writeCalDAVConfig(orgDir, resolved)
	if err != nil {
		return nil, fmt.Errorf("caldav config %s: %w", slug, err)
	}

	webdavConfig, err := m.writeWebDAVConfig(orgDir, resolved)
	if err != nil {
		return nil, fmt.Errorf("webdav config %s: %w", slug, err)
	}

	quotaConfig, err := m.writeQuotaConfig(orgDir, rec, resolved)
	if err != nil {
		return nil, fmt.Errorf("quota config %s: %w", slug, err)
	}

	slugs, err := m.resolvedSlugs(resolved)
	if err != nil {
		return nil, fmt.Errorf("resolve package slugs %s: %w", slug, err)
	}

	packagesConfig, err := m.writePackagesConfig(orgDir, slugs, m.cfg.PackageSlugs != nil)
	if err != nil {
		return nil, fmt.Errorf("packages config %s: %w", slug, err)
	}

	appConfig, err := m.writeAppConfig(orgDir, slug, rec)
	if err != nil {
		return nil, fmt.Errorf("app config %s: %w", slug, err)
	}

	inst, err := m.spawn(ctx, slug, orgDir, runtimeConfigs{
		cardDAV:  davConfig,
		calDAV:   caldavConfig,
		webDAV:   webdavConfig,
		quota:    quotaConfig,
		packages: packagesConfig,
		app:      appConfig,
	}, slices.Contains(slugs, "mail"))
	if err != nil {
		m.noteCrash(slug)
		return nil, err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		go inst.shutdown(drainTimeout, killTimeout)
		return nil, fmt.Errorf("manager shut down while loading org %q", slug)
	}
	if m.evictEpoch[slug] != startEpoch {
		// Suspend, archive, or deploy landed while this instance was starting.
		// It was built from the pre-evict record, so publishing it would keep
		// serving exactly what the operator just revoked. Tear it down and let
		// the next Get build a fresh one from the current record.
		m.mu.Unlock()
		go inst.shutdown(drainTimeout, killTimeout)
		return nil, fmt.Errorf("%w: %q was evicted while starting",
			orgerr.ErrOrgUnavailable, slug)
	}
	m.orgs[slug] = inst
	m.mu.Unlock()
	// Fate decided: the supervisor now owns accounting for this child's exit.
	close(inst.published)
	return inst, nil
}

// runtimeConfigs are the paths to an org's materialized .runtime/*.json files,
// each empty when no resolved package contributes that capability.
//
// Grouped rather than passed as positional strings: they are all the same type,
// so a mix-up at the call site would hand a tenant the wrong protocol's config
// and compile cleanly.
type runtimeConfigs struct {
	cardDAV  string
	calDAV   string
	webDAV   string
	quota    string
	packages string
	app      string
}

// spawn starts the tenant process and waits for it to report readiness.
func (m *OrgManager) spawn(ctx context.Context, slug, orgDir string, cfgs runtimeConfigs, withMail bool) (*OrgInstance, error) {
	socks, err := m.socketPaths(slug, withMail)
	if err != nil {
		return nil, err
	}
	sockPath := socks.http
	// A predecessor killed with SIGKILL leaves its socket files behind, and a
	// stale socket dials ambiguously. Clear them before the child binds.
	for _, p := range socks.all() {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: clear stale socket for %s: %v", orgerr.ErrOrgUnavailable, slug, err)
		}
	}

	readyR, readyW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("%w: readiness pipe for %s: %v", orgerr.ErrOrgUnavailable, slug, err)
	}
	defer readyR.Close()

	log := m.cfg.Logger
	proc, err := m.cfg.Spawner.Spawn(ctx, SpawnRequest{
		Slug:           slug,
		OrgDir:         orgDir,
		SocketPath:     sockPath,
		IMAPSocketPath: socks.imap,
		SMTPSocketPath: socks.smtp,
		MXSocketPath:   socks.mx,
		BinaryPath:     m.cfg.TenantBinary,
		CardDAVConfig:  cfgs.cardDAV,
		CalDAVConfig:   cfgs.calDAV,
		WebDAVConfig:   cfgs.webDAV,
		QuotaConfig:    cfgs.quota,
		PackagesConfig: cfgs.packages,
		AppConfig:      cfgs.app,
		HooksPool:      m.cfg.HooksPool,
		Drain:          drainTimeout,
		ReadyFile:      readyW,
		PackagesDir:    filepath.Join(m.cfg.Root, "packages"),
	}, log)
	// The write end belongs to the child now; the host must close its copy or
	// it will never observe EOF when the child dies.
	readyW.Close()
	if err != nil {
		return nil, fmt.Errorf("%w: spawn %s: %v", orgerr.ErrOrgUnavailable, slug, err)
	}

	inst := &OrgInstance{
		slug:      slug,
		sockPath:  sockPath,
		mailSocks: MailSockets{IMAP: socks.imap, SMTP: socks.smtp, MX: socks.mx},
		proc:      proc,
		proxy:     newProxy(sockPath, m.cfg.Forwarded, log),
		closed:    make(chan struct{}),
		dead:      make(chan struct{}),
		torndown:  make(chan struct{}),
		published: make(chan struct{}),
		log:       log,
	}
	inst.handler = http.HandlerFunc(inst.serveProxied)
	inst.lastUsed.Store(nowNanos())

	// Start reaping before waiting on readiness: a child that dies during boot
	// must close `dead` so the failure path below can complete.
	go m.supervise(inst)

	if err := awaitReady(ctx, readyR, inst); err != nil {
		// Host-initiated teardown of a spawn that never went live: mark it so
		// the supervisor doesn't ALSO account for the exit — the caller
		// records exactly one crash for the failed load, and a child the host
		// itself killed must not be logged as an unexpected exit.
		close(inst.closed)
		// Kill rather than TERM — it is either unresponsive or already broken —
		// and reap it, or a late-successful child would keep holding the socket.
		_ = proc.Kill()
		<-inst.dead
		_ = os.Remove(sockPath)
		return nil, err
	}

	// Record which socket files the child bound, so a later teardown can tell
	// its own sockets from a replacement's that re-bound the same paths (see
	// OrgInstance.ownsSocket). Mail sockets bind before readiness (the mail
	// listeners start during the tenant's OnServe chain), so their inodes are
	// observable here too.
	inst.sockIno = socketInode(sockPath)
	for _, p := range []string{socks.imap, socks.smtp, socks.mx} {
		if p != "" {
			inst.mailSockRefs = append(inst.mailSockRefs, sockRef{path: p, ino: socketInode(p)})
		}
	}

	m.clearCrash(slug)
	return inst, nil
}

// socketInode returns the inode of the file at path, or 0 when unobservable —
// the fail-safe value: teardown skips removal for a socket it cannot identify.
func socketInode(path string) uint64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return sys.Ino
	}
	return 0
}

// maxSocketPath is the practical ceiling for a unix socket path. The kernel's
// sockaddr_un.sun_path is 104 bytes on darwin/BSD and 108 on Linux; exceeding
// it fails at bind() with a bare "invalid argument". 100 leaves room for the
// NUL and keeps one limit across platforms.
const maxSocketPath = 100

// Basenames of the per-org mail sockets. Fixed short names: they share the
// org's socket directory (and its sun_path budget) with the HTTP socket.
const (
	imapSockName = "imap.sock"
	smtpSockName = "smtp.sock"
	mxSockName   = "mx.sock"
)

// orgSockets are the unix sockets one org's tenant serves on. http is always
// set; the mail sockets are set only for an org whose resolved package set
// includes mail.
type orgSockets struct {
	http string
	imap string
	smtp string
	mx   string
}

// all returns the non-empty socket paths.
func (s orgSockets) all() []string {
	out := []string{s.http}
	for _, p := range []string{s.imap, s.smtp, s.mx} {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// socketPaths resolves an org's socket directory and every socket in it,
// falling back to a short path under the system temp dir when MT_ROOT is deep
// enough to overrun the kernel's limit.
//
// All of an org's sockets live in ONE per-org directory. The Linux spawner
// chowns that directory — and only that directory — to the tenant's uid; a
// shared socket directory would end up owned by whichever tenant spawned
// last, and owning the directory is owning every org's socket: unlink a
// sibling's and bind your own in its place to intercept that org's traffic.
// The parent is traversal-only (0711): tenants pass through it to reach their
// own dir but cannot list it or unlink each other's. The primary-vs-fallback
// decision is therefore made once per org, on the longest basename needed —
// splitting an org's sockets across directories would leave some in a dir the
// spawner never chowned.
func (m *OrgManager) socketPaths(slug string, withMail bool) (orgSockets, error) {
	longestBase := func(httpBase string) string {
		longest := httpBase
		if withMail {
			for _, n := range []string{imapSockName, smtpSockName, mxSockName} {
				if len(n) > len(longest) {
					longest = n
				}
			}
		}
		return longest
	}
	build := func(dir, httpBase string) orgSockets {
		s := orgSockets{http: filepath.Join(dir, httpBase)}
		if withMail {
			s.imap = filepath.Join(dir, imapSockName)
			s.smtp = filepath.Join(dir, smtpSockName)
			s.mx = filepath.Join(dir, mxSockName)
		}
		return s
	}

	runDir := filepath.Join(m.cfg.Root, "run")
	primaryDir := filepath.Join(runDir, slug)
	if len(filepath.Join(primaryDir, longestBase(slug+".sock"))) <= maxSocketPath {
		if err := secureRuntimeDir(runDir, 0o711); err != nil {
			return orgSockets{}, fmt.Errorf("%w: create run dir for %s: %v", orgerr.ErrOrgUnavailable, slug, err)
		}
		if err := secureRuntimeDir(primaryDir, 0o700); err != nil {
			return orgSockets{}, fmt.Errorf("%w: create socket dir for %s: %v", orgerr.ErrOrgUnavailable, slug, err)
		}
		return build(primaryDir, slug+".sock"), nil
	}

	// The sockets are rendezvous points, not state: they are recreated on
	// every spawn and removed on teardown, so relocating them costs nothing.
	// The fallback exists because the primary overran sun_path, so it spends
	// its budget carefully: the slug appears only as the per-org directory
	// name and the sockets keep fixed short basenames.
	fallbackParent := fallbackSocketParent(m.cfg.Root)
	fallbackDir := filepath.Join(fallbackParent, slug)
	if len(filepath.Join(fallbackDir, longestBase("s.sock"))) > maxSocketPath {
		return orgSockets{}, fmt.Errorf("%w: socket path for %s exceeds %d bytes", orgerr.ErrOrgUnavailable, slug, maxSocketPath)
	}
	if err := secureRuntimeDir(fallbackParent, 0o711); err != nil {
		return orgSockets{}, fmt.Errorf("%w: create socket parent dir for %s: %v", orgerr.ErrOrgUnavailable, slug, err)
	}
	if err := secureRuntimeDir(fallbackDir, 0o700); err != nil {
		return orgSockets{}, fmt.Errorf("%w: create socket dir for %s: %v", orgerr.ErrOrgUnavailable, slug, err)
	}
	return build(fallbackDir, "s.sock"), nil
}

// runtimeDirRoot is the preferred home for the fallback socket tree: a
// root-owned directory the host controls, rather than the shared world-writable
// temp dir. Overridable for tests and for hosts without /run.
var runtimeDirRoot = "/run/tinycld-mt"

// secureRuntimeDir creates dir with the given mode, or validates it if it
// already exists, refusing anything we cannot safely chown afterwards.
//
// The spawner runs as root and chowns the per-org socket dir to a tenant uid;
// os.MkdirAll happily accepts a path that already resolves to a directory, and
// both it and os.Chown follow symlinks. Since the fallback path is derived only
// from MT_ROOT — predictable to anyone who can read the unit file — an
// unprivileged local user who wins the race to create it as a symlink gets the
// target chowned to a tenant uid. /etc is the obvious target.
//
// So: never follow a symlink, and never adopt a directory that is writable by
// anyone but its owner. Failing closed here costs an org its cold start; the
// alternative costs the host.
func secureRuntimeDir(dir string, mode os.FileMode) error {
	st, err := os.Lstat(dir)
	switch {
	case os.IsNotExist(err):
		if err := os.MkdirAll(dir, mode); err != nil {
			return fmt.Errorf("create runtime dir %s: %w", dir, err)
		}
		// MkdirAll honours umask, so set the mode explicitly.
		if err := os.Chmod(dir, mode); err != nil {
			return fmt.Errorf("chmod runtime dir %s: %w", dir, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("stat runtime dir %s: %w", dir, err)
	}

	if st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("runtime dir %s is a symlink; refusing to use it "+
			"(a chown through it would land on the link target)", dir)
	}
	if !st.IsDir() {
		return fmt.Errorf("runtime dir %s exists and is not a directory", dir)
	}
	// Group/other-writable means someone else can swap what lives inside it.
	if perm := st.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("runtime dir %s is group/other-writable (%o); refusing "+
			"to place tenant sockets there", dir, perm)
	}
	if err := os.Chmod(dir, mode); err != nil {
		return fmt.Errorf("chmod runtime dir %s: %w", dir, err)
	}
	return nil
}

// fallbackSocketParent picks the directory holding the per-org socket dirs when
// the primary path would overrun sun_path.
//
// Preference order is the host-owned runtime root, then the temp dir. The temp
// dir remains as a fallback-of-the-fallback because a non-root developer host
// has no writable /run — but it is only ever used through secureRuntimeDir,
// which refuses a planted or world-writable path.
func fallbackSocketParent(root string) string {
	digest := sha256.Sum256([]byte(root))
	name := fmt.Sprintf("mt-%x", digest[:6])

	if err := secureRuntimeDir(runtimeDirRoot, 0o711); err == nil {
		return filepath.Join(runtimeDirRoot, name)
	}
	return filepath.Join(os.TempDir(), name)
}

// readyMsg is the single line a tenant writes to its readiness pipe.
type readyMsg struct {
	OK    bool   `json:"ok"`
	PID   int    `json:"pid"`
	Error string `json:"error,omitempty"`
}

// awaitReady blocks until the child reports it is serving, reports a failure,
// dies (EOF), or the context expires.
//
// Reading a pipe beats polling the socket: a stale socket file dials
// ambiguously, and — more importantly — a crash during boot surfaces as EOF in
// milliseconds instead of burning the full timeout, which is what keeps the
// crash-loop backoff responsive.
func awaitReady(ctx context.Context, r *os.File, inst *OrgInstance) error {
	type result struct {
		msg readyMsg
		err error
	}
	done := make(chan result, 1)

	go func() {
		scanner := bufio.NewScanner(r)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				done <- result{err: fmt.Errorf("read readiness: %w", err)}
				return
			}
			done <- result{err: fmt.Errorf("exited before signalling ready")}
			return
		}
		var msg readyMsg
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			done <- result{err: fmt.Errorf("malformed readiness message: %w", err)}
			return
		}
		done <- result{msg: msg}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			// Prefer the child's exit status when it has one: "exit status 1"
			// is more useful than "exited before signalling ready".
			select {
			case <-inst.dead:
				if inst.exitErr != nil {
					return fmt.Errorf("%w: %s: %v", orgerr.ErrOrgUnavailable, inst.slug, inst.exitErr)
				}
			default:
			}
			return fmt.Errorf("%w: %s: %v", orgerr.ErrOrgUnavailable, inst.slug, res.err)
		}
		if !res.msg.OK {
			return fmt.Errorf("%w: %s failed to start: %s", orgerr.ErrOrgUnavailable, inst.slug, res.msg.Error)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%w: %s did not become ready in %s", orgerr.ErrOrgUnavailable, inst.slug, spawnTimeout)
	}
}

// supervise reaps the child and, if it died unexpectedly, drops it from the map
// so the next Get respawns it.
func (m *OrgManager) supervise(inst *OrgInstance) {
	inst.exitErr = inst.proc.Wait()
	close(inst.dead)

	select {
	case <-inst.closed:
		return // expected: shutdown is driving and is waiting on dead
	default:
	}

	// The child can die before load has decided the instance's fate (a crash
	// during boot). Block until it does: a published instance's exit is ours
	// to account for below; an abandoned spawn closes `closed` and its load
	// path records the one crash — counting it here too doubled the first
	// backoff and logged a host-killed child as an unexpected exit.
	select {
	case <-inst.closed:
		return
	case <-inst.published:
	}

	m.mu.Lock()
	// Identity check: a crash notification arriving after this slug was already
	// evicted and respawned must not delete the healthy replacement.
	if cur, ok := m.orgs[inst.slug]; ok && cur == inst {
		delete(m.orgs, inst.slug)
	}
	m.mu.Unlock()

	m.noteCrash(inst.slug)
	_ = os.Remove(inst.sockPath)
	for _, ref := range inst.mailSockRefs {
		_ = os.Remove(ref.path)
	}
	m.cfg.Logger.Error("tenant process exited unexpectedly",
		"slug", inst.slug, "pid", inst.proc.Pid(), "error", inst.exitErr)
}

// backoffRemaining reports how long a crash-looping slug must wait.
func (m *OrgManager) backoffRemaining(slug string) (time.Duration, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cs, ok := m.crashes[slug]
	if !ok {
		return 0, false
	}
	if wait := time.Until(cs.until); wait > 0 {
		return wait, true
	}
	return 0, false
}

func (m *OrgManager) noteCrash(slug string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cs, ok := m.crashes[slug]
	if !ok {
		cs = &crashState{}
		m.crashes[slug] = cs
	}
	cs.consecutive++

	backoff := backoffMin << (cs.consecutive - 1)
	if backoff > backoffMax || backoff <= 0 {
		backoff = backoffMax
	}
	// Jitter so a host restart doesn't resynchronise every crash-looping org.
	jitter := time.Duration(rand.Int63n(int64(backoff / 4)))
	cs.until = time.Now().Add(backoff + jitter)
}

// clearCrash resets backoff after a successful boot. Reset-on-success rather
// than on a timer: an org that boots fine and later crashes once should not
// inherit history from a bad deploy an hour ago.
func (m *OrgManager) clearCrash(slug string) {
	m.mu.Lock()
	delete(m.crashes, slug)
	m.mu.Unlock()
}

// writeCardDAVConfig resolves the org's CardDAV sources and writes them where
// the tenant can read them.
//
// The host resolves rather than the child because the host already holds the
// resolved package list. The child would have to readlink its materialized
// symlinks and walk back up into the package store — exactly the path-reaching
// the confinement exists to prevent.
func (m *OrgManager) writeCardDAVConfig(orgDir string, resolved []lockfile.ResolvedPackage) (string, error) {
	if m.cfg.CardDAVSources == nil {
		return "", nil
	}
	sources, err := m.cfg.CardDAVSources(resolved)
	if err != nil {
		return "", err
	}
	if len(sources) == 0 {
		return "", nil
	}

	runtimeDir := filepath.Join(orgDir, ".runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(runtimeDir, "carddav.json")

	body, err := json.Marshal(davconfig.Encode(sources))
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// writeQuotaConfig materializes the org's storage ceiling where the tenant can
// read it.
//
// It comes from the control-plane record rather than the org's own settings
// because each tenant has its own superusers: a limit stored inside the org
// could be raised by the org. Writing it here makes the plan limit the
// operator's to set and the tenant's only to obey.
//
// A zero limit still writes the file — "explicitly unlimited" and "no config"
// should not be distinguishable to the tenant, and the file's presence is what
// tells serve-org the router is managing this.
func (m *OrgManager) writeQuotaConfig(orgDir string, rec OrgRecord, resolved []lockfile.ResolvedPackage) (string, error) {
	var sources []quota.Source
	if m.cfg.QuotaSources != nil {
		var err error
		sources, err = m.cfg.QuotaSources(resolved)
		if err != nil {
			return "", err
		}
	}

	runtimeDir := filepath.Join(orgDir, ".runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(runtimeDir, "quota.json")

	body, err := json.Marshal(quotaConfigFile{
		StorageLimitBytes: rec.StorageLimitBytes,
		Sources:           davconfig.EncodeQuota(sources),
	})
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// quotaConfigFile is the wire shape of .runtime/quota.json.
type quotaConfigFile struct {
	StorageLimitBytes int64                   `json:"storageLimitBytes"`
	Sources           []davconfig.QuotaSource `json:"sources"`
}

// resolvedSlugs resolves the org's package slugs through the configured hook.
// Nil hook ⇒ nil slugs (the host manages no package set; the child registers
// no feature Go and gets no mail sockets).
func (m *OrgManager) resolvedSlugs(resolved []lockfile.ResolvedPackage) ([]string, error) {
	if m.cfg.PackageSlugs == nil {
		return nil, nil
	}
	return m.cfg.PackageSlugs(resolved)
}

// writePackagesConfig materializes the org's resolved package slugs where the
// tenant can read them. Like quota.json it is ALWAYS written when the hook is
// wired — an empty slug list must be indistinguishable from "no packages
// installed" (register no feature Go), never from "config missing". `wired`
// carries whether the PackageSlugs hook exists at all (the caller resolves
// the slugs once, for this file AND the mail-socket decision).
func (m *OrgManager) writePackagesConfig(orgDir string, slugs []string, wired bool) (string, error) {
	if !wired {
		return "", nil
	}

	runtimeDir := filepath.Join(orgDir, ".runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(runtimeDir, "packages.json")

	body, err := json.Marshal(packagesConfigFile{Slugs: slugs})
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// packagesConfigFile is the wire shape of .runtime/packages.json.
type packagesConfigFile struct {
	Slugs []string `json:"slugs"`
}

// writeAppConfig materializes app-level runtime config where the tenant can
// read it: the org's public URL (adopted as Settings().Meta.AppURL — the value
// PB interpolates into {APP_URL} in every auth email — because the child knows
// neither MT_BASE_DOMAIN nor the TLS mode) and the proxy-trust headers
// (adopted as Settings().TrustedProxy — over the unix socket the tenant's
// RemoteAddr is empty, so without trusting the router's X-Forwarded-For every
// request shares one rate-limit bucket). Always written: every router-managed
// tenant is fronted by the router, so the trust always applies even when no
// OrgURL hook is wired.
func (m *OrgManager) writeAppConfig(orgDir, slug string, rec OrgRecord) (string, error) {
	runtimeDir := filepath.Join(orgDir, ".runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(runtimeDir, "app.json")

	cfg := appConfigFile{
		TrustedProxyHeaders: []string{"X-Forwarded-For"},
		OrgName:             rec.DisplayName,
		BaseDomain:          m.cfg.BaseDomain,
	}
	if m.cfg.OrgURL != nil {
		cfg.AppURL = m.cfg.OrgURL(slug)
	}
	body, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// appConfigFile is the wire shape of .runtime/app.json.
type appConfigFile struct {
	AppURL string `json:"appURL"`

	// OrgName is the org's display name from the control-plane record, adopted
	// as the tenant's Settings().Meta.AppName (the client reads it back via
	// /api/org-info). Empty = leave the tenant's stored name alone.
	OrgName string `json:"orgName"`

	// BaseDomain is MT_BASE_DOMAIN, so the tenant can scope the cross-org
	// switcher cookie to the parent domain (Domain=.<baseDomain>). Empty = the
	// tenant sets no cross-org cookie.
	BaseDomain string `json:"baseDomain"`

	// TrustedProxyHeaders is what the tenant should set as
	// Settings().TrustedProxy.Headers. The router guarantees the RIGHTMOST
	// entry of the (last) header is the best-known client IP — which is
	// PocketBase's default resolution order (UseLeftmostIP false).
	TrustedProxyHeaders []string `json:"trustedProxyHeaders"`
}

// writeWebDAVConfig is writeCardDAVConfig's counterpart for WebDAV trees. Same
// rationale for resolving host-side.
func (m *OrgManager) writeWebDAVConfig(orgDir string, resolved []lockfile.ResolvedPackage) (string, error) {
	if m.cfg.WebDAVSources == nil {
		return "", nil
	}
	sources, err := m.cfg.WebDAVSources(resolved)
	if err != nil {
		return "", err
	}
	if len(sources) == 0 {
		return "", nil
	}

	runtimeDir := filepath.Join(orgDir, ".runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(runtimeDir, "webdav.json")

	body, err := json.Marshal(davconfig.EncodeWebDAV(sources))
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// writeCalDAVConfig is writeCardDAVConfig's counterpart for calendar trees. Same
// rationale for resolving host-side.
func (m *OrgManager) writeCalDAVConfig(orgDir string, resolved []lockfile.ResolvedPackage) (string, error) {
	if m.cfg.CalDAVSources == nil {
		return "", nil
	}
	sources, err := m.cfg.CalDAVSources(resolved)
	if err != nil {
		return "", err
	}
	if len(sources) == 0 {
		return "", nil
	}

	runtimeDir := filepath.Join(orgDir, ".runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(runtimeDir, "caldav.json")

	body, err := json.Marshal(davconfig.EncodeCalDAV(sources))
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// Evict removes an org's instance and tears down its process. The next Get
// respawns it fresh, picking up new packages/hooks/status.
//
// The map delete is synchronous — that is the part that must be atomic against
// Get, since an instance not in the map can receive no new requests. Teardown
// is handed to a goroutine because Evict is called from inside provisioning
// HTTP handlers (Deploy, suspend/resume/archive) and from the sweeper in a
// loop; blocking either on a full drain would be user-visible for no benefit.
func (m *OrgManager) Evict(slug string) {
	m.mu.Lock()
	// Bumped unconditionally, including when nothing is resident: the whole
	// point is to catch a load that has started but not yet published, which
	// by definition is not in the map yet.
	m.evictEpoch[slug]++
	inst, ok := m.orgs[slug]
	if ok {
		delete(m.orgs, slug)
	}
	m.mu.Unlock()
	if ok {
		go inst.shutdown(drainTimeout, killTimeout)
	}
}

// Shutdown stops the sweeper and tears down every resident instance.
func (m *OrgManager) Shutdown() {
	m.stopOnce.Do(func() { close(m.stop) })

	m.mu.Lock()
	m.closed = true
	insts := make([]*OrgInstance, 0, len(m.orgs))
	for _, inst := range m.orgs {
		insts = append(insts, inst)
	}
	m.orgs = map[string]*OrgInstance{}
	m.mu.Unlock()

	// Tear down concurrently: serially draining N orgs would take N*drain.
	var wg sync.WaitGroup
	for _, inst := range insts {
		wg.Add(1)
		go func(in *OrgInstance) {
			defer wg.Done()
			in.shutdown(drainTimeout, killTimeout)
		}(inst)
	}
	wg.Wait()
}

// sweep evicts instances idle longer than MaxIdle.
func (m *OrgManager) sweep() {
	// Floor the interval: NewTicker panics on a non-positive duration, which a
	// sub-2ns MaxIdle would otherwise produce.
	interval := m.cfg.MaxIdle / 2
	if interval <= 0 {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			m.sweepOnce()
		}
	}
}

// sweepOnce evicts every instance idle longer than MaxIdle. An instance with
// tracked connections open (long-lived IMAP/SMTP relays — see TrackConn) is
// never idle: evicting it would kill the tenant mid-session, and unlike HTTP
// those sessions don't refresh lastUsed per request.
func (m *OrgManager) sweepOnce() {
	cutoff := time.Now().Add(-m.cfg.MaxIdle).UnixNano()
	m.mu.RLock()
	var stale []string
	for slug, inst := range m.orgs {
		lu := inst.lastUsed.Load()
		if lu != 0 && lu < cutoff && inst.activeConns.Load() == 0 {
			stale = append(stale, slug)
		}
	}
	m.mu.RUnlock()
	for _, slug := range stale {
		m.Evict(slug)
	}
}
