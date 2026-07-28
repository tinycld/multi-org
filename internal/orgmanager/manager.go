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
	killTimeout = 5 * time.Second

	backoffMin = 1 * time.Second
	backoffMax = 30 * time.Second
)

// OrgRecord is the subset of an org's control-plane record the manager needs to
// load it.
type OrgRecord struct {
	Slug     string
	Status   string
	Lockfile []byte

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

	// OrgURL returns an org's public URL (scheme + host, no trailing slash),
	// e.g. "https://acme.tinycld.org". Materialized to .runtime/app.json and
	// adopted by the tenant as Settings().Meta.AppURL at boot — the value
	// PocketBase interpolates into {APP_URL} for verification, password-reset
	// and email-change links. The host resolves it because only the host knows
	// MT_BASE_DOMAIN and the TLS mode; without it every tenant's auth emails
	// carry PB's default http://localhost:8090. Nil ⇒ nothing materialized and
	// the tenant's stored settings are left untouched.
	OrgURL func(slug string) string
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
		cfg:     cfg,
		orgs:    map[string]*OrgInstance{},
		crashes: map[string]*crashState{},
		stop:    make(chan struct{}),
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

	packagesConfig, err := m.writePackagesConfig(orgDir, resolved)
	if err != nil {
		return nil, fmt.Errorf("packages config %s: %w", slug, err)
	}

	appConfig, err := m.writeAppConfig(orgDir, slug)
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
	})
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
	m.orgs[slug] = inst
	m.mu.Unlock()
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
func (m *OrgManager) spawn(ctx context.Context, slug, orgDir string, cfgs runtimeConfigs) (*OrgInstance, error) {
	sockPath, err := m.socketPath(slug)
	if err != nil {
		return nil, err
	}
	// A predecessor killed with SIGKILL leaves its socket file behind, and a
	// stale socket dials ambiguously. Clear it before the child binds.
	if err := os.Remove(sockPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("%w: clear stale socket for %s: %v", orgerr.ErrOrgUnavailable, slug, err)
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
		slug:     slug,
		sockPath: sockPath,
		proc:     proc,
		proxy:    newProxy(sockPath, log),
		closed:   make(chan struct{}),
		dead:     make(chan struct{}),
		log:      log,
	}
	inst.handler = http.HandlerFunc(inst.serveProxied)
	inst.lastUsed.Store(nowNanos())

	// Start reaping before waiting on readiness: a child that dies during boot
	// must close `dead` so the failure path below can complete.
	go m.supervise(inst)

	if err := awaitReady(ctx, readyR, inst); err != nil {
		// Kill rather than TERM — it is either unresponsive or already broken —
		// and reap it, or a late-successful child would keep holding the socket.
		_ = proc.Kill()
		<-inst.dead
		_ = os.Remove(sockPath)
		return nil, err
	}

	// Record which socket file the child bound, so a later teardown can tell
	// its own socket from a replacement's that re-bound the same path (see
	// OrgInstance.ownsSocket).
	if st, err := os.Stat(sockPath); err == nil {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			inst.sockIno = sys.Ino
		}
	}

	m.clearCrash(slug)
	return inst, nil
}

// maxSocketPath is the practical ceiling for a unix socket path. The kernel's
// sockaddr_un.sun_path is 104 bytes on darwin/BSD and 108 on Linux; exceeding
// it fails at bind() with a bare "invalid argument". 100 leaves room for the
// NUL and keeps one limit across platforms.
const maxSocketPath = 100

