package controlplane

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"

	"tinycld.org/multi-org/internal/lockfile"
	"tinycld.org/multi-org/internal/materialize"
	"tinycld.org/multi-org/internal/store"
)

// EvictFunc lets provisioning invalidate a cached org instance in the manager.
type EvictFunc func(slug string)

// VerifyFunc boots the org's tenant process and reports whether it became
// ready. serve-multi wires it to the org manager's Get: the spawn applies the
// org's migrations inside the confined tenant (apis.Serve runs
// RunAllMigrations before readiness), and a failure arrives back through the
// readiness pipe carrying the child's reason. This is what keeps untrusted
// tenant migration JS out of the control-plane process entirely — the control
// plane never opens a tenant app.
type VerifyFunc func(ctx context.Context, slug string) error

// Provisioner performs control-plane provisioning operations against the orgs/
// packages/deployments collections and the package store.
type Provisioner struct {
	app    core.App
	root   string
	store  *store.PackageStore
	evict  EvictFunc
	verify VerifyFunc
}

// NewProvisioner builds a Provisioner. verify may be nil, in which case
// CreateOrg activates the org without booting it and the org's migrations run
// at its first spawn instead — that trades the synchronous failure report for
// a 502 on first request, so production wiring should always pass one.
func NewProvisioner(app core.App, root string, s *store.PackageStore, evict EvictFunc, verify VerifyFunc) *Provisioner {
	return &Provisioner{app: app, root: root, store: s, evict: evict, verify: verify}
}

var slugRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// reservedSlugs are subdomain labels the front router routes to the control
// plane / apex redirect (frontrouter.go) — an org created with one would be
// unreachable, so reject them.
var reservedSlugs = map[string]bool{"admin": true, "www": true}

func validSlug(s string) bool { return slugRe.MatchString(s) && !reservedSlugs[s] }

