package builder

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"tinycld.org/core/pkgbuild"
)

// Test fixtures: fakePack writes extracted package trees + a fake tarball per
// spec, so resolve runs without npm or the network. Manifest evaluation is the
// real sobek evaluator — the same engine production uses.

var testToolchain = pkgbuild.Toolchain{Go: "go1.26.3", Node: "v22.12.0", Pnpm: "pnpm@11.3.0+sha512.f00d"}

// fixture describes one fake package a spec resolves to.
type fixture struct {
	base    bool
	slug    string
	name    string
	version string
	peers   map[string]string
}

func fakePackFor(t *testing.T, fixtures map[string]fixture) packFn {
	t.Helper()
	return func(spec, workDir string) (string, string, error) {
		fx, ok := fixtures[spec]
		if !ok {
			return "", "", fmt.Errorf("fake pack: unknown spec %q", spec)
		}
		dir := filepath.Join(workDir, "package")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", "", err
		}
		if fx.base {
			corePkg := fmt.Sprintf(`{"name": "@tinycld/core", "version": %q}`+"\n", fx.version)
			if err := os.MkdirAll(filepath.Join(dir, "core"), 0o755); err != nil {
				return "", "", err
			}
			if err := os.WriteFile(filepath.Join(dir, "core", "package.json"), []byte(corePkg), 0o644); err != nil {
				return "", "", err
			}
		} else {
			// The manifest's name is a DISPLAY string; the npm identity comes
			// from package.json. Write them differently so any code that reads
			// the wrong one fails these tests.
			manifest := fmt.Sprintf("const manifest = {\n    name: %q,\n    slug: %q,\n    version: %q,\n", "Display "+fx.slug, fx.slug, fx.version)
			if len(fx.peers) > 0 {
				manifest += "    peerVersions: {"
				for k, v := range fx.peers {
					manifest += fmt.Sprintf(" %q: %q,", k, v)
				}
				manifest += " },\n"
			}
			manifest += "}\nexport default manifest\n"
			if err := os.WriteFile(filepath.Join(dir, "manifest.ts"), []byte(manifest), 0o644); err != nil {
				return "", "", err
			}
			pkgJSON := fmt.Sprintf(`{"name": %q, "version": %q}`+"\n", fx.name, fx.version)
			if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0o644); err != nil {
				return "", "", err
			}
		}
		// The tarball's bytes are what integrity hashes — make them unique per
		// spec so each member's integrity is distinct and deterministic.
		tgz := filepath.Join(workDir, "fake.tgz")
		if err := os.WriteFile(tgz, []byte("tarball:"+spec), 0o644); err != nil {
			return "", "", err
		}
		return dir, tgz, nil
	}
}

func writeScaffoldRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	pins := `{"//": "test pins", "uniwind": "1.8.0"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, pkgbuild.OverridesFile), []byte(pins), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// fakeRunner satisfies Runner, recording invocations and staging a minimal
// artifact tree.
type fakeRunner struct {
	mu       sync.Mutex
	calls    int32
	inFlight int32
	maxSeen  int32
	specs    []JobSpec
	fail     bool
	block    chan struct{} // non-nil: Run waits until closed
}

func (r *fakeRunner) Run(_ context.Context, spec JobSpec, _ pkgbuild.ProgressSink) error {
	atomic.AddInt32(&r.calls, 1)
	cur := atomic.AddInt32(&r.inFlight, 1)
	defer atomic.AddInt32(&r.inFlight, -1)
	for {
		prev := atomic.LoadInt32(&r.maxSeen)
		if cur <= prev || atomic.CompareAndSwapInt32(&r.maxSeen, prev, cur) {
			break
		}
	}
	if r.block != nil {
		<-r.block
	}
	r.mu.Lock()
	r.specs = append(r.specs, spec)
	r.mu.Unlock()
	if r.fail {
		return fmt.Errorf("boom")
	}
	return os.MkdirAll(spec.ArtifactDir, 0o755)
}

func testBuilder(t *testing.T, fixtures map[string]fixture, runner Runner) *Builder {
	t.Helper()
	b, err := New(Config{
		Root:         t.TempDir(),
		ScaffoldRoot: writeScaffoldRoot(t),
		Toolchain:    testToolchain,
		Runner:       runner,
		pack:         fakePackFor(t, fixtures),
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var defaultFixtures = map[string]fixture{
	"tinycld@0.4.0":       {base: true, version: "0.0.9"},
	"@tinycld/mail@0.3.1": {slug: "mail", name: "@tinycld/mail", version: "0.3.1", peers: map[string]string{"@tinycld/core": ">=0.0.4 <0.1.0"}},
}

var defaultRefs = []PackageRef{{Spec: "tinycld@0.4.0"}, {Spec: "@tinycld/mail@0.3.1"}}

func TestBuild_ProducesCommittedArtifact(t *testing.T) {
	runner := &fakeRunner{}
	b := testBuilder(t, defaultFixtures, runner)

	res, err := b.Build(context.Background(), defaultRefs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Cached {
		t.Fatal("first build reported Cached")
	}
	if ok, _ := b.Store().Has(res.RecipeHash); !ok {
		t.Fatal("no committed artifact after Build")
	}

	recipe, err := b.Store().ReadRecipe(res.RecipeHash)
	if err != nil {
		t.Fatal(err)
	}
	if recipe.RecipeHash != res.RecipeHash || recipe.Toolchain != testToolchain {
		t.Fatalf("recipe = %+v", recipe)
	}
	slugs := map[string]pkgbuild.ResolvedMember{}
	for _, m := range recipe.Members {
		slugs[m.Slug] = m
	}
	if base := slugs["tinycld"]; base.Name != "@tinycld/core" || base.Version != "0.0.9" || base.Integrity == "" {
		t.Fatalf("base member = %+v", base)
	}
	if mail := slugs["mail"]; mail.Name != "@tinycld/mail" || mail.Version != "0.3.1" || mail.Integrity == "" {
		t.Fatalf("mail member = %+v", mail)
	}

	// The job saw parent-computed facts: pre-fetched dirs and integrities for
	// every member, and the deterministic hash-derived build id.
	spec := runner.specs[0]
	if len(spec.MemberDirs) != 2 || spec.Integrities["mail"] != slugs["mail"].Integrity {
		t.Fatalf("job spec = %+v", spec)
	}
	if spec.BuildID != buildIDFor(res.RecipeHash) {
		t.Fatalf("BuildID = %q", spec.BuildID)
	}

	// The recipe hash matches an independent computation over the same facts —
	// the value an org's own honest resolution would arrive at.
	want, err := pkgbuild.RecipeHash(recipe.Members, map[string]string{"uniwind": "1.8.0"}, testToolchain)
	if err != nil {
		t.Fatal(err)
	}
	if res.RecipeHash != want {
		t.Fatalf("RecipeHash = %q, want %q", res.RecipeHash, want)
	}

	// Each feature member's evaluated manifest is staged parent-side into
	// manifests/<slug>/manifest.json; the base ships none.
	raw, err := os.ReadFile(filepath.Join(res.Dir, MemberManifestsDir, "mail", "manifest.json"))
	if err != nil {
		t.Fatalf("staged mail manifest: %v", err)
	}
	var mf struct {
		Slug         string            `json:"slug"`
		PeerVersions map[string]string `json:"peerVersions"`
	}
	if err := json.Unmarshal(raw, &mf); err != nil {
		t.Fatal(err)
	}
	if mf.Slug != "mail" || mf.PeerVersions["@tinycld/core"] == "" {
		t.Fatalf("staged mail manifest = %s", raw)
	}
	if _, err := os.Stat(filepath.Join(res.Dir, MemberManifestsDir, "tinycld")); !os.IsNotExist(err) {
		t.Fatalf("base member staged a manifest (err=%v)", err)
	}
}

func TestBuild_SecondCallHitsCache(t *testing.T) {
	runner := &fakeRunner{}
	b := testBuilder(t, defaultFixtures, runner)

	first, err := b.Build(context.Background(), defaultRefs, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Same set, different order — the recipe is order-independent.
	again, err := b.Build(context.Background(), []PackageRef{{Spec: "@tinycld/mail@0.3.1"}, {Spec: "tinycld@0.4.0"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Cached || again.RecipeHash != first.RecipeHash || again.Dir != first.Dir {
		t.Fatalf("second build = %+v, first = %+v", again, first)
	}
	if got := atomic.LoadInt32(&runner.calls); got != 1 {
		t.Fatalf("runner ran %d times, want 1", got)
	}
}

func TestBuild_ConcurrentSameSetSharesOneBuild(t *testing.T) {
	runner := &fakeRunner{}
	b := testBuilder(t, defaultFixtures, runner)

	const n = 4
	var wg sync.WaitGroup
	results := make([]Result, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = b.Build(context.Background(), defaultRefs, nil)
		}()
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if results[i].RecipeHash != results[0].RecipeHash {
			t.Fatalf("hash mismatch across concurrent builds")
		}
	}
	if got := atomic.LoadInt32(&runner.calls); got != 1 {
		t.Fatalf("runner ran %d times, want 1 (singleflight)", got)
	}
}

func TestBuild_ConcurrencyCapQueuesJobs(t *testing.T) {
	moreFixtures := map[string]fixture{}
	for k, v := range defaultFixtures {
		moreFixtures[k] = v
	}
	moreFixtures["@tinycld/drive@1.0.0"] = fixture{slug: "drive", name: "@tinycld/drive", version: "1.0.0"}

	runner := &fakeRunner{block: make(chan struct{})}
	b := testBuilder(t, moreFixtures, runner)

	var wg sync.WaitGroup
	for _, refs := range [][]PackageRef{
		defaultRefs,
		{{Spec: "tinycld@0.4.0"}, {Spec: "@tinycld/drive@1.0.0"}},
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := b.Build(context.Background(), refs, nil); err != nil {
				t.Error(err)
			}
		}()
	}
	// Both builds are in flight; with MaxConcurrent=1 only one job may run at
	// once. Wait until one job has started, then release both.
	for atomic.LoadInt32(&runner.calls) == 0 {
		runtime.Gosched()
	}
	close(runner.block)
	wg.Wait()
	if max := atomic.LoadInt32(&runner.maxSeen); max != 1 {
		t.Fatalf("max concurrent jobs = %d, want 1", max)
	}
	if got := atomic.LoadInt32(&runner.calls); got != 2 {
		t.Fatalf("runner ran %d times, want 2", got)
	}
}

func TestBuild_FailedJobKeepsNothingInStore(t *testing.T) {
	runner := &fakeRunner{fail: true}
	b := testBuilder(t, defaultFixtures, runner)

	_, err := b.Build(context.Background(), defaultRefs, nil)
	if err == nil {
		t.Fatal("build succeeded despite failing runner")
	}
	entries, listErr := b.Store().List()
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(entries) != 0 {
		t.Fatalf("store has %d entries after failed build", len(entries))
	}

	// The failure is not sticky: a later build of the same set runs again.
	runner.fail = false
	if _, err := b.Build(context.Background(), defaultRefs, nil); err != nil {
		t.Fatal(err)
	}
}

func TestBuild_RefusesPeerViolationBeforeRunning(t *testing.T) {
	fixtures := map[string]fixture{
		"tinycld@0.4.0":       defaultFixtures["tinycld@0.4.0"],
		"@tinycld/mail@9.9.9": {slug: "mail", name: "@tinycld/mail", version: "9.9.9", peers: map[string]string{"@tinycld/core": ">=1.0.0"}},
	}
	runner := &fakeRunner{}
	b := testBuilder(t, fixtures, runner)

	_, err := b.Build(context.Background(), []PackageRef{{Spec: "tinycld@0.4.0"}, {Spec: "@tinycld/mail@9.9.9"}}, nil)
	if err == nil {
		t.Fatal("peer-violating set built")
	}
	if got := atomic.LoadInt32(&runner.calls); got != 0 {
		t.Fatalf("runner ran %d times for a refused set", got)
	}
}
