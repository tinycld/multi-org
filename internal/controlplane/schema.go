package controlplane

import (
	"github.com/pocketbase/pocketbase/core"
)

// registerSchema appends the control-plane collections to the process-global app
// migrations. Called once from New before Bootstrap. The filename prefix is
// deliberately well above PocketBase's own base migration timestamps (16xx) so
// filename ordering reflects the real ordering: PB's base tables must exist first.
func registerSchema() {
	core.AppMigrations.Register(func(txApp core.App) error {
		orgs, err := createOrgs(txApp)
		if err != nil {
			return err
		}
		if err := createPackages(txApp); err != nil {
			return err
		}
		return createDeployments(txApp, orgs)
	}, func(txApp core.App) error {
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
	}, "1900000000_controlplane_init.go")
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
