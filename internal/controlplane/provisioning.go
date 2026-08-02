package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
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

	// createOwnerFn mints the org's operator. Nil means the real path: run
	// `create-owner` on the artifact's own binary. Tests substitute it —
	// unit tests have no compiled artifact to execute, and the behaviour
	// worth asserting there is the ORCHESTRATION (that it runs after
	// verification, that a failure is reported), not exec plumbing.
	createOwnerFn func(slug, orgDir, artifactDir, email, password string) error
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

// OwnerAccount is the org's first operator, minted during provisioning.
//
// Email is REQUIRED: a hosted org has no other way to get an account. The
// /setup wizard that bootstraps a single-tenant deployment is bound in the
// HOST composition only, so a tenant never serves it — an org provisioned
// without an owner boots, serves, and has zero users, leaving nobody able to
// log in. Requiring it makes that state unreachable rather than a trap.
//
// Password is optional: supply one to pre-fill a known secret, or leave it
// empty and provisioning generates a random one and returns it (the only time
// it is ever visible — it is stored hashed).
//
// Both identities the setup wizard creates are minted from these credentials:
// the PocketBase `_superusers` record behind /_/, and the `users` record with
// role=owner plus its `super_admins` grant that the APP authenticates against.
type OwnerAccount struct {
	Email    string
	Password string
}

// CreateOrg provisions a new org, or resumes a previously half-provisioned one.
// If an org row for the slug already exists and is still active, it errors; if
// the existing row is in a non-active (e.g. stranded "provisioning") state, it
// resumes: re-builds the set (a cache hit), re-verifies the tenant boot, and
// flips to active.
//
// The org's owner account is created before the org is returned, so a
// provisioned org is immediately usable. The returned password is the one that
// was set — the caller's, or the generated one when owner.Password was empty.
// It is the only time the password is visible; it is stored hashed.
func (p *Provisioner) CreateOrg(slug, displayName string, lock map[string]string, owner OwnerAccount) (*core.Record, string, error) {
	if !validSlug(slug) {
		return nil, "", fmt.Errorf("invalid slug %q", slug)
	}
	if strings.TrimSpace(owner.Email) == "" {
		return nil, "", fmt.Errorf("owner email is required: an org without one has no account that can log in")
	}
	ownerPassword := owner.Password
	if ownerPassword == "" {
		generated, err := generateOwnerPassword()
		if err != nil {
			return nil, "", fmt.Errorf("generate owner password: %w", err)
		}
		ownerPassword = generated
	}
	col, err := p.app.FindCollectionByNameOrId("orgs")
	if err != nil {
		return nil, "", fmt.Errorf("find orgs collection: %w", err)
	}

	existing, _ := p.app.FindFirstRecordByData("orgs", "slug", slug)
	if existing != nil && existing.GetString("status") == "active" {
		return nil, "", fmt.Errorf("org %q already exists", slug)
	}

	// D7: no explicit lockfile ⇒ copy the control-plane template. The org
	// owns the copy from this moment — later template edits affect new orgs
	// only. An explicit empty map remains "zero packages".
	if lock == nil {
		var err error
		lock, err = DefaultLockfile(p.app)
		if err != nil {
			return nil, "", fmt.Errorf("default lockfile: %w", err)
		}
	}

	// Every org boots from a committed build artifact, so the set must be
	// buildable: it names the app shell (the builder refuses a set without
	// it), and a builder must be configured. The org's live trees come from
	// the artifact at load — nothing is materialized here. A base-bearing
	// default set is a cache hit, so provisioning costs seconds (D4).
	if !IsArtifactSet(lock) {
		return nil, "", fmt.Errorf("lockfile must include the app shell (%q): every org boots from a built artifact", pkgbuild.BaseMemberSlug)
	}
	if p.deployer == nil {
		return nil, "", fmt.Errorf("no builder is configured (MT_SCAFFOLD_ROOT); cannot provision orgs")
	}

	orgDir := filepath.Join(p.root, "pb_orgs", slug)
	if err := os.MkdirAll(filepath.Join(orgDir, "pb_data"), 0o755); err != nil {
		return nil, "", fmt.Errorf("create org dir: %w", err)
	}

	lfBytes, err := lockfile.OrgLockfile(lock).Marshal()
	if err != nil {
		return nil, "", fmt.Errorf("marshal lockfile: %w", err)
	}

	res, err := p.deployer.BuildSet(context.Background(), lock)
	if err != nil {
		return nil, "", fmt.Errorf("build package set: %w", err)
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
		return nil, "", fmt.Errorf("save org record: %w", err)
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
		return nil, "", fmt.Errorf("activate org record: %w", err)
	}

	if p.verify != nil {
		ctx, cancel := context.WithTimeout(context.Background(), tenantVerifyTimeout)
		defer cancel()
		if err := p.verify(ctx, slug); err != nil {
			rec.Set("status", "provisioning")
			if saveErr := p.app.Save(rec); saveErr != nil {
				return nil, "", fmt.Errorf("tenant bootstrap: %w (and rollback to provisioning failed: %v)", err, saveErr)
			}
			return nil, "", fmt.Errorf("tenant bootstrap: %w", err)
		}
	}

	// Owner account LAST: it must run after verification, because verification
	// is the boot that runs the org's migrations — `users` and `super_admins`
	// do not exist before it. A failure here leaves the org active and serving
	// (it is a real, working org) but with no way in, so it is reported rather
	// than swallowed; a retried CreateOrg re-runs the step idempotently.
	createOwner := p.createOwnerFn
	if createOwner == nil {
		createOwner = p.createOwner
	}
	if err := createOwner(slug, orgDir, res.Dir, owner.Email, ownerPassword); err != nil {
		return nil, "", fmt.Errorf("org %q provisioned but owner account failed: %w", slug, err)
	}
	return rec, ownerPassword, nil
}

