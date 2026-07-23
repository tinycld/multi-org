package controlplane

import (
	"github.com/pocketbase/pocketbase/core"
)

// registerSchema appends the control-plane collections to the process-global app
// migrations. Called once from New before Bootstrap.
func registerSchema() {
	core.AppMigrations.Register(func(txApp core.App) error {
		if err := createOrgs(txApp); err != nil {
			return err
		}
		if err := createPackages(txApp); err != nil {
			return err
		}
		return createDeployments(txApp)
	}, nil, "1000000000_controlplane_init.go")
}

func createOrgs(app core.App) error {
	c := core.NewBaseCollection("orgs")
	c.Fields.Add(&core.TextField{Name: "slug", Required: true})
	c.Fields.Add(&core.SelectField{Name: "status", Values: []string{"provisioning", "active", "suspended", "archived"}, MaxSelect: 1})
	c.Fields.Add(&core.TextField{Name: "data_dir"})
	c.Fields.Add(&core.JSONField{Name: "custom_domains"})
	c.Fields.Add(&core.JSONField{Name: "lockfile"})
	c.Fields.Add(&core.TextField{Name: "display_name"})
	c.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	c.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
	c.AddIndex("idx_orgs_slug", true, "slug", "")
	return app.Save(c)
}

func createPackages(app core.App) error {
	c := core.NewBaseCollection("packages")
	c.Fields.Add(&core.TextField{Name: "name", Required: true})
	c.Fields.Add(&core.TextField{Name: "version", Required: true})
	c.Fields.Add(&core.TextField{Name: "store_path"})
	c.Fields.Add(&core.TextField{Name: "content_hash"})
	c.Fields.Add(&core.JSONField{Name: "manifest"})
	c.Fields.Add(&core.SelectField{Name: "kind", Values: []string{"official", "fork", "custom"}, MaxSelect: 1})
	c.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	c.AddIndex("idx_packages_name_version", true, "name, version", "")
	return app.Save(c)
}

func createDeployments(app core.App) error {
	c := core.NewBaseCollection("deployments")
	c.Fields.Add(&core.TextField{Name: "org", Required: true})
	c.Fields.Add(&core.JSONField{Name: "lockfile"})
	c.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	return app.Save(c)
}
