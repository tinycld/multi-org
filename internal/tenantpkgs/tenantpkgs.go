// Package tenantpkgs pins the canonical feature menu a hosted deployment
// supports — the set of feature packages whose server Go links into the
// serve-org tenant binary.
//
// This is docs/SCOPE-tenant-feature-go.md closed as its option (b): the menu
// is HAND-PINNED here, never emitted by the workspace generator, so the
// tenant binary does not silently depend on whichever members a developer's
// bootstrap assembly happens to contain (a router built in a mail-only
// workspace must not quietly host a calendar org without calendar's guards —
// that is the composition-gap bug shape all over again). The trade, recorded:
// hosted packages OUTSIDE this menu are TS-hooks/rules-only; their Go, if
// any, does not run in a tenant.
//
// Each entry is a feature's RegisterTenant — the tenant-shaped composition
// that feature exports (shared record hooks, endpoints, workers; no port
// listeners, no DAV mounts — the router owns every listening socket and the
// tenant mounts DAV from materialized config). Registration is gated by the
// org's RESOLVED package set: an org that has not installed a package must
// not get its hooks, its background goroutines, or its collections'
// behaviour.
package tenantpkgs

import (
	"sort"

	"github.com/pocketbase/pocketbase"

	calc "tinycld.org/packages/calc"
	calendar "tinycld.org/packages/calendar"
	contacts "tinycld.org/packages/contacts"
	drive "tinycld.org/packages/drive"
	mail "tinycld.org/packages/mail"
	text "tinycld.org/packages/text"
)

// registrars is the pinned menu: manifest slug → the feature's tenant entry.
// Adding a row means (1) a `replace tinycld.org/packages/<slug>` in go.mod,
// (2) the feature exports RegisterTenant, and (3) rebuilding + redeploying
// the tenant binary. google-takeout-import ships no server Go.
var registrars = map[string]func(*pocketbase.PocketBase){
	"calc":     calc.RegisterTenant,
	"calendar": calendar.RegisterTenant,
	"contacts": contacts.RegisterTenant,
	"drive":    drive.RegisterTenant,
	"mail":     mail.RegisterTenant,
	"text":     text.RegisterTenant,
}

// Register runs the tenant entry of every enabled slug that is on the pinned
// menu, in sorted order for deterministic hook binding. Returns the slugs it
// registered and the enabled slugs it has no Go for (TS-only or third-party
// packages — not an error, but worth a boot log line so a menu omission is
// visible instead of silent).
func Register(app *pocketbase.PocketBase, enabled []string) (registered, unknown []string) {
	slugs := append([]string(nil), enabled...)
	sort.Strings(slugs)
	for _, slug := range slugs {
		reg, ok := registrars[slug]
		if !ok {
			unknown = append(unknown, slug)
			continue
		}
		reg(app)
		registered = append(registered, slug)
	}
	return registered, unknown
}

// Menu returns the pinned slugs, sorted. Exposed for tests and logging.
func Menu() []string {
	out := make([]string, 0, len(registrars))
	for slug := range registrars {
		out = append(out, slug)
	}
	sort.Strings(out)
	return out
}
