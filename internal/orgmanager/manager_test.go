package orgmanager

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"tinycld.org/multi-org/internal/orgerr"
	"tinycld.org/multi-org/internal/store"
)

func stubLookup(m map[string]OrgRecord) LookupFunc {
	return func(slug string) (OrgRecord, bool) { r, ok := m[slug]; return r, ok }
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestManager wires a manager over a fake spawner and a store holding one
// trivial package.
func newTestManager(t *testing.T, sp *fakeSpawner) *OrgManager {
	t.Helper()
	root := t.TempDir()

	s := store.New(root)
	if err := s.Publish("@tinycld/core", "1.0.0", map[string][]byte{
		"server/main.pb.js":      []byte("routerAdd('GET','/ping',(e)=>e.json(200,{ok:true}))"),
		"client/dist/index.html": []byte("<html></html>"),
	}); err != nil {
		t.Fatal(err)
	}

	mgr := New(Config{
		Root:      root,
		Store:     s,
		Spawner:   sp,
		Logger:    quietLogger(),
		HooksPool: 2,
		LookupOrg: stubLookup(map[string]OrgRecord{
			"acme":      {Slug: "acme", Status: "active", Lockfile: []byte(`{"@tinycld/core":"1.0.0"}`)},
			"suspended": {Slug: "suspended", Status: "suspended", Lockfile: []byte(`{"@tinycld/core":"1.0.0"}`)},
		}),
	})
	t.Cleanup(mgr.Shutdown)
	return mgr
}

func TestGet_SpawnsAndProxies(t *testing.T) {
	sp := &fakeSpawner{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello from tenant"))
	})}
	mgr := newTestManager(t, sp)

	inst, err := mgr.Get(context.Background(), "acme")
	if err != nil {
		t.Fatalf("Get(acme): %v", err)
	}

	rec := httptest.NewRecorder()
	inst.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "hello from tenant" {
		t.Fatalf("body = %q, want the tenant's response", got)
	}
}

// The proxy must preserve the inbound Host: PocketBase builds absolute URLs
// (verification links, OAuth redirects) from it.
func TestProxy_PreservesHostAndSetsForwardedHeaders(t *testing.T) {
	var gotHost, gotFwd string
	sp := &fakeSpawner{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		gotFwd = r.Header.Get("X-Forwarded-For")
	})}
	mgr := newTestManager(t, sp)

	inst, err := mgr.Get(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Host = "acme.tinycld.org"
	req.RemoteAddr = "203.0.113.7:1234"
	inst.Mux().ServeHTTP(httptest.NewRecorder(), req)

	if gotHost != "acme.tinycld.org" {
		t.Fatalf("tenant saw Host %q, want acme.tinycld.org", gotHost)
	}
	if gotFwd == "" {
		t.Fatal("expected X-Forwarded-For to be set")
	}
}

// Realtime is SSE; without immediate flushing those streams stall silently
// rather than failing, so assert a chunk arrives before the handler returns.
func TestProxy_StreamsWithoutBuffering(t *testing.T) {
	release := make(chan struct{})
	sp := &fakeSpawner{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: first\n\n"))
		w.(http.Flusher).Flush()
		<-release // hold the response open
	})}
	mgr := newTestManager(t, sp)

	inst, err := mgr.Get(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(inst.Mux())
	defer srv.Close()
	defer close(release)

	resp, err := http.Get(srv.URL + "/api/realtime")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	buf := make([]byte, len("data: first\n\n"))
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(resp.Body, buf)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("read stream: %v", err)
		}
		if !strings.Contains(string(buf), "first") {
			t.Fatalf("got %q, want the first event", buf)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no data before the handler returned — the proxy is buffering SSE")
	}
}

func TestGet_UnknownOrgIsNotFound(t *testing.T) {
	mgr := newTestManager(t, &fakeSpawner{})
	_, err := mgr.Get(context.Background(), "ghost")
	if !errors.Is(err, orgerr.ErrOrgNotFound) {
		t.Fatalf("err = %v, want ErrOrgNotFound", err)
	}
}

func TestGet_SuspendedOrgIsNotActive(t *testing.T) {
	mgr := newTestManager(t, &fakeSpawner{})
	_, err := mgr.Get(context.Background(), "suspended")
	if !errors.Is(err, orgerr.ErrOrgNotActive) {
		t.Fatalf("err = %v, want ErrOrgNotActive", err)
	}
}

