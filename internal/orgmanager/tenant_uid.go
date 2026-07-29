package orgmanager

import (
	"fmt"
	"sync"
)

// uidAllocator hands each org a distinct host uid inside the configured window.
//
// The uid window IS the tenant boundary: file ownership plus chownTree's
// 0600/0700 modes are what stop one tenant reading another's pb_data, and they
// are what closes the cross-org `ATTACH DATABASE` read that tenant JS can
// otherwise reach through PocketBase's raw-SQL surface. Two orgs sharing a uid
// therefore do not degrade isolation for that pair — they eliminate it.
//
// This is why the mapping is allocated and remembered rather than hashed.
// Hashing a slug into the window needs no state and is stable for free, but it
// collides at a birthday rate: 50 orgs in a 1000-uid window collide with ~71%
// probability, and 1000 orgs in a 65536-uid window with ~99.9%. A boundary that
// fails silently for a likely fraction of tenant pairs is not a boundary.
//
// Two properties matter and both are tested:
//
//   - Stability. A tenant's files on disk are owned by its uid, so a slug must
//     map to the same uid for the life of the host. Assignments are held for
//     the process lifetime and re-derived from disk ownership on restart (see
//     adopt), never reallocated underneath existing files.
//   - Fail-closed exhaustion. When the window is full, spawning refuses rather
//     than reusing a uid, because reuse is the failure this type exists to
//     prevent. The operator's fix is to widen MT_TENANT_UID_RANGE.
type uidAllocator struct {
	base int
	span int

	mu     sync.Mutex
	bySlug map[string]int
	byUID  map[int]string
}

func newUIDAllocator(base, span int) *uidAllocator {
	return &uidAllocator{
		base:   base,
		span:   span,
		bySlug: make(map[string]int),
		byUID:  make(map[int]string),
	}
}

// uidFor returns the org's uid, allocating one on first use.
func (a *uidAllocator) uidFor(slug string) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if uid, ok := a.bySlug[slug]; ok {
		return uid, nil
	}
	for off := 0; off < a.span; off++ {
		uid := a.base + off
		if _, taken := a.byUID[uid]; taken {
			continue
		}
		a.bySlug[slug] = uid
		a.byUID[uid] = slug
		return uid, nil
	}
	return 0, fmt.Errorf(
		"tenant uid window [%d,%d) is full (%d orgs): widen MT_TENANT_UID_RANGE — "+
			"refusing to reuse another org's uid, which would put both orgs inside "+
			"one isolation boundary",
		a.base, a.base+a.span, a.span)
}

// adopt records a uid an org already owns on disk, so a restarted host keeps
// the mapping its files were chowned under instead of handing the slug a fresh
// uid and orphaning them.
//
// It reports whether the uid was adopted: a uid outside the window, or one
// already held by a different slug, is refused so a stale or hostile ownership
// stamp cannot collapse two orgs into one uid.
func (a *uidAllocator) adopt(slug string, uid int) bool {
	if uid < a.base || uid >= a.base+a.span {
		return false
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if existing, ok := a.byUID[uid]; ok {
		return existing == slug
	}
	if prior, ok := a.bySlug[slug]; ok {
		// The slug already holds a different uid this process lifetime;
		// keeping it is what stability requires.
		return prior == uid
	}
	a.bySlug[slug] = uid
	a.byUID[uid] = slug
	return true
}
