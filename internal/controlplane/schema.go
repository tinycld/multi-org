package controlplane

import (
	"github.com/pocketbase/pocketbase/core"
)

// controlPlaneSchemaFile is the migration filename recorded in the control-plane's
// _migrations table. The prefix is well above PocketBase's base migration
// timestamps (16xx) so ordering reflects reality: PB's base tables exist first.
const controlPlaneSchemaFile = "1900000000_controlplane_init.go"

// controlPlaneMigrations returns the control-plane schema as an APP-SCOPED
// migration list — NOT the process-global core.AppMigrations. This is
// deliberate: core.AppMigrations is shared by every PocketBase app in the process
// (including every tenant), so registering the control-plane schema there leaked
// orgs/packages/deployments into every tenant DB. Running this list only against
// the control-plane app (RunSchema) keeps the registry out of tenants.
func controlPlaneMigrations() core.MigrationsList {
	var list core.MigrationsList
	list.Add(&core.Migration{
		File: controlPlaneSchemaFile,
		Up: func(txApp core.App) error {
			orgs, err := createOrgs(txApp)
			if err != nil {
				return err
			}
			if err := createPackages(txApp); err != nil {
				return err
			}
			return createDeployments(txApp, orgs)
		},
		Down: func(txApp core.App) error {
			// reverse order (deployments references orgs via relation)
			for _, name := range []string{"deployments", "packages", "orgs"} {
				c, err := txApp.FindCollectionByNameOrId(name)
				if err != nil {
					continue // already gone
				}
				if err := txApp.Delete(c); err != nil {
					return err
				}
			}
			return nil
		},
	})
	// A separate migration (not folded into the init one) so control planes
	// provisioned before it gain the collection on their next boot.
	list.Add(&core.Migration{
		File: "1900000001_org_mail_domains.go",
		Up:   createOrgMailDomains,
		Down: func(txApp core.App) error {
			c, err := txApp.FindCollectionByNameOrId("org_mail_domains")
			if err != nil {
				return nil // already gone
			}
			return txApp.Delete(c)
		},
	})
	// Build-artifact backing (design §7 step 3): orgs point at a committed
	// recipe, deployments carry outcome, and the operator-editable default
	// package set (D7) gets a home. Separate migration for the same
	// upgrade-in-place reason as above.
	list.Add(&core.Migration{
		File: "1900000002_org_builds.go",
		Up:   addBuildFields,
		Down: func(txApp core.App) error {
			if c, err := txApp.FindCollectionByNameOrId("control_settings"); err == nil {
				if err := txApp.Delete(c); err != nil {
					return err
				}
			}
			if orgs, err := txApp.FindCollectionByNameOrId("orgs"); err == nil {
				orgs.Fields.RemoveByName("recipe_hash")
				orgs.Fields.RemoveByName("prev_recipe_hash")
				if err := txApp.Save(orgs); err != nil {
					return err
				}
			}
			if deps, err := txApp.FindCollectionByNameOrId("deployments"); err == nil {
				for _, name := range []string{"recipe_hash", "status", "error", "job_id"} {
					deps.Fields.RemoveByName(name)
				}
				if err := txApp.Save(deps); err != nil {
					return err
				}
			}
			return nil
		},
	})
	return list
}

// addBuildFields extends orgs and deployments for artifact-backed deploys and
// creates the control_settings singleton.
//
//   - orgs.recipe_hash: the committed build the org boots from ("" = legacy
//     store-materialized org). orgs.prev_recipe_hash: the revert target — kept
//     after a successful deploy too, because GC liveness counts it (design D4:
//     "plus each org's previous build").
//   - deployments gains recipe_hash, a status lifecycle
//     (proposed → committed | reverted, failed for builds that never repointed;
//     "" on legacy rows means committed), the failure reason, and the
//     tenant-minted job_id that ties an outcome back to the pkg_install_log
//     row the org's Packages UI polls.
func addBuildFields(txApp core.App) error {
	orgs, err := txApp.FindCollectionByNameOrId("orgs")
	if err != nil {
		return err
	}
	orgs.Fields.Add(&core.TextField{Name: "recipe_hash"})
	orgs.Fields.Add(&core.TextField{Name: "prev_recipe_hash"})
	if err := txApp.Save(orgs); err != nil {
		return err
	}

	deps, err := txApp.FindCollectionByNameOrId("deployments")
	if err != nil {
		return err
	}
	deps.Fields.Add(&core.TextField{Name: "recipe_hash"})
	deps.Fields.Add(&core.SelectField{Name: "status", Values: []string{"proposed", "committed", "reverted", "failed"}, MaxSelect: 1})
	deps.Fields.Add(&core.TextField{Name: "error"})
	deps.Fields.Add(&core.TextField{Name: "job_id"})
	if err := txApp.Save(deps); err != nil {
		return err
	}

	return createControlSettings(txApp)
}