func TestGet_ReadyFailureIsUnavailable(t *testing.T) {
	sp := &fakeSpawner{failReady: "migration 003 failed"}
	mgr := newTestManager(t, sp)

	_, err := mgr.Get(context.Background(), "acme")
	if !errors.Is(err, orgerr.ErrOrgUnavailable) {
		t.Fatalf("err = %v, want ErrOrgUnavailable", err)
	}
	if !strings.Contains(err.Error(), "migration 003 failed") {
		t.Fatalf("err = %v, want the child's reason included", err)
	}
}

// A child that dies during boot must be detected via EOF on the readiness pipe,
// not by waiting out the spawn timeout.
func TestGet_DeathBeforeReadyFailsFast(t *testing.T) {
	sp := &fakeSpawner{dieBeforeReady: true}
	mgr := newTestManager(t, sp)

	start := time.Now()
	_, err := mgr.Get(context.Background(), "acme")
	elapsed := time.Since(start)

	if !errors.Is(err, orgerr.ErrOrgUnavailable) {
		t.Fatalf("err = %v, want ErrOrgUnavailable", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("took %s — EOF should be detected immediately, not by timeout", elapsed)
	}
}

func TestGet_FailedLoadLeavesNoInstance(t *testing.T) {
	sp := &fakeSpawner{failReady: "nope"}
	mgr := newTestManager(t, sp)

	if _, err := mgr.Get(context.Background(), "acme"); err == nil {
		t.Fatal("expected an error")
	}
	mgr.mu.RLock()
	n := len(mgr.orgs)
	mgr.mu.RUnlock()
	if n != 0 {
		t.Fatalf("resident orgs = %d, want 0 after a failed load", n)
	}
}

func TestGet_ConcurrentCallsSpawnOnce(t *testing.T) {
	sp := &fakeSpawner{}
	mgr := newTestManager(t, sp)

	var wg sync.WaitGroup
	insts := make([]*OrgInstance, 10)
	for i := range insts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			in, err := mgr.Get(context.Background(), "acme")
			if err != nil {
				t.Error(err)
				return
			}
			insts[i] = in
		}(i)
	}
	wg.Wait()

	for i := 1; i < len(insts); i++ {
		if insts[i] != insts[0] {
			t.Fatal("concurrent Get returned different instances — singleflight failed")
		}
	}
	if got := sp.spawnCount(); got != 1 {
		t.Fatalf("spawned %d processes, want 1", got)
	}
}

