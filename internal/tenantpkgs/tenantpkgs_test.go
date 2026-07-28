package tenantpkgs

import (
	"reflect"
	"testing"

	"github.com/pocketbase/pocketbase"

	"tinycld.org/core/rlstest"
)

// An org that has not installed a package must not get its hooks — the gate is
// the whole point of the menu. contacts is the probe feature here because its
// registrations are safe to repeat within one test process (calc/text register
// a realtime room kind, which panics on a duplicate).
func TestRegisterGatesBySlug(t *testing.T) {
	bare := pocketbase.NewWithConfig(pocketbase.Config{DefaultDataDir: t.TempDir()})
	bareCounts := rlstest.HookHandlerCounts(t, bare)

	// nil enabled set → nothing registered, nothing unknown.
	empty := pocketbase.NewWithConfig(pocketbase.Config{DefaultDataDir: t.TempDir()})
	registered, unknown := Register(empty, nil, Options{})
	if len(registered) != 0 || len(unknown) != 0 {
		t.Fatalf("Register(nil) = %v, %v; want none registered, none unknown", registered, unknown)
	}
	if got := rlstest.HookHandlerCounts(t, empty); !reflect.DeepEqual(got, bareCounts) {
		t.Fatalf("an empty package set must contribute no hooks; got %v want %v", got, bareCounts)
	}

	// An enabled menu slug registers its Go; a slug with no Go on the menu
	// (TS-only or third-party package) is reported, not an error.
	app := pocketbase.NewWithConfig(pocketbase.Config{DefaultDataDir: t.TempDir()})
	registered, unknown = Register(app, []string{"acme-custom", "contacts"}, Options{})
	if !reflect.DeepEqual(registered, []string{"contacts"}) {
		t.Fatalf("registered = %v, want [contacts]", registered)
	}
	if !reflect.DeepEqual(unknown, []string{"acme-custom"}) {
		t.Fatalf("unknown = %v, want [acme-custom]", unknown)
	}

	withCounts := rlstest.HookHandlerCounts(t, app)
	more := false
	for name, n := range withCounts {
		if n > bareCounts[name] {
			more = true
			break
		}
	}
	if !more {
		t.Fatal("enabling contacts bound no hooks — the menu entry registered nothing")
	}
}

// The menu is the recorded product decision (SCOPE-tenant-feature-go.md option
// b). Pin its contents so a slug can't fall off — or sneak on — without this
// test naming the change.
func TestMenuIsThePinnedSet(t *testing.T) {
	want := []string{"calc", "calendar", "contacts", "drive", "mail", "text"}
	if got := Menu(); !reflect.DeepEqual(got, want) {
		t.Fatalf("pinned menu = %v, want %v — changing it is a product decision; update this test alongside go.mod and the registrars map", got, want)
	}
}
