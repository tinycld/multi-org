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
	return list
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
	c.Fields.Add(&core.JSONField{Name: "custom_domains"})
	c.Fields.Add(&core.JSONField{Name: "lockfile"})
	c.Fields.Add(&core.TextField{Name: "display_name"})
	c.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	c.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
	c.AddIndex("idx_orgs_slug", true, "slug", "")
	if err := app.Save(c); err != nil {
		return nil, err
	}
	return c, nil
}

func createPackages(app core.App) error {
	c := core.NewBaseCollection("packages")
	c.Fields.Add(&core.TextField{Name: "name", Required: true})
	c.Fields.Add(&core.TextField{Name: "version", Required: true})
	c.Fields.Add(&core.TextField{Name: "store_path"})
	c.Fields.Add(&core.TextField{Name: "content_hash"})
	c.Fields.Add(&core.JSONField{Name: "manifest"})
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