// A caller giving up must not abort the spawn for everyone else waiting on it.
func TestGet_CallerCancellationDoesNotAbortSharedSpawn(t *testing.T) {
	sp := &fakeSpawner{}
	mgr := newTestManager(t, sp)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := mgr.Get(ctx, "acme"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	// A fresh caller still gets a working instance.
	inst, err := mgr.Get(context.Background(), "acme")
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if inst == nil {
		t.Fatal("expected an instance")
	}
}

func TestEvict_StopsChildAndNextGetRespawns(t *testing.T) {
	sp := &fakeSpawner{}
	mgr := newTestManager(t, sp)

	first, err := mgr.Get(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	proc := sp.lastProc()

	mgr.Evict("acme")

	if !waitFor(3*time.Second, func() bool { return proc.gotSIGTERM.Load() }) {
		t.Fatal("evicted child was never signalled")
	}

	second, err := mgr.Get(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("expected a fresh instance after Evict")
	}
	if got := sp.spawnCount(); got != 2 {
		t.Fatalf("spawned %d, want 2", got)
	}
}

// A child that ignores SIGTERM must be killed rather than lingering.
func TestEvict_KillsChildThatIgnoresSIGTERM(t *testing.T) {
	sp := &fakeSpawner{ignoreSIGTERM: true}
	mgr := newTestManager(t, sp)

	if _, err := mgr.Get(context.Background(), "acme"); err != nil {
		t.Fatal(err)
	}
	proc := sp.lastProc()

	go mgr.Evict("acme")

	if !waitFor(killTimeout+5*time.Second, func() bool { return proc.killed.Load() }) {
		t.Fatal("child ignoring SIGTERM was never killed")
	}
}

// After teardown starts, the instance must stop accepting new requests rather
// than proxying them to a dying child.
func TestShutdownInstance_RejectsNewRequests(t *testing.T) {
	sp := &fakeSpawner{}
	mgr := newTestManager(t, sp)

	inst, err := mgr.Get(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	mgr.Evict("acme")
	if !waitFor(3*time.Second, func() bool {
		select {
		case <-inst.closed:
			return true
		default:
			return false
		}
	}) {
		t.Fatal("instance never entered shutdown")
	}

	rec := httptest.NewRecorder()
	inst.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 once shutting down", rec.Code)
	}
}

// An unexpected exit must drop the instance so the next Get respawns it.
func TestSupervisor_CrashRemovesInstance(t *testing.T) {
	sp := &fakeSpawner{}
	mgr := newTestManager(t, sp)

	if _, err := mgr.Get(context.Background(), "acme"); err != nil {
		t.Fatal(err)
	}
	sp.lastProc().crash()

	if !waitFor(3*time.Second, func() bool {
		mgr.mu.RLock()
		defer mgr.mu.RUnlock()
		return len(mgr.orgs) == 0
	}) {
		t.Fatal("crashed instance was not removed from the map")
	}
}

// A repeatedly-crashing org must back off rather than being respawned on every
// request.
func TestBackoff_CrashLoopingOrgIsRejectedWithoutSpawning(t *testing.T) {
	sp := &fakeSpawner{dieBeforeReady: true}
	mgr := newTestManager(t, sp)

	if _, err := mgr.Get(context.Background(), "acme"); err == nil {
		t.Fatal("expected the first spawn to fail")
	}
	spawnsAfterFirst := sp.spawnCount()

	_, err := mgr.Get(context.Background(), "acme")
	if !errors.Is(err, orgerr.ErrOrgUnavailable) {
		t.Fatalf("err = %v, want ErrOrgUnavailable", err)
	}
	if !strings.Contains(err.Error(), "crash-looping") {
		t.Fatalf("err = %v, want a crash-loop message", err)
	}
	if got := sp.spawnCount(); got != spawnsAfterFirst {
		t.Fatalf("spawned %d more processes during backoff, want 0", got-spawnsAfterFirst)
	}
}

// A successful boot must clear prior crash history.
func TestBackoff_ClearedOnSuccessfulBoot(t *testing.T) {
	sp := &fakeSpawner{}
	mgr := newTestManager(t, sp)

	mgr.noteCrash("acme")
	mgr.clearCrash("acme")

	if _, err := mgr.Get(context.Background(), "acme"); err != nil {
		t.Fatalf("Get after cleared backoff: %v", err)
	}
	mgr.mu.RLock()
	_, stillTracked := mgr.crashes["acme"]
	mgr.mu.RUnlock()
	if stillTracked {
		t.Fatal("crash state survived a successful boot")
	}
}

func TestShutdown_IsIdempotent(t *testing.T) {
	sp := &fakeSpawner{}
	root := t.TempDir()
	mgr := New(Config{
		Root:      root,
		Store:     store.New(root),
		Spawner:   sp,
		Logger:    quietLogger(),
		LookupOrg: stubLookup(nil),
	})
	mgr.Shutdown()
	mgr.Shutdown() // must not panic on a double close
}

func TestGet_AfterShutdownStoresNothing(t *testing.T) {
	sp := &fakeSpawner{}
	mgr := newTestManager(t, sp)
	mgr.Shutdown()

	_, _ = mgr.Get(context.Background(), "acme")

	mgr.mu.RLock()
	n := len(mgr.orgs)
	mgr.mu.RUnlock()
	if n != 0 {
		t.Fatalf("resident orgs = %d, want 0 after shutdown", n)
	}
}

func TestInstance_LastUsedSeededAndAdvancedByRequests(t *testing.T) {
	sp := &fakeSpawner{}
	mgr := newTestManager(t, sp)

	inst, err := mgr.Get(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if inst.lastUsed.Load() == 0 {
		t.Fatal("lastUsed must be seeded at load, or the sweeper sees it as infinitely idle")
	}

	// Proxied requests must touch it too, so a long-lived SSE stream is not
	// swept out from under itself.
	inst.lastUsed.Store(1)
	inst.Mux().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if inst.lastUsed.Load() <= 1 {
		t.Fatal("a proxied request did not advance lastUsed")
	}
}
