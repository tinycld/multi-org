package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"tinycld.org/core/pkgbuild"
	"tinycld.org/core/tenantcfg"
	"tinycld.org/multi-org/internal/builder"
)

func quietTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

const (
	hashOld = "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hashNew = "sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type fakeArtifactBuilder struct {
	hash  string
	err   error
	calls atomic.Int32
}

func (f *fakeArtifactBuilder) Build(_ context.Context, refs []builder.PackageRef, _ pkgbuild.ProgressSink) (builder.Result, error) {
	f.calls.Add(1)
	if f.err != nil {
		return builder.Result{}, f.err
	}
	return builder.Result{RecipeHash: f.hash, Dir: "/builds/x", Cached: true}, nil
}

func (f *fakeArtifactBuilder) NpmRegistry() string { return "" }

func (f *fakeArtifactBuilder) ResolveSpec(spec string) (builder.ResolvedSpec, error) {
	if f.err != nil {
		return builder.ResolvedSpec{}, f.err
	}
	return builder.ResolvedSpec{
		Slug: "todo", Name: "@tinycld/todo", Version: "1.0.0",
		Integrity: "sha256:ff", Migrations: []string{"1751000000_todo.js"},
	}, nil
}

// deployHarness is a control plane with one active artifact-backed org.
type deployHarness struct {
	cp     *ControlPlane
	root   string
	orgDir string
}

func newDeployHarness(t *testing.T) *deployHarness {
	t.Helper()
	root := t.TempDir()
	cp, err := New(filepath.Join(root, "pb_control", "pb_data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.App.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { WaitForAppDeploys(cp.App); _ = cp.App.ResetBootstrapState() })
	if err := cpInitForTest(cp); err != nil {
		t.Fatal(err)
	}

	col, err := cp.App.FindCollectionByNameOrId("orgs")
	if err != nil {
		t.Fatal(err)
	}
	rec := core.NewRecord(col)
	rec.Set("slug", "acme")
	rec.Set("status", "active")
	rec.Set("data_dir", "pb_orgs/acme")
	rec.Set("lockfile", `{"tinycld":"1.0.0"}`)
	rec.Set("recipe_hash", hashOld)
	if err := cp.App.Save(rec); err != nil {
		t.Fatal(err)
	}

	orgDir := filepath.Join(root, "pb_orgs", "acme")
	if err := os.MkdirAll(filepath.Join(orgDir, "pb_data"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &deployHarness{cp: cp, root: root, orgDir: orgDir}
}

func (h *deployHarness) org(t *testing.T) *core.Record {
	t.Helper()
	rec, err := h.cp.App.FindFirstRecordByData("orgs", "slug", "acme")
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func (h *deployHarness) lastDeployment(t *testing.T) *core.Record {
	t.Helper()
	recs, err := h.cp.App.FindRecordsByFilter("deployments", "1=1", "-created", 1, 0)
	if err != nil || len(recs) == 0 {
		t.Fatalf("no deployments row (err=%v)", err)
	}
	return recs[0]
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

var nextLock = map[string]string{"tinycld": "1.1.0", "@tinycld/mail": "2.0.0"}

func TestDeploy_CommitsOnHealthyRespawn(t *testing.T) {
	h := newDeployHarness(t)

	// A tenant-left snapshot that a committed deploy must drop.
	if err := os.MkdirAll(filepath.Join(h.orgDir, ".deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshotPath(h.orgDir), []byte("snapshot"), 0o644); err != nil {
		t.Fatal(err)
	}

	var evicted, verified atomic.Int32
	d := newDeployer(h.cp.App, h.root, &fakeArtifactBuilder{hash: hashNew},
		func(slug string) { evicted.Add(1) },
		func(ctx context.Context, slug string) error { verified.Add(1); return nil },
		quietTestLogger())

	gotHash, err := d.Deploy(context.Background(), "acme", nextLock, "job_42")
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if gotHash != hashNew {
		t.Fatalf("Deploy returned %q", gotHash)
	}

	// The proposal is already durable before finish() runs.
	org := h.org(t)
	if org.GetString("recipe_hash") != hashNew || org.GetString("prev_recipe_hash") != hashOld {
		t.Fatalf("org row not repointed: recipe=%s prev=%s", org.GetString("recipe_hash"), org.GetString("prev_recipe_hash"))
	}
	if !strings.Contains(org.GetString("lockfile"), "@tinycld/mail") {
		t.Fatalf("org lockfile not updated: %s", org.GetString("lockfile"))
	}

	waitUntil(t, "deployment committed", func() bool {
		return h.lastDeployment(t).GetString("status") == "committed"
	})
	dep := h.lastDeployment(t)
	if dep.GetString("recipe_hash") != hashNew || dep.GetString("job_id") != "job_42" {
		t.Fatalf("deployment row = recipe %s job %s", dep.GetString("recipe_hash"), dep.GetString("job_id"))
	}
	if evicted.Load() != 1 || verified.Load() != 1 {
		t.Fatalf("evicted=%d verified=%d, want 1/1", evicted.Load(), verified.Load())
	}

	res, ok, err := tenantcfg.LoadDeployResult(h.orgDir)
	if err != nil || !ok {
		t.Fatalf("deploy result missing (ok=%v err=%v)", ok, err)
	}
	if res.Status != tenantcfg.DeployCommitted || res.JobID != "job_42" || res.RecipeHash != hashNew {
		t.Fatalf("deploy result = %+v", res)
	}
	if _, err := os.Stat(snapshotPath(h.orgDir)); !os.IsNotExist(err) {
		t.Fatalf("snapshot not dropped after commit (err=%v)", err)
	}
}

func TestDeploy_RevertsOnFailedRespawn(t *testing.T) {
	h := newDeployHarness(t)

	// Live DB mutated by the tenant's downs; the snapshot holds the pre-deploy
	// bytes. WAL sidecars must not survive a restore.
	dbPath := filepath.Join(h.orgDir, "pb_data", "data.db")
	if err := os.WriteFile(dbPath, []byte("post-downs"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, sidecar := range []string{dbPath + "-wal", dbPath + "-shm"} {
		if err := os.WriteFile(sidecar, []byte("frames"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(h.orgDir, ".deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshotPath(h.orgDir), []byte("pre-deploy"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The new build fails its boot; the restored previous build comes up.
	var verifies atomic.Int32
	verify := func(ctx context.Context, slug string) error {
		verifies.Add(1)
		rec, err := h.cp.App.FindFirstRecordByData("orgs", "slug", slug)
		if err != nil {
			return err
		}
		if rec.GetString("recipe_hash") == hashNew {
			return fmt.Errorf("migration exploded")
		}
		return nil
	}
	d := newDeployer(h.cp.App, h.root, &fakeArtifactBuilder{hash: hashNew},
		func(string) {}, verify, quietTestLogger())

	// Tenant-proposed: the downs ran behind the snapshot, so the revert
	// restores it.
	if _, err := d.DeployProposed(context.Background(), "acme", nextLock, "job_9"); err != nil {
		t.Fatalf("DeployProposed: %v", err)
	}

	waitUntil(t, "deployment reverted", func() bool {
		return h.lastDeployment(t).GetString("status") == "reverted"
	})

	org := h.org(t)
	if org.GetString("recipe_hash") != hashOld || org.GetString("prev_recipe_hash") != "" {
		t.Fatalf("org row not restored: recipe=%s prev=%s", org.GetString("recipe_hash"), org.GetString("prev_recipe_hash"))
	}
	if org.GetString("lockfile") != `{"tinycld":"1.0.0"}` {
		t.Fatalf("org lockfile not restored: %s", org.GetString("lockfile"))
	}

	db, err := os.ReadFile(dbPath)
	if err != nil || string(db) != "pre-deploy" {
		t.Fatalf("database not restored from snapshot: %q (err=%v)", db, err)
	}
	for _, sidecar := range []string{dbPath + "-wal", dbPath + "-shm"} {
		if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
			t.Fatalf("WAL sidecar %s survived the restore", sidecar)
		}
	}

	dep := h.lastDeployment(t)
	if !strings.Contains(dep.GetString("error"), "migration exploded") {
		t.Fatalf("deployment error = %q", dep.GetString("error"))
	}

	res, ok, err := tenantcfg.LoadDeployResult(h.orgDir)
	if err != nil || !ok {
		t.Fatalf("deploy result missing (ok=%v err=%v)", ok, err)
	}
	if res.Status != tenantcfg.DeployReverted || res.RecipeHash != hashOld || !strings.Contains(res.Error, "migration exploded") {
		t.Fatalf("deploy result = %+v", res)
	}

	// The failed boot plus the post-revert respawn.
	if verifies.Load() != 2 {
		t.Fatalf("verify ran %d times, want 2 (commit decision + respawn)", verifies.Load())
	}
}

// An OPERATOR deploy has no proposing tenant and took no downs, so its revert
// must never restore .deploy/backup.db: any snapshot present is
// tenant-planted, not this deploy's (M3).
func TestDeploy_OperatorRevertIgnoresPlantedSnapshot(t *testing.T) {
	h := newDeployHarness(t)

	dbPath := filepath.Join(h.orgDir, "pb_data", "data.db")
	if err := os.WriteFile(dbPath, []byte("live"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(h.orgDir, ".deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshotPath(h.orgDir), []byte("planted"), 0o644); err != nil {
		t.Fatal(err)
	}

	verify := func(ctx context.Context, slug string) error {
		rec, err := h.cp.App.FindFirstRecordByData("orgs", "slug", slug)
		if err != nil {
			return err
		}
		if rec.GetString("recipe_hash") == hashNew {
			return fmt.Errorf("boot failed")
		}
		return nil
	}
	d := newDeployer(h.cp.App, h.root, &fakeArtifactBuilder{hash: hashNew},
		func(string) {}, verify, quietTestLogger())

	if _, err := d.Deploy(context.Background(), "acme", nextLock, ""); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	waitUntil(t, "deployment reverted", func() bool {
		return h.lastDeployment(t).GetString("status") == "reverted"
	})

	if db, err := os.ReadFile(dbPath); err != nil || string(db) != "live" {
		t.Fatalf("operator revert adopted a planted snapshot: db=%q err=%v", db, err)
	}
}

// A tenant-proposed deploy restores ONLY the snapshot fingerprinted at
// proposal time: a file swapped in afterwards is refused (M3).
func TestDeploy_ProposedRevertRefusesSwappedSnapshot(t *testing.T) {
	h := newDeployHarness(t)

	dbPath := filepath.Join(h.orgDir, "pb_data", "data.db")
	if err := os.WriteFile(dbPath, []byte("post-downs"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(h.orgDir, ".deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshotPath(h.orgDir), []byte("pre-deploy"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The evict hook runs after the fingerprint capture and before the failed
	// verify — the window a live tenant would use to swap the file.
	evict := func(string) {
		if err := os.Remove(snapshotPath(h.orgDir)); err != nil {
			t.Errorf("swap snapshot: %v", err)
		}
		if err := os.WriteFile(snapshotPath(h.orgDir), []byte("swapped-in"), 0o644); err != nil {
			t.Errorf("swap snapshot: %v", err)
		}
	}
	verify := func(ctx context.Context, slug string) error {
		rec, err := h.cp.App.FindFirstRecordByData("orgs", "slug", slug)
		if err != nil {
			return err
		}
		if rec.GetString("recipe_hash") == hashNew {
			return fmt.Errorf("boot failed")
		}
		return nil
	}
	d := newDeployer(h.cp.App, h.root, &fakeArtifactBuilder{hash: hashNew},
		evict, verify, quietTestLogger())

	if _, err := d.DeployProposed(context.Background(), "acme", nextLock, "job_3"); err != nil {
		t.Fatalf("DeployProposed: %v", err)
	}
	waitUntil(t, "deployment reverted", func() bool {
		return h.lastDeployment(t).GetString("status") == "reverted"
	})

	if db, err := os.ReadFile(dbPath); err != nil || string(db) != "post-downs" {
		t.Fatalf("revert adopted a swapped snapshot: db=%q err=%v", db, err)
	}
}

func TestDeploy_SerializesPerOrgAndRateLimits(t *testing.T) {
	h := newDeployHarness(t)

	release := make(chan struct{})
	verify := func(ctx context.Context, slug string) error {
		<-release
		return nil
	}
	d := newDeployer(h.cp.App, h.root, &fakeArtifactBuilder{hash: hashNew},
		func(string) {}, verify, quietTestLogger())
	d.minInterval = time.Hour

	if _, err := d.Deploy(context.Background(), "acme", nextLock, ""); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	// The org stays busy until its respawn settles.
	if _, err := d.Deploy(context.Background(), "acme", nextLock, ""); err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("concurrent Deploy = %v, want busy refusal", err)
	}
	close(release)
	waitUntil(t, "deploy settled", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return !d.busy["acme"]
	})
	// Settled, but inside the rate-limit floor.
	if _, err := d.Deploy(context.Background(), "acme", nextLock, ""); err == nil || !strings.Contains(err.Error(), "rate-limited") {
		t.Fatalf("rapid re-deploy = %v, want rate-limit refusal", err)
	}
}

func TestDeploy_FailedBuildRecordsAndLeavesOrgUntouched(t *testing.T) {
	h := newDeployHarness(t)
	d := newDeployer(h.cp.App, h.root, &fakeArtifactBuilder{err: fmt.Errorf("peer violation: mail needs core >=2")},
		func(string) {}, nil, quietTestLogger())

	_, err := d.Deploy(context.Background(), "acme", nextLock, "job_7")
	if err == nil || !strings.Contains(err.Error(), "peer violation") {
		t.Fatalf("Deploy = %v, want the build refusal", err)
	}

	org := h.org(t)
	if org.GetString("recipe_hash") != hashOld {
		t.Fatal("org row repointed despite failed build")
	}
	dep := h.lastDeployment(t)
	if dep.GetString("status") != "failed" || dep.GetString("job_id") != "job_7" {
		t.Fatalf("deployment row = status %s job %s", dep.GetString("status"), dep.GetString("job_id"))
	}
	// The org is deployable again immediately — a failed build must not hold
	// the busy slot.
	d.mu.Lock()
	busy := d.busy["acme"]
	d.mu.Unlock()
	if busy {
		t.Fatal("org still busy after failed build")
	}
}

// The control-socket surface: bad bodies refuse, busy/ratelimited map to
// 409/429, and a healthy proposal answers 202 with the recipe hash before the
// eviction lands.
func TestControlHandler_DeployProtocol(t *testing.T) {
	h := newDeployHarness(t)
	d := newDeployer(h.cp.App, h.root, &fakeArtifactBuilder{hash: hashNew},
		func(string) {}, nil, quietTestLogger())
	d.minInterval = time.Hour
	handler := d.Handler("acme")

	post := func(path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		handler.ServeHTTP(rec, req)
		return rec
	}

	if rec := post("/v1/deploy", `{`); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body = %d", rec.Code)
	}
	if rec := post("/v1/deploy", `{"lockfile":{}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty lockfile = %d", rec.Code)
	}
	if rec := post("/v1/deploy", `{"lockfile":{"tinycld":"1.1.0"},"jobId":"../etc"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("hostile jobId = %d", rec.Code)
	}

	rec := post("/v1/build", `{"lockfile":{"tinycld":"1.1.0"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("build = %d: %s", rec.Code, rec.Body)
	}
	var buildResp struct {
		RecipeHash string `json:"recipeHash"`
		Cached     bool   `json:"cached"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &buildResp); err != nil || buildResp.RecipeHash != hashNew {
		t.Fatalf("build response = %s (err=%v)", rec.Body, err)
	}

	rec = post("/v1/deploy", `{"lockfile":{"tinycld":"1.1.0"},"jobId":"job_1"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("deploy = %d: %s", rec.Code, rec.Body)
	}

	waitUntil(t, "deploy settled", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return !d.busy["acme"]
	})
	if rec := post("/v1/deploy", `{"lockfile":{"tinycld":"1.1.0"}}`); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("rate-limited deploy = %d", rec.Code)
	}
}

// The step-4 metadata surface: /v1/resolve, /v1/versions input validation,
// /v1/state, and /v1/build's migrations list.
func TestControlHandler_MetadataSurface(t *testing.T) {
	h := newDeployHarness(t)
	d := newDeployer(h.cp.App, h.root, &fakeArtifactBuilder{hash: hashNew},
		func(string) {}, nil, quietTestLogger())
	d.readMin = 0 // exercise several read endpoints back-to-back
	handler := d.Handler("acme")

	post := func(path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		handler.ServeHTTP(rec, req)
		return rec
	}

	// /v1/resolve returns the builder's trusted-side facts.
	rec := post("/v1/resolve", `{"spec":"@tinycld/todo@1.0.0"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve = %d: %s", rec.Code, rec.Body)
	}
	var resolved builder.ResolvedSpec
	if err := json.Unmarshal(rec.Body.Bytes(), &resolved); err != nil {
		t.Fatal(err)
	}
	if resolved.Slug != "todo" || resolved.Name != "@tinycld/todo" || len(resolved.Migrations) != 1 {
		t.Fatalf("resolved = %+v", resolved)
	}

	// Spec shape is validated before any subprocess sees it.
	if rec := post("/v1/resolve", `{"spec":"; rm -rf /"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("hostile resolve spec = %d", rec.Code)
	}
	if rec := post("/v1/versions", `{"spec":"--flag-smuggle"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("hostile versions spec = %d", rec.Code)
	}

	// /v1/build carries the artifact's migration basenames (the fake's dir has
	// none — an empty, non-null list).
	rec = post("/v1/build", `{"lockfile":{"tinycld":"1.1.0"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("build = %d: %s", rec.Code, rec.Body)
	}
	var buildResp struct {
		Migrations []string `json:"migrations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &buildResp); err != nil || buildResp.Migrations == nil {
		t.Fatalf("build response migrations missing/null: %s (err=%v)", rec.Body, err)
	}

	// /v1/state serves the org row's lockfile + recipe hash.
	stateRec := httptest.NewRecorder()
	handler.ServeHTTP(stateRec, httptest.NewRequest(http.MethodGet, "/v1/state", nil))
	if stateRec.Code != http.StatusOK {
		t.Fatalf("state = %d: %s", stateRec.Code, stateRec.Body)
	}
	var state struct {
		Lockfile   map[string]string `json:"lockfile"`
		RecipeHash string            `json:"recipeHash"`
	}
	if err := json.Unmarshal(stateRec.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.Lockfile["tinycld"] != "1.0.0" || state.RecipeHash != hashOld {
		t.Fatalf("state = %+v", state)
	}
}

// The metadata surface runs its subprocess (`npm pack`, `npm view`,
// `git ls-remote`) inline in the ROUTER process, outside every confinement.
// A git/URL spec passes ValidatePackageSpec but would make `npm pack` run the
// package's prepare scripts as root (RCE) and `git ls-remote` hit an arbitrary
// URL with the router's creds (SSRF). Both /v1/resolve and /v1/versions must
// refuse anything that is not a registry spec.
func TestControlHandler_MetadataSurfaceRefusesNonRegistrySpecs(t *testing.T) {
	h := newDeployHarness(t)
	d := newDeployer(h.cp.App, h.root, &fakeArtifactBuilder{hash: hashNew},
		func(string) {}, nil, quietTestLogger())
	d.readMin = 0 // exercise several read endpoints back-to-back
	handler := d.Handler("acme")

	post := func(path, body string) int {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		return rec.Code
	}

	// Each of these is accepted by ValidatePackageSpec (git/URL forms npm pack
	// understands) but must be refused before the subprocess runs.
	for _, spec := range []string{
		"github:evil/pkg",
		"git+https://internal-host/repo.git",
		"git+ssh://git@internal/repo",
		"https://169.254.169.254/latest/meta-data",
		"git+file:///root/secret",
	} {
		if code := post("/v1/resolve", `{"spec":`+jsonString(spec)+`}`); code != http.StatusBadRequest {
			t.Errorf("/v1/resolve %q = %d, want 400 (non-registry spec must be refused before the subprocess)", spec, code)
		}
		if code := post("/v1/versions", `{"spec":`+jsonString(spec)+`}`); code != http.StatusBadRequest {
			t.Errorf("/v1/versions %q = %d, want 400", spec, code)
		}
	}

	// A registry spec still resolves.
	if code := post("/v1/resolve", `{"spec":"@tinycld/todo@1.0.0"}`); code != http.StatusOK {
		t.Errorf("/v1/resolve registry spec = %d, want 200", code)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// H1: an over-cap lockfile is refused before the builder resolves it, and the
// read-ish endpoints (/v1/resolve here) are per-org rate-limited so a tenant
// cannot hammer the router's inline toolchain.
func TestControlHandler_ReadEndpointsBounded(t *testing.T) {
	h := newDeployHarness(t)
	d := newDeployer(h.cp.App, h.root, &fakeArtifactBuilder{hash: hashNew},
		func(string) {}, nil, quietTestLogger())
	handler := d.Handler("acme")

	post := func(path, body string) int {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		return rec.Code
	}

	// An over-cap lockfile is refused (the first read call in this test, so the
	// rate limiter admits it — the 400 is the size cap, not the throttle).
	var b strings.Builder
	b.WriteString(`{"lockfile":{`)
	for i := 0; i <= maxLockfileEntries; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"pkg%d":"1.0.0"`, i)
	}
	b.WriteString(`}}`)
	if code := post("/v1/build", b.String()); code != http.StatusBadRequest {
		t.Fatalf("over-cap lockfile = %d, want 400", code)
	}

	// Hammering /v1/resolve: the first call in the readMin window is admitted
	// (400 here — the fake resolves fine, but this exercises the throttle, not
	// the resolver), subsequent ones inside the window are throttled to 429.
	d.lastRead["acme"] = time.Time{} // reset the window the build call consumed
	throttled := 0
	for i := 0; i < 20; i++ {
		if post("/v1/resolve", `{"spec":"@tinycld/todo@1.0.0"}`) == http.StatusTooManyRequests {
			throttled++
		}
	}
	if throttled == 0 {
		t.Fatalf("no /v1/resolve call was throttled; the per-org rate limit is not enforced")
	}
}

// One legitimate UI interaction is a same-second sequence of read calls (the
// Packages screen fires a /v1/versions per registry row on mount, and Install
// fires /v1/resolve right behind them). The token bucket must admit that whole
// burst — a strict one-per-second floor failed the install's resolve — while
// still refusing once the burst is exhausted.
func TestBeginRead_AllowsUIBurstThenThrottles(t *testing.T) {
	d := newDeployer(nil, "", nil, nil, nil, nil)
	for i := 0; i < int(d.readBurst); i++ {
		if err := d.beginRead("acme"); err != nil {
			t.Fatalf("burst call %d refused: %v", i, err)
		}
	}
	if err := d.beginRead("acme"); err == nil {
		t.Fatal("burst exhausted but the next call was still admitted; the sustained rate is unbounded")
	}
}

// refsFor rejects an over-cap set directly, before the builder ever runs.
func TestRefsFor_RejectsOverCapLockfile(t *testing.T) {
	lock := make(map[string]string, maxLockfileEntries+1)
	for i := 0; i <= maxLockfileEntries; i++ {
		lock[fmt.Sprintf("pkg%d", i)] = "1.0.0"
	}
	if _, err := refsFor(lock); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("refsFor(over-cap) = %v, want a size-cap refusal", err)
	}
}

// A lockfile value is either a bare version (registry form) or a complete git
// spec. Both must survive refsFor unchanged in the ways the builder relies on:
// the registry form gets the package name concatenated, the git form does not.
func TestRefsFor_GitAndRegistrySpecs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  string
	}{
		{"tinycld", "0.4.0", "tinycld@0.4.0"},
		{"tinycld", "github:tinycld/tinycld#main", "github:tinycld/tinycld#main"},
		{"tinycld", "github:tinycld/tinycld", "github:tinycld/tinycld"},
		{"tinycld", "git+ssh://git@github.com/tinycld/tinycld.git#v0.4.0", "git+ssh://git@github.com/tinycld/tinycld.git#v0.4.0"},
		{"tinycld", "git+https://github.com/tinycld/tinycld.git", "git+https://github.com/tinycld/tinycld.git"},
		{"tinycld", "git+file:///srv/tinycld-mt/tinycld", "git+file:///srv/tinycld-mt/tinycld"},
	} {
		refs, err := refsFor(map[string]string{tc.name: tc.value})
		if err != nil {
			t.Fatalf("refsFor(%q: %q) errored: %v", tc.name, tc.value, err)
		}
		if len(refs) != 1 || refs[0].Spec != tc.want {
			t.Errorf("refsFor(%q: %q) = %+v, want spec %q", tc.name, tc.value, refs, tc.want)
		}
	}
}

// The git-spec passthrough must not become a hole around spec validation: a
// value that looks git-ish but carries an unsafe ref or characters is refused
// here rather than reaching `npm pack`.
func TestRefsFor_RejectsUnsafeSpecs(t *testing.T) {
	for _, value := range []string{
		"github:tinycld/tinycld#--upload-pack=touch /tmp/pwned",
		"github:tinycld/tinycld#$(whoami)",
		"github:tinycld/tinycld#a;b",
		"../../etc/passwd",
		"-S/tmp/evil",
		"0.4.0 --registry=http://evil",
	} {
		if _, err := refsFor(map[string]string{"tinycld": value}); err == nil {
			t.Errorf("refsFor(tinycld: %q) was accepted; want a refusal", value)
		}
	}
}

func TestLiveRecipeHashes_CoversRowsAndInflight(t *testing.T) {
	h := newDeployHarness(t)
	org := h.org(t)
	org.Set("prev_recipe_hash", hashNew)
	if err := h.cp.App.Save(org); err != nil {
		t.Fatal(err)
	}

	d := newDeployer(h.cp.App, h.root, &fakeArtifactBuilder{hash: hashNew}, func(string) {}, nil, quietTestLogger())
	d.trackHash("sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")

	live, err := d.LiveRecipeHashes()
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{hashOld, hashNew, "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"} {
		if !live[h] {
			t.Fatalf("live set missing %s", h)
		}
	}
}

// D7: CreateOrg with no lockfile copies the template and provisions from the
// (cache-hit) artifact. The zero-package escape hatch is gone with the store:
// every org boots from a built artifact, so an explicit empty set is refused
// rather than provisioned bare.
func TestCreateOrg_DefaultTemplateBuildsArtifact(t *testing.T) {
	root := t.TempDir()
	cp, err := New(filepath.Join(root, "pb_control", "pb_data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.App.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { WaitForAppDeploys(cp.App); _ = cp.App.ResetBootstrapState() })
	if err := cpInitForTest(cp); err != nil {
		t.Fatal(err)
	}
	if err := SetDefaultLockfile(cp.App, map[string]string{"tinycld": "1.0.0", "@tinycld/mail": "2.0.0"}); err != nil {
		t.Fatal(err)
	}

	fake := &fakeArtifactBuilder{hash: hashNew}
	p := NewProvisioner(cp.App, root, func(string) {}, nil)
	p.deployer = newDeployer(cp.App, root, fake, func(string) {}, nil, quietTestLogger())
	stubOwnerStep(p)
	// No compiled artifact behind the fake builder's Dir, so the real owner
	// step cannot run; this test is about the default-template build.
	p.createOwnerFn = func(_, _, _, _, _ string) error { return nil }

	rec, _, err := p.CreateOrg("acme", "Acme", nil, OwnerAccount{Email: "owner@example.com"})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if rec.GetString("recipe_hash") != hashNew {
		t.Fatalf("recipe_hash = %q, want the built artifact", rec.GetString("recipe_hash"))
	}
	if !strings.Contains(rec.GetString("lockfile"), "@tinycld/mail") {
		t.Fatalf("lockfile did not copy the template: %s", rec.GetString("lockfile"))
	}
	if fake.calls.Load() != 1 {
		t.Fatalf("builder ran %d times, want 1", fake.calls.Load())
	}

	// An explicit empty map is not buildable (no app shell) — refused, and the
	// builder never runs for it.
	if _, _, err := p.CreateOrg("lean", "Lean", map[string]string{}, OwnerAccount{Email: "owner@example.com"}); err == nil || !strings.Contains(err.Error(), "app shell") {
		t.Fatalf("CreateOrg(lean) = %v, want the app-shell refusal", err)
	}
	if fake.calls.Load() != 1 {
		t.Fatalf("builder ran for a refused empty set")
	}
}

// A base-bearing lockfile on a router with no builder must refuse loudly.
func TestCreateOrg_ArtifactSetWithoutBuilderRefuses(t *testing.T) {
	root := t.TempDir()
	cp, err := New(filepath.Join(root, "pb_control", "pb_data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.App.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { WaitForAppDeploys(cp.App); _ = cp.App.ResetBootstrapState() })
	if err := cpInitForTest(cp); err != nil {
		t.Fatal(err)
	}
	p := NewProvisioner(cp.App, root, func(string) {}, nil)
	if _, _, err := p.CreateOrg("acme", "Acme", map[string]string{"tinycld": "1.0.0"}, OwnerAccount{Email: "owner@example.com"}); err == nil || !strings.Contains(err.Error(), "no builder") {
		t.Fatalf("CreateOrg = %v, want no-builder refusal", err)
	}
}

func TestDefaultLockfile_Roundtrip(t *testing.T) {
	root := t.TempDir()
	cp, err := New(filepath.Join(root, "pb_control", "pb_data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.App.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { WaitForAppDeploys(cp.App); _ = cp.App.ResetBootstrapState() })
	if err := cpInitForTest(cp); err != nil {
		t.Fatal(err)
	}

	lock, err := DefaultLockfile(cp.App)
	if err != nil || lock != nil {
		t.Fatalf("fresh control plane default = %v (err=%v), want nil", lock, err)
	}
	if err := SetDefaultLockfile(cp.App, map[string]string{"tinycld": "1.0.0"}); err != nil {
		t.Fatal(err)
	}
	lock, err = DefaultLockfile(cp.App)
	if err != nil || lock["tinycld"] != "1.0.0" {
		t.Fatalf("default = %v (err=%v)", lock, err)
	}
	// Clearing the template returns new orgs to zero-package provisioning.
	if err := SetDefaultLockfile(cp.App, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	lock, err = DefaultLockfile(cp.App)
	if err != nil || lock != nil {
		t.Fatalf("cleared default = %v (err=%v), want nil", lock, err)
	}
}
