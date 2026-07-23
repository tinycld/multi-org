package orgmanager

import (
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/plugins/jsvm"
	"golang.org/x/sync/singleflight"

	"tinycld.org/multi-org/internal/lockfile"
	"tinycld.org/multi-org/internal/materialize"
	"tinycld.org/multi-org/internal/progcache"
	"tinycld.org/multi-org/internal/store"
)

// Ensure the shared cache satisfies the fork's jsvm seam.
var _ jsvm.ProgramSource = (*progcache.SharedProgramCache)(nil)

// OrgRecord is the subset of an org's control-plane record the manager needs to
// load it.
type OrgRecord struct {
	Slug     string
	Status   string
	Lockfile []byte
}

// LookupFunc resolves an org's control-plane record by slug.
type LookupFunc func(slug string) (OrgRecord, bool)

type Config struct {
	Root      string
	Store     *store.PackageStore
	Programs  *progcache.SharedProgramCache
	LookupOrg LookupFunc
	HooksPool int
	MaxIdle   time.Duration // 0 => no idle eviction sweeper
}

type OrgManager struct {
	cfg    Config
	group  singleflight.Group
	mu     sync.RWMutex
	orgs   map[string]*OrgInstance
	closed bool
	stop   chan struct{}
}

func nowNanos() int64 { return time.Now().UnixNano() }

func New(cfg Config) *OrgManager {
	if cfg.HooksPool <= 0 {
		cfg.HooksPool = 15
	}
	m := &OrgManager{cfg: cfg, orgs: map[string]*OrgInstance{}, stop: make(chan struct{})}
	if cfg.MaxIdle > 0 {
		go m.sweep()
	}
	return m
}

// Get returns the org's instance, lazily loading it on first request. Concurrent
// first-requests for the same slug collapse via singleflight to a single load.
func (m *OrgManager) Get(slug string) (*OrgInstance, error) {
	m.mu.RLock()
	if inst, ok := m.orgs[slug]; ok {
		m.mu.RUnlock()
		inst.touch(nowNanos())
		return inst, nil
	}
	m.mu.RUnlock()

	v, err, _ := m.group.Do(slug, func() (any, error) {
		m.mu.RLock()
		if inst, ok := m.orgs[slug]; ok {
			m.mu.RUnlock()
			return inst, nil
		}
		m.mu.RUnlock()
		return m.load(slug)
	})
	if err != nil {
		return nil, err
	}
	inst := v.(*OrgInstance)
	inst.touch(nowNanos())
	return inst, nil
}

func (m *OrgManager) load(slug string) (*OrgInstance, error) {
	rec, ok := m.cfg.LookupOrg(slug)
	if !ok {
		return nil, fmt.Errorf("unknown org %q", slug)
	}
	if rec.Status != "active" {
		return nil, fmt.Errorf("org %q is %s", slug, rec.Status)
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

	pb := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir:  filepath.Join(orgDir, "pb_data"),
		HideStartBanner: true,
	})
	jsvm.MustRegister(pb, jsvm.Config{
		HooksDir:      filepath.Join(orgDir, "pb_hooks"),
		MigrationsDir: filepath.Join(orgDir, "pb_migrations"),
		HooksWatch:    false,
		HooksPoolSize: m.cfg.HooksPool,
		ProgramSource: m.cfg.Programs,
	})
	if err := pb.Bootstrap(); err != nil {
		return nil, fmt.Errorf("bootstrap %s: %w", slug, err)
	}
	if err := pb.App.RunAllMigrations(); err != nil {
		_ = pb.App.ResetBootstrapState()
		return nil, fmt.Errorf("migrations %s: %w", slug, err)
	}

	mux, err := apis.BuildServeMux(pb, apis.ServeConfig{})
	if err != nil {
		_ = pb.App.ResetBootstrapState()
		return nil, fmt.Errorf("build mux %s: %w", slug, err)
	}

	inst := &OrgInstance{slug: slug, app: pb, mux: mux}
	inst.lastUsed.Store(nowNanos())

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = inst.close()
		return nil, fmt.Errorf("manager shut down while loading org %q", slug)
	}
	m.orgs[slug] = inst
	m.mu.Unlock()
	return inst, nil
}

// Evict removes an org's instance and releases its resources. The next Get
// reloads it fresh (picking up new bundle/hooks/status).
func (m *OrgManager) Evict(slug string) {
	m.mu.Lock()
	inst, ok := m.orgs[slug]
	if ok {
		delete(m.orgs, slug)
	}
	m.mu.Unlock()
	if ok {
		_ = inst.close()
	}
}

// Shutdown stops the sweeper and closes all resident instances.
func (m *OrgManager) Shutdown() {
	close(m.stop)
	m.mu.Lock()
	m.closed = true
	insts := make([]*OrgInstance, 0, len(m.orgs))
	for _, inst := range m.orgs {
		insts = append(insts, inst)
	}
	m.orgs = map[string]*OrgInstance{}
	m.mu.Unlock()
	for _, inst := range insts {
		_ = inst.close()
	}
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
