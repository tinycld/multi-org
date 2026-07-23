package controlplane

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"

	"tinycld.org/multitenant/internal/lockfile"
	"tinycld.org/multitenant/internal/materialize"
	"tinycld.org/multitenant/internal/store"
)

// EvictFunc lets provisioning invalidate a cached org instance in the manager.
type EvictFunc func(slug string)

// Provisioner performs control-plane provisioning operations against the orgs/
// packages/deployments collections and the package store.
type Provisioner struct {
	app   core.App
	root  string
	store *store.PackageStore
	evict EvictFunc
}

func NewProvisioner(app core.App, root string, s *store.PackageStore, evict EvictFunc) *Provisioner {
	return &Provisioner{app: app, root: root, store: s, evict: evict}
}

var slugRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func validSlug(s string) bool { return slugRe.MatchString(s) }

// CreateOrg provisions a new org: validates the slug, writes the dir tree,
// materializes from the lockfile, creates the org row, bootstraps the tenant DB
// once to run migrations, then flips the row to active. Fails if the slug exists.
func (p *Provisioner) CreateOrg(slug, displayName string, lock map[string]string) (*core.Record, error) {
	if !validSlug(slug) {
		return nil, fmt.Errorf("invalid slug %q", slug)
	}
	col, err := p.app.FindCollectionByNameOrId("orgs")
	if err != nil {
		return nil, err
	}
	if existing, _ := p.app.FindFirstRecordByData("orgs", "slug", slug); existing != nil {
		return nil, fmt.Errorf("org %q already exists", slug)
	}

	orgDir := filepath.Join(p.root, "pb_orgs", slug)
	for _, sub := range []string{"pb_data", "pb_hooks", "pb_public", "pb_migrations"} {
		if err := os.MkdirAll(filepath.Join(orgDir, sub), 0o755); err != nil {
			return nil, err
		}
	}

	lfBytes, err := lockfile.OrgLockfile(lock).Marshal()
	if err != nil {
		return nil, err
	}
	lf, err := lockfile.Parse(lfBytes)
	if err != nil {
		return nil, err
	}
	resolved, err := lf.Resolve(p.store)
	if err != nil {
		return nil, err
	}
	if err := materialize.Materialize(orgDir, resolved); err != nil {
		return nil, err
	}

	rec := core.NewRecord(col)
	rec.Set("slug", slug)
	rec.Set("display_name", displayName)
	rec.Set("status", "provisioning")
	rec.Set("data_dir", filepath.Join("pb_orgs", slug))
	rec.Set("lockfile", string(lfBytes))
	rec.Set("custom_domains", "[]")
	if err := p.app.Save(rec); err != nil {
		return nil, err
	}

	if err := bootstrapTenantOnce(orgDir); err != nil {
		return nil, fmt.Errorf("tenant bootstrap: %w", err)
	}

	rec.Set("status", "active")
	if err := p.app.Save(rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// Deploy re-resolves a new lockfile, re-materializes, records a deployment, and
// evicts the running instance so the next request loads fresh.
func (p *Provisioner) Deploy(slug string, lock map[string]string) error {
	rec, err := p.app.FindFirstRecordByData("orgs", "slug", slug)
	if err != nil || rec == nil {
		return fmt.Errorf("org %q not found", slug)
	}
	lfBytes, err := lockfile.OrgLockfile(lock).Marshal()
	if err != nil {
		return err
	}
	lf, err := lockfile.Parse(lfBytes)
	if err != nil {
		return err
	}
	resolved, err := lf.Resolve(p.store)
	if err != nil {
		return err
	}
	orgDir := filepath.Join(p.root, "pb_orgs", slug)
	if err := materialize.Materialize(orgDir, resolved); err != nil {
		return err
	}
	rec.Set("lockfile", string(lfBytes))
	if err := p.app.Save(rec); err != nil {
		return err
	}
	if dcol, derr := p.app.FindCollectionByNameOrId("deployments"); derr == nil {
		d := core.NewRecord(dcol)
		d.Set("org", rec.Id) // relation set by org record id
		d.Set("lockfile", string(lfBytes))
		_ = p.app.Save(d)
	}
	p.evict(slug)
	return nil
}

func (p *Provisioner) setStatus(slug, status string) error {
	rec, err := p.app.FindFirstRecordByData("orgs", "slug", slug)
	if err != nil || rec == nil {
		return fmt.Errorf("org %q not found", slug)
	}
	rec.Set("status", status)
	if err := p.app.Save(rec); err != nil {
		return err
	}
	p.evict(slug)
	return nil
}

func (p *Provisioner) Suspend(slug string) error { return p.setStatus(slug, "suspended") }
func (p *Provisioner) Resume(slug string) error  { return p.setStatus(slug, "active") }
func (p *Provisioner) Archive(slug string) error { return p.setStatus(slug, "archived") }

// PublishPackage writes a package version into the store and registers it in the
// packages collection.
func (p *Provisioner) PublishPackage(name, version string, files map[string][]byte, kind string) error {
	if err := p.store.Publish(name, version, files); err != nil {
		return err
	}
	col, err := p.app.FindCollectionByNameOrId("packages")
	if err != nil {
		return err
	}
	rec := core.NewRecord(col)
	rec.Set("name", name)
	rec.Set("version", version)
	rec.Set("store_path", filepath.Join("packages", name, version))
	rec.Set("kind", kind)
	return p.app.Save(rec)
}

func bootstrapTenantOnce(orgDir string) error {
	pb := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir:  filepath.Join(orgDir, "pb_data"),
		HideStartBanner: true,
	})
	if err := pb.Bootstrap(); err != nil {
		return err
	}
	defer pb.App.ResetBootstrapState()
	return pb.App.RunAllMigrations()
}