// createOwner mints the org's first app account by running the artifact's own
// binary against the org's pb_data.
//
// It runs the binary rather than opening the DB here for two reasons. The
// schema belongs to the tenant's package set, not the router — only the
// artifact knows how to bootstrap its own app. And the router deliberately
// never links tenant code: executing the artifact keeps that boundary, exactly
// as the deploy path does.
//
// The tenant is evicted first: SQLite is held open by the running process, and
// a second writer risks corruption. The next request respawns it.
func (p *Provisioner) createOwner(slug, orgDir, artifactDir, email, password string) error {
	binary := filepath.Join(artifactDir, ownerBinaryName)
	if _, err := os.Stat(binary); err != nil {
		return fmt.Errorf("artifact binary not found at %s: %w", binary, err)
	}

	if p.evict != nil {
		p.evict(slug)
		// Give the evicted tenant a moment to finish its drain and release
		// the database before a second process opens it for writing.
		time.Sleep(evictDrainGrace)
	}

	ctx, cancel := context.WithTimeout(context.Background(), ownerCreateTimeout)
	defer cancel()

	// The password rides in a flag rather than positionally so it is never
	// mistaken for the email; both are visible in the process table either way,
	// which is acceptable for a root-only host but worth knowing.
	cmd := exec.CommandContext(ctx, binary, "create-owner", email,
		"--password", password,
		"--dir", filepath.Join(orgDir, "pb_data"))
	cmd.Dir = orgDir
	// Scrubbed environment, same posture as a build job: the artifact is
	// tenant-supplied code and has no business reading the router's env
	// (MT_SUPERUSER_PASSWORD, TLS key paths, the Cloudflare token).
	cmd.Env = []string{"HOME=" + orgDir, "PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("create-owner: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// Exit status alone does NOT prove the account exists. PocketBase's root
	// command exits 0 on an unknown subcommand — an artifact built before
	// create-owner existed prints `unknown command "create-owner"` and returns
	// success, which would silently hand the operator a password for an org
	// with no accounts. Require the command's own confirmation line instead of
	// trusting the status.
	text := string(out)
	if !strings.Contains(text, "owner: ") {
		return fmt.Errorf("create-owner did not confirm the account (artifact may predate the command): %s",
			strings.TrimSpace(firstLine(text)))
	}
	return nil
}

// firstLine returns s's first non-empty line, for quoting a subprocess's
// complaint into an error without dragging in a whole help dump.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return "(no output)"
}

