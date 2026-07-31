// Command serve-org runs ONE organization's PocketBase app in its own OS
// process, serving on a unix socket handed down by the multi-org router.
//
// All tenant transport — config loading, the unix socket, the readiness
// handshake, confinement, shutdown — lives in core's tenantmain package (the
// same entry point a per-org build's dual-mode binary uses). What remains
// here is exactly this binary's feature composition:
//
// FEATURE Go links here through the pinned menu (internal/tenantpkgs,
// docs/SCOPE-tenant-feature-go.md closed as option b): each feature's
// RegisterTenant runs when the org's resolved package set — materialized to
// .runtime/packages.json — includes its slug. A feature's request-hook
// enforcement therefore runs in a tenant. Packages outside the menu are
// TS-hooks/rules-only. (DESIGN-org-package-agency.md replaces this menu with
// per-org builds; it stays until step 5 deletes it.)
package main

import (
	"log"

	"github.com/pocketbase/pocketbase"

	"tinycld.org/core/tenantmain"
	"tinycld.org/multi-org/internal/tenantpkgs"
	mail "tinycld.org/packages/mail"
)

func main() {
	err := tenantmain.Run(tenantmain.Options{
		RegisterExtras: func(app *pocketbase.PocketBase, ex tenantmain.Extras) {
			// A resolved slug with no Go on the menu is normal (TS-only or
			// third-party package) but logged, so a menu omission is visible
			// at boot instead of silently serving rules-only.
			registered, unknown := tenantpkgs.Register(app, ex.PackageSlugs, tenantpkgs.Options{
				Mail: mail.TenantListeners{
					IMAP:       ex.Mail.IMAP,
					Submission: ex.Mail.Submission,
					InboundMX:  ex.Mail.InboundMX,
				},
			})
			log.Printf("serve-org[%s]: feature Go registered=%v ts-only=%v", ex.Slug, registered, unknown)
		},
	})
	if err != nil {
		log.Fatalf("serve-org: %v", err)
	}
}
