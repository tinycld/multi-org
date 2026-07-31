package orgmanager

import (
	"context"
	"errors"
	"testing"
	"time"

	"tinycld.org/multi-org/internal/orgerr"
)

// newAdmissionManager wires a manager over sp with the given caps and three
// active orgs (a, b, c) — all booting the same committed artifact — so
// admission tests can fill the resident set.
func newAdmissionManager(t *testing.T, sp *fakeSpawner, maxResident, maxSpawns int) *OrgManager {
	t.Helper()
	root := t.TempDir()

	ref := buildArtifact(t, buildsRootFor(root), artifactSpec{
		Hash:  "core1",
		Files: map[string]string{"pb_public/index.html": "<html></html>"},
	})

	mgr := New(Config{
		Root:                root,
		Spawner:             sp,
		Logger:              quietLogger(),
		MaxResident:         maxResident,
		MaxConcurrentSpawns: maxSpawns,
		ResolveBuild:        resolveBuilds(map[string]BuildRef{"core1": ref}),
		LookupOrg: stubLookup(map[string]OrgRecord{
			"a": {Slug: "a", Status: "active", RecipeHash: "core1"},
			"b": {Slug: "b", Status: "active", RecipeHash: "core1"},
			"c": {Slug: "c", Status: "active", RecipeHash: "core1"},
		}),
	})
	t.Cleanup(mgr.Shutdown)
	return mgr
}

func residentSlugs(mgr *OrgManager) map[string]bool {
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()
	out := map[string]bool{}
	for slug := range mgr.orgs {
		out[slug] = true
	}
	return out
}

// With the resident set full, admitting a new org must evict the
// least-recently-used idle instance rather than growing without bound — one
// request per enumerable slug must not hold every org's process resident.
func TestAdmission_ResidentCapEvictsLRUIdle(t *testing.T) {
	sp := &fakeSpawner{}
	mgr := newAdmissionManager(t, sp, 2, 0)

	if _, err := mgr.Get(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	instB, err := mgr.Get(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	procB := sp.lastProc()
	// Backdate b explicitly rather than re-touching a: consecutive touches can
	// land on the same wall-clock tick, which would leave the LRU choice to
	// map iteration order.
	instB.lastUsed.Store(1)

	if _, err := mgr.Get(context.Background(), "c"); err != nil {
		t.Fatalf("Get(c) at capacity: %v — want LRU eviction, not refusal", err)
	}

	res := residentSlugs(mgr)
	if len(res) > 2 {
		t.Fatalf("resident orgs = %v, want at most MaxResident (2)", res)
	}
	if !res["a"] || !res["c"] {
		t.Fatalf("resident orgs = %v, want the touched org (a) and the new one (c)", res)
	}
	if !waitFor(3*time.Second, func() bool { return procB.gotSIGTERM.Load() }) {
		t.Fatal("LRU org b was dropped from the map but its child was never torn down")
	}
}

// When every resident org is busy (long-lived mail connections tracked via
// TrackConn), the manager must refuse the newcomer rather than killing a live
// session out from under a client.
func TestAdmission_AtCapacityWithBusyOrgsRefuses(t *testing.T) {
	sp := &fakeSpawner{}
	mgr := newAdmissionManager(t, sp, 1, 0)

	instA, err := mgr.Get(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	release := instA.TrackConn()
	defer release()

	_, err = mgr.Get(context.Background(), "b")
	if !errors.Is(err, orgerr.ErrOrgUnavailable) {
		t.Fatalf("err = %v, want ErrOrgUnavailable when at capacity and every org is busy", err)
	}
	if got := sp.spawnCount(); got != 1 {
		t.Fatalf("spawned %d, want 1 — the refused org must not have spawned", got)
	}
	if !residentSlugs(mgr)["a"] {
		t.Fatal("the busy org was evicted; refusal must leave it serving")
	}
}

// Concurrent cold starts are capped: with one spawn slot, a second org's load
// must wait for the first to finish rather than piling a spawn stampede onto
// the host (each cold start runs migrations + JS compilation).
func TestAdmission_ConcurrentSpawnsCapped(t *testing.T) {
	gate := make(chan struct{})
	sp := &fakeSpawner{gate: gate, spawnEntered: make(chan struct{})}
	mgr := newAdmissionManager(t, sp, 0, 1)

	done := make(chan error, 2)
	go func() {
		_, err := mgr.Get(context.Background(), "a")
		done <- err
	}()
	<-sp.spawnEntered

	go func() {
		_, err := mgr.Get(context.Background(), "b")
		done <- err
	}()

	// b must not enter Spawn while a holds the only slot.
	if waitFor(200*time.Millisecond, func() bool { return sp.spawnCount() > 1 }) {
		t.Fatal("second spawn started while the only spawn slot was held")
	}

	close(gate)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if got := sp.spawnCount(); got != 2 {
		t.Fatalf("spawned %d, want 2 once the slot freed up", got)
	}
}
