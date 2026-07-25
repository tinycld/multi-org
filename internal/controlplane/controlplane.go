package controlplane

import (
	"github.com/pocketbase/pocketbase"
)

// ControlPlane wraps the control-plane PocketBase app: the authoritative registry
// of orgs, packages, and deployments. It holds no tenant/user data.
type ControlPlane struct {
	App *pocketbase.PocketBase
}

// New builds (but does not bootstrap) the control-plane app rooted at dataDir.
//
// The control-plane schema is NOT registered into the process-global
// core.AppMigrations (that would run it in every tenant app too — the leak this
// design avoids). Instead the caller applies it explicitly via Init (or RunSchema)
// after Bootstrap, so only the control-plane app gets orgs/packages/deployments.
func New(dataDir string) (*ControlPlane, error) {
	pb := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir:  dataDir,
		HideStartBanner: true,
	})
	return &ControlPlane{App: pb}, nil
}

// Init bootstraps the control-plane app, runs PocketBase's system migrations, and
// applies the control-plane schema (app-scoped, not global). Use it instead of
// hand-sequencing Bootstrap + RunAllMigrations + RunSchema. Safe to call once per
// process on a fresh or existing data dir.
func (cp *ControlPlane) Init() error {
	if err := cp.App.Bootstrap(); err != nil {
		return err
	}
	if err := cp.App.RunSystemMigrations(); err != nil {
		return err
	}
	return RunSchema(cp.App)
}
