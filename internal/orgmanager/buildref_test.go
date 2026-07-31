package orgmanager

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tinycld.org/multi-org/internal/lockfile"
)

// fakeArtifact lays out a minimal committed build artifact: the runtime trees,
// a dual-mode binary stand-in, and one feature member's staged manifest.
func fakeArtifact(t *testing.T) string {
	t.Helper()
	buildsRoot := filepath.Join(t.TempDir(), "builds")
	dir := filepath.Join(buildsRoot, "abc123")
	for _, name := range []string{"pb_hooks", "pb_public", "pb_migrations"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "pb_public", "index.html"), []byte("<html>artifact</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tinycld"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifestDir := filepath.Join(dir, "manifests", "mail")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"slug":"mail","name":"@tinycld/mail","version":"9.9.9"}`
	if err := os.WriteFile(filepath.Join(manifestDir, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// slugsFromManifests is a minimal stand-in for controlplane.PackageSlugs: it
// reads each resolved package's manifest.json — which for an artifact-backed
// org resolves into the artifact's manifests/<slug>/ dir.
func slugsFromManifests(resolved []lockfile.ResolvedPackage) ([]string, error) {
	var slugs []string
	for _, pkg := range resolved {
		raw, err := os.ReadFile(filepath.Join(pkg.Dir, "manifest.json"))
		if err != nil {
			return nil, err
		}
		var mc struct {
			Slug string `json:"slug"`
		}
		if err := json.Unmarshal(raw, &mc); err != nil {
			return nil, err
		}
		slugs = append(slugs, mc.Slug)
	}
	return slugs, nil
}

func newArtifactManager(t *testing.T, sp *fakeSpawner, artifactDir string) *OrgManager {
	t.Helper()
	mgr := New(Config{
		Root:         t.TempDir(),
		Spawner:      sp,
		Logger:       quietLogger(),
		HooksPool:    2,
		PackageSlugs: slugsFromManifests,
		ResolveBuild: func(recipeHash string) (BuildRef, error) {
			if recipeHash != "sha256:abc123" {
				t.Fatalf("ResolveBuild called with %q", recipeHash)
			}
			return BuildRef{
				Dir:    artifactDir,
				Binary: filepath.Join(artifactDir, "tinycld"),
				Packages: []lockfile.ResolvedPackage{{
					Name:    "@tinycld/mail",
					Version: "9.9.9",
					Dir:     filepath.Join(artifactDir, "manifests", "mail"),
				}},
			}, nil
		},
		LookupOrg: stubLookup(map[string]OrgRecord{
			"acme": {Slug: "acme", Status: "active", RecipeHash: "sha256:abc123"},
		}),
	})
	t.Cleanup(mgr.Shutdown)
	return mgr
}

// An artifact-backed org boots its build's own binary, its live trees point
// into the artifact, confinement covers the builds root, and the capability
// hooks read the artifact's staged manifests.
func TestGet_ArtifactBackedOrgSpawnsFromBuild(t *testing.T) {
	sp := &fakeSpawner{}
	artifactDir := fakeArtifact(t)
	mgr := newArtifactManager(t, sp, artifactDir)

	if _, err := mgr.Get(context.Background(), "acme"); err != nil {
		t.Fatalf("Get(acme): %v", err)
	}

	req := sp.lastReq()
	if req.BinaryPath != filepath.Join(artifactDir, "tinycld") {
		t.Fatalf("BinaryPath = %q, want the artifact's binary", req.BinaryPath)
	}
	if req.PackagesDir != filepath.Dir(artifactDir) {
		t.Fatalf("PackagesDir = %q, want the builds root %q", req.PackagesDir, filepath.Dir(artifactDir))
	}

	// Live trees are symlinks into the artifact.
	orgDir := req.OrgDir
	idx, err := os.ReadFile(filepath.Join(orgDir, "pb_public", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if string(idx) != "<html>artifact</html>" {
		t.Fatalf("pb_public serves %q, want the artifact's tree", idx)
	}

	// The package-slug capability resolved through the artifact's staged
	// manifest — and "mail" present means the mail sockets were assigned.
	var pkgs struct {
		Slugs []string `json:"slugs"`
	}
	raw, err := os.ReadFile(req.PackagesConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &pkgs); err != nil {
		t.Fatal(err)
	}
	if len(pkgs.Slugs) != 1 || pkgs.Slugs[0] != "mail" {
		t.Fatalf("packages.json slugs = %v, want [mail]", pkgs.Slugs)
	}
	if req.IMAPSocketPath == "" {
		t.Fatal("mail member did not produce mail sockets")
	}
}

// An artifact-backed org whose manager has no build resolver must fail its
// load loudly, not fall back to the store path.
func TestGet_ArtifactBackedOrgWithoutResolverFails(t *testing.T) {
	sp := &fakeSpawner{}
	mgr := New(Config{
		Root:      t.TempDir(),
		Spawner:   sp,
		Logger:    quietLogger(),
		HooksPool: 2,
		LookupOrg: stubLookup(map[string]OrgRecord{
			"acme": {Slug: "acme", Status: "active", RecipeHash: "sha256:abc123"},
		}),
	})
	t.Cleanup(mgr.Shutdown)

	if _, err := mgr.Get(context.Background(), "acme"); err == nil {
		t.Fatal("Get succeeded with no ResolveBuild wired")
	}
	if sp.spawnCount() != 0 {
		t.Fatalf("spawned %d times, want 0", sp.spawnCount())
	}
}

// The control socket is router-bound before the child starts, dialable while
// the instance lives, passed to the tenant, and removed at teardown.
func TestControlSocket_BoundServedAndTornDown(t *testing.T) {
	sp := &fakeSpawner{}
	artifactDir := fakeArtifact(t)
	mgr := newArtifactManager(t, sp, artifactDir)
	mgr.cfg.Control = func(slug string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte("proposal from " + slug))
		})
	}

	if _, err := mgr.Get(context.Background(), "acme"); err != nil {
		t.Fatalf("Get(acme): %v", err)
	}
	ctlPath := sp.lastReq().ControlSocketPath
	if ctlPath == "" {
		t.Fatal("no control socket path handed to the tenant")
	}

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", ctlPath)
		},
	}}
	resp, err := client.Post("http://control/v1/deploy", "application/json", nil)
	if err != nil {
		t.Fatalf("dial control socket: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("control response = %d, want 202", resp.StatusCode)
	}

	mgr.Evict("acme")
	if !waitFor(5*time.Second, func() bool {
		_, err := os.Lstat(ctlPath)
		return os.IsNotExist(err)
	}) {
		t.Fatal("control socket file survived teardown")
	}
}

// L2: with a Control handler wired but the spawner NOT confining tenants (a
// non-root host, unset uid window, or the darwin dev fallback), every tenant
// runs as the same host user, so the 0700 socket-dir identity that
// authenticates the ctl.sock collapses. The manager must refuse to bind the
// control socket at all — degraded mode has no tenant-proposed deploys.
func TestControlSocket_NotBoundWhenUnconfined(t *testing.T) {
	controlHandler := func(slug string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
		})
	}

	// Confined spawner: the control socket is bound and handed to the tenant.
	confined := &fakeSpawner{}
	mgr := newArtifactManager(t, confined, fakeArtifact(t))
	mgr.cfg.Control = controlHandler
	if _, err := mgr.Get(context.Background(), "acme"); err != nil {
		t.Fatalf("Get(acme) confined: %v", err)
	}
	if confined.lastReq().ControlSocketPath == "" {
		t.Fatal("confined spawner: no control socket bound despite a Control handler")
	}

	// Unconfined spawner: same Control handler, but no socket is bound and the
	// tenant is handed no control path.
	unconfined := &fakeSpawner{unconfined: true}
	mgr2 := newArtifactManager(t, unconfined, fakeArtifact(t))
	mgr2.cfg.Control = controlHandler
	if _, err := mgr2.Get(context.Background(), "acme"); err != nil {
		t.Fatalf("Get(acme) unconfined: %v", err)
	}
	if got := unconfined.lastReq().ControlSocketPath; got != "" {
		t.Fatalf("unconfined spawner bound a control socket (%q); degraded mode must not accept tenant-proposed deploys", got)
	}
}