// socketPath resolves an org's socket, falling back to a short path under the
// system temp dir when MT_ROOT is deep enough to overrun the kernel's limit.
//
// Each socket lives in its own per-org directory. The Linux spawner chowns
// that directory — and only that directory — to the tenant's uid; a shared
// socket directory would end up owned by whichever tenant spawned last, and
// owning the directory is owning every org's socket: unlink a sibling's and
// bind your own in its place to intercept that org's traffic. The parent is
// traversal-only (0711): tenants pass through it to reach their own dir but
// cannot list it or unlink each other's.
func (m *OrgManager) socketPath(slug string) (string, error) {
	runDir := filepath.Join(m.cfg.Root, "run")
	primary := filepath.Join(runDir, slug, slug+".sock")
	if len(primary) <= maxSocketPath {
		if err := os.MkdirAll(runDir, 0o711); err != nil {
			return "", fmt.Errorf("%w: create run dir for %s: %v", orgerr.ErrOrgUnavailable, slug, err)
		}
		if err := os.MkdirAll(filepath.Dir(primary), 0o700); err != nil {
			return "", fmt.Errorf("%w: create socket dir for %s: %v", orgerr.ErrOrgUnavailable, slug, err)
		}
		return primary, nil
	}

	// The socket is a rendezvous point, not state: it is recreated on every
	// spawn and removed on teardown, so relocating it costs nothing. The
	// fallback exists because the primary overran sun_path, so it spends its
	// budget carefully: the slug appears only as the per-org directory name
	// and the socket itself keeps a fixed short basename.
	digest := sha256.Sum256([]byte(m.cfg.Root))
	fallbackParent := filepath.Join(os.TempDir(), fmt.Sprintf("mt-%x", digest[:6]))
	fallbackDir := filepath.Join(fallbackParent, slug)
	fallback := filepath.Join(fallbackDir, "s.sock")
	if len(fallback) > maxSocketPath {
		return "", fmt.Errorf("%w: socket path for %s exceeds %d bytes", orgerr.ErrOrgUnavailable, slug, maxSocketPath)
	}
	if err := os.MkdirAll(fallbackParent, 0o711); err != nil {
		return "", fmt.Errorf("%w: create socket parent dir for %s: %v", orgerr.ErrOrgUnavailable, slug, err)
	}
	if err := os.MkdirAll(fallbackDir, 0o700); err != nil {
		return "", fmt.Errorf("%w: create socket dir for %s: %v", orgerr.ErrOrgUnavailable, slug, err)
	}
	return fallback, nil
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

	m.mu.Lock()
	// Identity check: a crash notification arriving after this slug was already
	// evicted and respawned must not delete the healthy replacement.
	if cur, ok := m.orgs[inst.slug]; ok && cur == inst {
		delete(m.orgs, inst.slug)
	}
	m.mu.Unlock()

	m.noteCrash(inst.slug)
	_ = os.Remove(inst.sockPath)
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

// writePackagesConfig materializes the org's resolved package slugs where the
// tenant can read them. Like quota.json it is ALWAYS written when the hook is
// wired — an empty slug list must be indistinguishable from "no packages
// installed" (register no feature Go), never from "config missing".
func (m *OrgManager) writePackagesConfig(orgDir string, resolved []lockfile.ResolvedPackage) (string, error) {
	if m.cfg.PackageSlugs == nil {
		return "", nil
	}
	slugs, err := m.cfg.PackageSlugs(resolved)
	if err != nil {
		return "", err
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

// writeAppConfig materializes the org's public URL where the tenant can read
// it. The tenant adopts it as Settings().Meta.AppURL at boot — the value PB
// interpolates into {APP_URL} in every auth email — because the child knows
// neither MT_BASE_DOMAIN nor the TLS mode.
func (m *OrgManager) writeAppConfig(orgDir, slug string) (string, error) {
	if m.cfg.OrgURL == nil {
		return "", nil
	}

	runtimeDir := filepath.Join(orgDir, ".runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(runtimeDir, "app.json")

	body, err := json.Marshal(appConfigFile{AppURL: m.cfg.OrgURL(slug)})
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
	ticker := time.NewTicker(m.cfg.MaxIdle / 2)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case now := <-ticker.C:
			cutoff := now.Add(-m.cfg.MaxIdle).UnixNano()
			m.mu.RLock()
			var stale []string
			for slug, inst := range m.orgs {
				lu := inst.lastUsed.Load()
				if lu != 0 && lu < cutoff {
					stale = append(stale, slug)
				}
			}
			m.mu.RUnlock()
			for _, slug := range stale {
				m.Evict(slug)
			}
		}
	}
}