// ownerBinaryName is the server binary's name inside an artifact. It matches
// builder.Config.BinaryName's default; a deployment that renames one must
// rename both.
const ownerBinaryName = "tinycld"

// generateOwnerPassword returns a URL-safe random password for a provisioned
// org's operator: 24 random bytes, base64url-encoded to 32 characters.
//
// Deliberately duplicated rather than imported from coreserver: that package
// pulls the whole app framework (Sentry, Postmark, webpush, websockets) into
// the router's dependency graph, and the router links no tenant code by
// design. Twenty lines of crypto/rand is the cheaper side of that trade.
func generateOwnerPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// evictDrainGrace is how long to wait after evicting a tenant before opening
// its database from another process. The manager's own drain is bounded; this
// is the small margin on top of it.
const evictDrainGrace = 3 * time.Second

// ownerCreateTimeout bounds the create-owner subprocess. It opens the DB and
// writes two records — seconds of work — so this only fires if it wedges.
const ownerCreateTimeout = 60 * time.Second

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
				// The org's first operator. Email is REQUIRED — a tenant
				// serves no setup wizard, so an org without one has no
				// account that can log in. Password is optional: omit it and
				// a random one is generated and returned.
				OwnerEmail    string `json:"owner_email"`
				OwnerPassword string `json:"owner_password"`
			}
			if err := re.BindBody(&body); err != nil {
				return re.BadRequestError("invalid body", err)
			}
			rec, password, err := p.CreateOrg(body.Slug, body.DisplayName, body.Lockfile,
				OwnerAccount{Email: body.OwnerEmail, Password: body.OwnerPassword})
			if err != nil {
				return re.BadRequestError(err.Error(), err)
			}
			resp := map[string]any{
				"slug":        rec.GetString("slug"),
				"status":      rec.GetString("status"),
				"owner_email": body.OwnerEmail,
			}
			// Return the password ONLY when we generated it: echoing one the
			// caller already sent back over the wire buys nothing and widens
			// where it can be logged.
			if body.OwnerPassword == "" {
				resp["owner_password"] = password
			}
			return re.JSON(200, resp)
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

		// Mail domains: the MX routing registry, keyed by slug rather than the
		// orgs relation id an operator would otherwise have to look up first.
		// Superuser-only like the rest — an org must not be able to claim its
		// own domains, since the unique index means a claim denies it to every
		// other org.
		g.GET("/orgs/{slug}/mail-domains", func(re *core.RequestEvent) error {
			domains, err := p.ListMailDomains(re.Request.PathValue("slug"))
			if err != nil {
				return re.NotFoundError(err.Error(), err)
			}
			return re.JSON(200, map[string]any{"domains": domains})
		}).Bind(apis.RequireSuperuserAuth())

		g.POST("/orgs/{slug}/mail-domains", func(re *core.RequestEvent) error {
			var body struct {
				Domain string `json:"domain"`
			}
			if err := re.BindBody(&body); err != nil {
				return re.BadRequestError("invalid body", err)
			}
			rec, err := p.AddMailDomain(re.Request.PathValue("slug"), body.Domain)
			if err != nil {
				return re.BadRequestError(err.Error(), err)
			}
			return re.JSON(201, map[string]any{"domain": rec.GetString("domain")})
		}).Bind(apis.RequireSuperuserAuth())

		g.DELETE("/orgs/{slug}/mail-domains/{domain}", func(re *core.RequestEvent) error {
			err := p.RemoveMailDomain(re.Request.PathValue("slug"), re.Request.PathValue("domain"))
			if err != nil {
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
