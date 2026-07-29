package orgmanager

import (
	"context"
	"testing"
	"time"
)

// An Evict that lands while the org is still spawning must not be lost.
//
// Evict is how suspend, archive, and deploy take effect: the control plane
// flips the record and calls Evict so the next Get re-reads it. If Evict only
// inspects the resident map, an evict arriving inside the spawn window matches
// nothing and the instance publishes immediately afterwards — so a suspended
// org keeps serving every request, and because that traffic refreshes lastUsed,
// the idle sweeper never reclaims it either. The same shape leaves a deployed
// org serving the previous package generation while the control plane reports
// the new lockfile.
func TestEvict_DuringInFlightSpawnIsNotLost(t *testing.T) {
	sp := &fakeSpawner{
		gate:         make(chan struct{}),
		spawnEntered: make(chan struct{}),
	}
	mgr := newTestManager(t, sp)

	type result struct {
		inst *OrgInstance
		err  error
	}
	done := make(chan result, 1)
	go func() {
		inst, err := mgr.Get(context.Background(), "acme")
		done <- result{inst, err}
	}()

	// Wait until the spawn is genuinely in flight — the window this test exists
	// to probe. Without this the Evict could land before load starts, which the
	// old code handles correctly and would make the test unfalsifiable.
	select {
	case <-sp.spawnEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("spawn never started")
	}

	mgr.Evict("acme")
	close(sp.gate)

	res := <-done

	// Either outcome is acceptable for THIS caller: it may receive the
	// instance it asked for, or an error. What must not happen is the evicted
	// instance staying resident and serving subsequent callers.
	if res.err == nil && res.inst == nil {
		t.Fatal("Get returned neither an instance nor an error")
	}

	mgr.mu.RLock()
	_, resident := mgr.orgs["acme"]
	mgr.mu.RUnlock()

	if resident {
		t.Fatal("the evicted org is resident in the map: an Evict racing a spawn " +
			"was silently dropped, so suspend/archive/deploy do not take effect")
	}
}

// The eviction must also tear the process down, not merely unlink it from the
// map — otherwise a suspended org's tenant keeps running (and holding its
// sockets and memory) until the host restarts.
func TestEvict_DuringInFlightSpawnStopsTheProcess(t *testing.T) {
	sp := &fakeSpawner{
		gate:         make(chan struct{}),
		spawnEntered: make(chan struct{}),
	}
	mgr := newTestManager(t, sp)

	go func() { _, _ = mgr.Get(context.Background(), "acme") }()

	select {
	case <-sp.spawnEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("spawn never started")
	}

	mgr.Evict("acme")
	close(sp.gate)

	proc := sp.lastProc()
	if proc == nil {
		t.Fatal("no process was spawned")
	}
	stopped := waitFor(5*time.Second, func() bool {
		select {
		case <-proc.exited:
			return true
		default:
			return false
		}
	})
	if !stopped {
		t.Fatal("the tenant process spawned during an evicted load is still " +
			"running: it holds its sockets and memory with nothing tracking it")
	}
}
