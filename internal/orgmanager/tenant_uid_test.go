package orgmanager

import (
	"fmt"
	"testing"
)

// The uid window IS the tenant boundary. Two orgs sharing a host uid lose uid
// separation, chownTree's 0600/0700 modes, and the cross-org ATTACH DATABASE
// defence simultaneously — a total boundary failure for that pair, not a
// degradation. A pure hash-modulo mapping collides at a birthday rate, so these
// tests pin the allocator's contract: distinct slugs never share a uid, and the
// mapping is stable across restarts because tenant files on disk are owned by
// it.

func TestTenantUID_NoCollisionsWithinWindow(t *testing.T) {
	const (
		base = 60000
		span = 64
	)
	a := newUIDAllocator(base, span)

	// Far more slugs than a hash-modulo mapping could keep distinct: with 64
	// uids, a hash collides among 40 slugs with probability ~1 - e^(-40*39/128),
	// i.e. essentially always.
	seen := make(map[int]string, 40)
	for i := 0; i < span; i++ {
		slug := fmt.Sprintf("org-%d", i)
		uid, err := a.uidFor(slug)
		if err != nil {
			t.Fatalf("uidFor(%s): %v", slug, err)
		}
		if uid < base || uid >= base+span {
			t.Fatalf("uidFor(%s) = %d, outside window [%d,%d)", slug, uid, base, base+span)
		}
		if prev, dup := seen[uid]; dup {
			t.Fatalf("uid %d assigned to both %q and %q — the tenant boundary "+
				"collapses for that pair", uid, prev, slug)
		}
		seen[uid] = slug
	}
}

func TestTenantUID_StableAcrossLookups(t *testing.T) {
	a := newUIDAllocator(60000, 500)

	first, err := a.uidFor("acme")
	if err != nil {
		t.Fatal(err)
	}
	// Interleave other orgs: a naive "next free" allocator that forgets its
	// assignments would hand acme a different uid the second time, orphaning
	// every file chownTree already wrote.
	for _, s := range []string{"globex", "initech", "umbrella"} {
		if _, err := a.uidFor(s); err != nil {
			t.Fatal(err)
		}
	}
	again, err := a.uidFor("acme")
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Fatalf("uidFor(acme) = %d then %d; the mapping must be stable or the "+
			"tenant's on-disk files stop matching its uid", first, again)
	}
}

// A full window must fail closed. Silently reusing a uid would put two orgs
// inside one boundary, which is exactly the failure the allocator exists to
// prevent — refusing to spawn is the safe direction.
func TestTenantUID_ExhaustedWindowFailsClosed(t *testing.T) {
	const span = 4
	a := newUIDAllocator(60000, span)

	for i := 0; i < span; i++ {
		if _, err := a.uidFor(fmt.Sprintf("org-%d", i)); err != nil {
			t.Fatalf("uidFor(org-%d) failed while the window still had room: %v", i, err)
		}
	}

	if _, err := a.uidFor("one-too-many"); err == nil {
		t.Fatal("uidFor succeeded on an exhausted window; it must fail closed " +
			"rather than reuse another org's uid")
	}
}

// On restart the allocator must re-adopt the uid an org's files are already
// chowned under, or the tenant loses access to its own pb_data.
func TestTenantUID_AdoptKeepsExistingOwnership(t *testing.T) {
	a := newUIDAllocator(60000, 500)

	if !a.adopt("acme", 60123) {
		t.Fatal("adopt refused a uid inside the window")
	}
	uid, err := a.uidFor("acme")
	if err != nil {
		t.Fatal(err)
	}
	if uid != 60123 {
		t.Fatalf("uidFor(acme) = %d after adopting 60123; on-disk ownership must win", uid)
	}

	// A later org must not be handed the adopted uid.
	other, err := a.uidFor("globex")
	if err != nil {
		t.Fatal(err)
	}
	if other == 60123 {
		t.Fatal("allocated an adopted uid to a second org")
	}
}

func TestTenantUID_AdoptRefusesUnsafeStamps(t *testing.T) {
	a := newUIDAllocator(60000, 500)

	if a.adopt("outside", 59999) || a.adopt("outside", 60500) {
		t.Fatal("adopted a uid outside the configured window")
	}

	if !a.adopt("acme", 60123) {
		t.Fatal("adopt refused a valid uid")
	}
	// A second slug claiming acme's uid — a stale or tampered ownership stamp —
	// must be refused rather than collapsing both orgs into one boundary.
	if a.adopt("impostor", 60123) {
		t.Fatal("adopted a uid already held by another org")
	}
}
