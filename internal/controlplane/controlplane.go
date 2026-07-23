package controlplane

import (
	"sync"

	"github.com/pocketbase/pocketbase"
)

// registerOnce guards registration into core.AppMigrations, which is a
// PROCESS-GLOBAL registry deliberately shared across all PocketBase apps in the
// process. sync.Once makes registration idempotent so multiple ControlPlane
// instances (or repeated New calls) don't double-register the schema migration.
var registerOnce sync.Once

// ControlPlane wraps the control-plane PocketBase app: the authoritative registry
// of orgs, packages, and deployments. It holds no tenant/user data.
type ControlPlane struct {
	App *pocketbase.PocketBase
}

// New builds (but does not bootstrap) the control-plane app rooted at dataDir.
// The schema migration is registered once into the process-global AppMigrations.
func New(dataDir string) (*ControlPlane, error) {
	registerOnce.Do(registerSchema)
	pb := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir:  dataDir,
		HideStartBanner: true,
	})
	return &ControlPlane{App: pb}, nil
}
