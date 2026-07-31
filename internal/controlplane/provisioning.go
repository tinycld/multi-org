package controlplane

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"

	"tinycld.org/core/pkgbuild"
	"tinycld.org/multi-org/internal/builder"
	"tinycld.org/multi-org/internal/lockfile"
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
// deployments collections.
type Provisioner struct {
	app    core.App
	root   string
	evict  EvictFunc
	verify VerifyFunc

	// deployer executes artifact-backed provisioning and the D6 deploy
	// protocol. Nil until EnableBuilds — a router without a builder can serve
	// existing orgs but refuses to provision or deploy.
	deployer *Deployer
}

// NewProvisioner builds a Provisioner. verify may be nil, in which case
// CreateOrg activates the org without booting it and the org's migrations run
// at its first spawn instead — that trades the synchronous failure report for
// a 502 on first request, so production wiring should always pass one.
func NewProvisioner(app core.App, root string, evict EvictFunc, verify VerifyFunc) *Provisioner {
	return &Provisioner{app: app, root: root, evict: evict, verify: verify}
}

// EnableBuilds attaches the trusted builder: CreateOrg and Deploy gain the
// artifact path for base-bearing lockfiles, and ControlHandler starts serving
// the per-org deploy protocol.
func (p *Provisioner) EnableBuilds(b *builder.Builder, log *slog.Logger) {
	p.deployer = newDeployer(p.app, p.root, b, p.evict, p.verify, log)
}

// Deployer exposes the deploy orchestrator (nil before EnableBuilds) — the
// GC sweep reads its liveness policy.
func (p *Provisioner) Deployer() *Deployer { return p.deployer }

// ControlHandler is the orgmanager.Config.Control hook: the per-org control
// socket surface. Nil when builds are not enabled, which keeps the manager
// from binding control sockets that could accept nothing.
func (p *Provisioner) ControlHandler() func(slug string) http.Handler {
	if p.deployer == nil {
		return nil
	}
	return p.deployer.Handler
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
// resumes: re-builds the set (a cache hit), re-verifies the tenant boot, and
// flips to active.
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

	// D7: no explicit lockfile ⇒ copy the control-plane template. The org
	// owns the copy from this moment — later template edits affect new orgs
	// only. An explicit empty map remains "zero packages".
	if lock == nil {
		var err error
		lock, err = DefaultLockfile(p.app)
		if err != nil {
			return nil, fmt.Errorf("default lockfile: %w", err)
		}
	}

	// Every org boots from a committed build artifact, so the set must be
	// buildable: it names the app shell (the builder refuses a set without
	// it), and a builder must be configured. The org's live trees come from
	// the artifact at load — nothing is materialized here. A base-bearing
	// default set is a cache hit, so provisioning costs seconds (D4).
	if !IsArtifactSet(lock) {
		return nil, fmt.Errorf("lockfile must include the app shell (%q): every org boots from a built artifact", pkgbuild.BaseMemberSlug)
	}
	if p.deployer == nil {
		return nil, fmt.Errorf("no builder is configured (MT_SCAFFOLD_ROOT); cannot provision orgs")
	}

	orgDir := filepath.Join(p.root, "pb_orgs", slug)
	if err := os.MkdirAll(filepath.Join(orgDir, "pb_data"), 0o755); err != nil {
		return nil, fmt.Errorf("create org dir: %w", err)
	}

	lfBytes, err := lockfile.OrgLockfile(lock).Marshal()
	if err != nil {
		return nil, fmt.Errorf("marshal lockfile: %w", err)
	}

	res, err := p.deployer.BuildSet(context.Background(), lock)
	if err != nil {
		return nil, fmt.Errorf("build package set: %w", err)
	}
	recipeHash := res.RecipeHash
	p.deployer.trackHash(recipeHash)
	defer p.deployer.untrackHash(recipeHash)

	rec := existing
	if rec == nil {
		rec = core.NewRecord(col)
	}
	rec.Set("slug", slug)
	rec.Set("display_name", displayName)
	rec.Set("status", "provisioning")
	rec.Set("data_dir", filepath.Join("pb_orgs", slug))
	rec.Set("lockfile", string(lfBytes))
	rec.Set("recipe_hash", recipeHash)
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
			slug := re.Request.PathValue("slug")
			// Every deploy runs through the D6 orchestrator: build →
			// repoint → respawn with commit/revert.
			if !IsArtifactSet(body.Lockfile) {
				err := fmt.Errorf("lockfile must include the app shell (%q): every org boots from a built artifact", pkgbuild.BaseMemberSlug)
				return re.BadRequestError(err.Error(), err)
			}
			if p.deployer == nil {
				err := fmt.Errorf("no builder is configured (MT_SCAFFOLD_ROOT); cannot deploy")
				return re.BadRequestError(err.Error(), err)
			}
			recipeHash, err := p.deployer.Deploy(context.Background(), slug, body.Lockfile, "")
			if err != nil {
				return re.BadRequestError(err.Error(), err)
			}
			return re.JSON(202, map[string]any{"recipeHash": recipeHash})
		}).Bind(apis.RequireSuperuserAuth())

		// The D7 template: the package set new orgs copy when POST /api/orgs
		// carries no lockfile. Operator-owned, superuser-only, affects NEW
		// orgs only.
		g.GET("/settings/default-lockfile", func(re *core.RequestEvent) error {
			lock, err := DefaultLockfile(p.app)
			if err != nil {
				return re.InternalServerError(err.Error(), err)
			}
			if lock == nil {
				lock = map[string]string{}
			}
			return re.JSON(200, map[string]any{"lockfile": lock})
		}).Bind(apis.RequireSuperuserAuth())

		g.PUT("/settings/default-lockfile", func(re *core.RequestEvent) error {
			var body struct {
				Lockfile map[string]string `json:"lockfile"`
			}
			if err := re.BindBody(&body); err != nil {
				return re.BadRequestError("invalid body", err)
			}
			// Validate the entries now — a template that cannot build should
			// fail the operator saving it, not the next org's provisioning.
			if len(body.Lockfile) > 0 {
				if _, err := refsFor(body.Lockfile); err != nil {
					return re.BadRequestError(err.Error(), err)
				}
			}
			if err := SetDefaultLockfile(p.app, body.Lockfile); err != nil {
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

		return e.Next()
	})
}