// CreateOrg provisions a new org, or resumes a previously half-provisioned one.
// If an org row for the slug already exists and is still active, it errors; if
// the existing row is in a non-active (e.g. stranded "provisioning") state, it
// resumes: re-materializes, re-verifies the tenant boot, and flips to active.
func (p *Provisioner) CreateOrg(slug, displayName string, lock map[string]string) (*core.Record, error) {
	if !validSlug(slug) {
		return nil, fmt.Errorf("invalid slug %q", slug)
	}
	col, err := p.app.FindCollectionByNameOrId("orgs")
	if err != nil {
		return nil, fmt.Errorf("find orgs collection: %w", err)
	}

	existing, _ := p.app.FindFirstRecordByData("orgs", "slug", slug)
	if existing != nil && existing.GetString("status") == "active" {
		return nil, fmt.Errorf("org %q already exists", slug)
	}

	orgDir := filepath.Join(p.root, "pb_orgs", slug)
	for _, sub := range []string{"pb_data", "pb_hooks", "pb_public", "pb_migrations"} {
		if err := os.MkdirAll(filepath.Join(orgDir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("create org dir: %w", err)
		}
	}

	lfBytes, err := lockfile.OrgLockfile(lock).Marshal()
	if err != nil {
		return nil, fmt.Errorf("marshal lockfile: %w", err)
	}
	lf, err := lockfile.Parse(lfBytes)
	if err != nil {
		return nil, fmt.Errorf("parse lockfile: %w", err)
	}
	resolved, err := lf.Resolve(p.store)
	if err != nil {
		return nil, fmt.Errorf("resolve lockfile: %w", err)
	}
	if err := materialize.Materialize(orgDir, resolved); err != nil {
		return nil, fmt.Errorf("materialize: %w", err)
	}

	rec := existing
	if rec == nil {
		rec = core.NewRecord(col)
	}
	rec.Set("slug", slug)
	rec.Set("display_name", displayName)
	rec.Set("status", "provisioning")
	rec.Set("data_dir", filepath.Join("pb_orgs", slug))
	rec.Set("lockfile", string(lfBytes))
	if err := p.app.Save(rec); err != nil {
		return nil, fmt.Errorf("save org record: %w", err)
	}

	// Activate BEFORE verifying: the manager's load path refuses a non-active
	// org, and verification IS a load. The org's migrations run inside the
	// confined tenant process (apis.Serve runs RunAllMigrations before the
	// readiness report), so the control plane never executes tenant JS. On
	// failure the child's reason arrives through the readiness pipe and the
	// record rolls back to "provisioning", which a retried CreateOrg resumes.
	// The brief active-but-unverified window is benign — a request racing it
	// collapses into the same singleflight spawn the verification drives.
	rec.Set("status", "active")
	if err := p.app.Save(rec); err != nil {
		return nil, fmt.Errorf("activate org record: %w", err)
	}

	if p.verify != nil {
		ctx, cancel := context.WithTimeout(context.Background(), tenantVerifyTimeout)
		defer cancel()
		if err := p.verify(ctx, slug); err != nil {
			rec.Set("status", "provisioning")
			if saveErr := p.app.Save(rec); saveErr != nil {
				return nil, fmt.Errorf("tenant bootstrap: %w (and rollback to provisioning failed: %v)", err, saveErr)
			}
			return nil, fmt.Errorf("tenant bootstrap: %w", err)
		}
	}
	return rec, nil
}

// tenantVerifyTimeout bounds the provision-time boot verification. It only
// caps how long CreateOrg waits: the manager bounds the spawn itself with its
// own (shorter) readiness timeout, so this fires only if the manager path
// itself wedges.
const tenantVerifyTimeout = 60 * time.Second

// Deploy re-resolves a new lockfile, re-materializes, records a deployment, and
// evicts the running instance so the next request loads fresh.
func (p *Provisioner) Deploy(slug string, lock map[string]string) error {
	rec, err := p.app.FindFirstRecordByData("orgs", "slug", slug)
	if err != nil || rec == nil {
		return fmt.Errorf("org %q not found", slug)
	}
	if s := rec.GetString("status"); s != "active" {
		return fmt.Errorf("cannot deploy to org %q in status %q", slug, s)
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
	dcol, err := p.app.FindCollectionByNameOrId("deployments")
	if err != nil {
		return fmt.Errorf("find deployments collection: %w", err)
	}
	d := core.NewRecord(dcol)
	d.Set("org", rec.Id)
	d.Set("lockfile", string(lfBytes))
	if err := p.app.Save(d); err != nil {
		return fmt.Errorf("record deployment: %w", err)
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
	files, err := transpileForStore(files)
	if err != nil {
		return fmt.Errorf("transpile package %s@%s: %w", name, version, err)
	}
	// Emit a parsed manifest.json so the host can read the package's host-side
	// capability config (carddav/fts/audit) without a TS loader at load time.
	files, err = emitManifestJSON(files)
	if err != nil {
		return fmt.Errorf("manifest json for %s@%s: %w", name, version, err)
	}
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

// RegisterRoutes binds the provisioning API onto the control-plane app's OnServe.
// All routes require a superuser (control-plane admin) auth.
func (p *Provisioner) RegisterRoutes() {
	p.app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		g := e.Router.Group("/api")

		g.POST("/orgs", func(re *core.RequestEvent) error {
			var body struct {
				Slug        string            `json:"slug"`
				DisplayName string            `json:"display_name"`
				Lockfile    map[string]string `json:"lockfile"`
			}
			if err := re.BindBody(&body); err != nil {
				return re.BadRequestError("invalid body", err)
			}
			rec, err := p.CreateOrg(body.Slug, body.DisplayName, body.Lockfile)
			if err != nil {
				return re.BadRequestError(err.Error(), err)
			}
			return re.JSON(200, map[string]any{"slug": rec.GetString("slug"), "status": rec.GetString("status")})
		}).Bind(apis.RequireSuperuserAuth())

		g.POST("/orgs/{slug}/deploy", func(re *core.RequestEvent) error {
			var body struct {
				Lockfile map[string]string `json:"lockfile"`
			}
			if err := re.BindBody(&body); err != nil {
				return re.BadRequestError("invalid body", err)
			}
			if err := p.Deploy(re.Request.PathValue("slug"), body.Lockfile); err != nil {
				return re.BadRequestError(err.Error(), err)
			}
			return re.NoContent(204)
		}).Bind(apis.RequireSuperuserAuth())

		g.POST("/orgs/{slug}/suspend", func(re *core.RequestEvent) error {
			if err := p.Suspend(re.Request.PathValue("slug")); err != nil {
				return re.BadRequestError(err.Error(), err)
			}
			return re.NoContent(204)
		}).Bind(apis.RequireSuperuserAuth())

		g.POST("/orgs/{slug}/resume", func(re *core.RequestEvent) error {
			if err := p.Resume(re.Request.PathValue("slug")); err != nil {
				return re.BadRequestError(err.Error(), err)
			}
			return re.NoContent(204)
		}).Bind(apis.RequireSuperuserAuth())

		g.DELETE("/orgs/{slug}", func(re *core.RequestEvent) error {
			if err := p.Archive(re.Request.PathValue("slug")); err != nil {
				return re.BadRequestError(err.Error(), err)
			}
			return re.NoContent(204)
		}).Bind(apis.RequireSuperuserAuth())

		g.POST("/store/packages", func(re *core.RequestEvent) error {
			var body struct {
				Name    string            `json:"name"`
				Version string            `json:"version"`
				Kind    string            `json:"kind"`
				Files   map[string]string `json:"files"` // path -> base64 content
			}
			if err := re.BindBody(&body); err != nil {
				return re.BadRequestError("invalid body", err)
			}
			files := make(map[string][]byte, len(body.Files))
			for path, b64 := range body.Files {
				raw, err := base64.StdEncoding.DecodeString(b64)
				if err != nil {
					return re.BadRequestError("invalid base64 for "+path, err)
				}
				files[path] = raw
			}
			if err := p.PublishPackage(body.Name, body.Version, files, body.Kind); err != nil {
				return re.BadRequestError(err.Error(), err)
			}
			return re.JSON(200, map[string]any{"name": body.Name, "version": body.Version})
		}).Bind(apis.RequireSuperuserAuth())

		return e.Next()
	})
}