// createControlSettings is the control plane's one-row settings collection.
// All rules nil (superuser-only): the default set is the operator's, same
// trust altitude as the storage ceiling. The single row is created lazily by
// settingsRecord — a fresh collection with zero rows means zero defaults.
func createControlSettings(app core.App) error {
	c := core.NewBaseCollection("control_settings")
	// default_lockfile is the package set CreateOrg copies when the caller
	// passes none (design D7). Editing it affects NEW orgs only — an org owns
	// its lockfile from the moment it is copied.
	c.Fields.Add(&core.JSONField{Name: "default_lockfile"})
	c.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
	return app.Save(c)
}

// RunSchema applies the control-plane schema against a single app, recording it
// in that app's _migrations table (idempotent — a re-run is a no-op). Call it
// only on the control-plane app, after Bootstrap and RunSystemMigrations.
func RunSchema(app core.App) error {
	_, err := core.NewMigrationsRunner(app, controlPlaneMigrations()).Up()
	return err
}

func createOrgs(app core.App) (*core.Collection, error) {
	c := core.NewBaseCollection("orgs")
	c.Fields.Add(&core.TextField{Name: "slug", Required: true})
	c.Fields.Add(&core.SelectField{Name: "status", Values: []string{"provisioning", "active", "suspended", "archived"}, MaxSelect: 1, Required: true})
	c.Fields.Add(&core.TextField{Name: "data_dir", Required: true})
	c.Fields.Add(&core.JSONField{Name: "lockfile"})
	c.Fields.Add(&core.TextField{Name: "display_name"})
	// The org's storage ceiling in bytes (0 = unlimited). This is the operator's
	// commercial decision — the plan the org was sold — so it lives HERE and not
	// in the tenant's own `settings`, where that org's superusers could raise it.
	// The manager materializes it into the org's runtime config at spawn.
	c.Fields.Add(&core.NumberField{Name: "storage_limit_bytes"})
	c.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	c.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
	c.AddIndex("idx_orgs_slug", true, "slug", "")
	if err := app.Save(c); err != nil {
		return nil, err
	}
	return c, nil
}

// createOrgMailDomains is the MX routing registry: which org owns a mail
// domain. The router's :25 frontend routes every RCPT TO through it, so
//   - `domain` is unique ACROSS orgs — otherwise a hostile org could claim a
//     sibling's domain and receive its mail;
//   - the stored value must be a canonical lowercase FQDN (the pattern
//     rejects uppercase rather than normalizing, so every write path stores
//     exactly what the relay's case-folded lookup compares against);
//   - all rules stay nil (superuser-only): the operator assigns domains, the
//     same trust altitude as the storage ceiling on orgs.
func createOrgMailDomains(app core.App) error {
	orgs, err := app.FindCollectionByNameOrId("orgs")
	if err != nil {
		return err
	}
	c := core.NewBaseCollection("org_mail_domains")
	c.Fields.Add(&core.RelationField{Name: "org", CollectionId: orgs.Id, MaxSelect: 1, Required: true, CascadeDelete: true})
	c.Fields.Add(&core.TextField{
		Name:     "domain",
		Required: true,
		Pattern:  `^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`,
	})
	c.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	c.AddIndex("idx_org_mail_domains_domain", true, "domain", "")
	return app.Save(c)
}

func createPackages(app core.App) error {
	c := core.NewBaseCollection("packages")
	c.Fields.Add(&core.TextField{Name: "name", Required: true})
	c.Fields.Add(&core.TextField{Name: "version", Required: true})
	c.Fields.Add(&core.TextField{Name: "store_path"})
	c.Fields.Add(&core.SelectField{Name: "kind", Values: []string{"official", "fork", "custom"}, MaxSelect: 1, Required: true})
	c.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	c.AddIndex("idx_packages_name_version", true, "name, version", "")
	return app.Save(c)
}

func createDeployments(app core.App, orgs *core.Collection) error {
	c := core.NewBaseCollection("deployments")
	c.Fields.Add(&core.RelationField{Name: "org", CollectionId: orgs.Id, MaxSelect: 1, Required: true, CascadeDelete: true})
	c.Fields.Add(&core.JSONField{Name: "lockfile"})
	c.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	return app.Save(c)
}
