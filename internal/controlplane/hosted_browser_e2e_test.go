package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	pbcore "github.com/pocketbase/pocketbase/core"

	"tinycld.org/multi-org/internal/builder"
	"tinycld.org/multi-org/internal/materialize"
	"tinycld.org/multi-org/internal/orgerr"
	"tinycld.org/multi-org/internal/server"
)

// TestHostedBrowserE2E is the browser-level acceptance TestHostedDeployE2E's own
// header calls out as missing: Playwright driving a REAL provisioned org over a
// REAL subdomain, rather than the protocol-level coverage that test provides.
//
// It exists because the feature under test — switching between saved servers —
// is about moving between origins, and nothing single-origin can produce that.
// Two orgs on the router are two subdomains, two tenant processes and two
// databases, which is exactly what a user's "two servers" means.
//
// What is REAL here (the point of the test):
//   - the trusted builder produces an actual artifact from the sibling checkouts
//   - both orgs are provisioned properly: orgs rows, materialized org dirs,
//     each with its own pb_data
//   - each tenant is the artifact's own dual-mode binary, spawned as its own
//     process on its own unix socket, through the same readiness handshake the
//     manager uses
//   - requests reach them through frontrouter.New via server.BuildHandler, so
//     subdomain dispatch is the production path
//
// The one deliberate economy: both orgs share ONE artifact. The builder is
// content-addressed by recipe hash, so two orgs with the same package set
// resolve to the same build anyway — building twice would test the cache, not
// the router. Per-org package sets are already covered by TestHostedDeployE2E.
//
// Chrome resolves *.localhost to loopback with no DNS or TLS setup, so the
// front router can serve plain HTTP on 127.0.0.1 and the browser can still
// reach acme.localhost / globex.localhost.
//
//	RUN_HOSTED_BROWSER_E2E=1 go test ./internal/controlplane/ \
//	  -run TestHostedBrowserE2E -v -timeout 60m
//
// Env: TINYCLD_WS_ROOT (default ../../..), BUILDER_E2E_PNPM_STORE (a warm pnpm
// store — strongly recommended).
func TestHostedBrowserE2E(t *testing.T) {
	if os.Getenv("RUN_HOSTED_BROWSER_E2E") != "1" {
		t.Skip("set RUN_HOSTED_BROWSER_E2E=1 to run the hosted browser e2e (a real build + two tenants; needs node/pnpm/npm/go + playwright)")
	}
	if testing.Short() {
		t.Skip("skipping in -short")
	}

	wsRoot := os.Getenv("TINYCLD_WS_ROOT")
	if wsRoot == "" {
		abs, err := filepath.Abs(filepath.Join("..", "..", ".."))
		if err != nil {
			t.Fatal(err)
		}
		wsRoot = abs
	}
	for _, member := range []string{"tinycld"} {
		if _, err := os.Stat(filepath.Join(wsRoot, member, "package.json")); err != nil {
			t.Fatalf("workspace member %q not checked out under %s: %v", member, wsRoot, err)
		}
	}

	// ---- local npm registry over the sibling checkouts ----
	reg := startLocalNpmRegistry(t, filepath.Join(wsRoot, "tinycld"))
	baseVer := reg.version("tinycld")
	t.Logf("local registry: tinycld@%s at %s", baseVer, reg.srv.URL)

	// ---- control plane + real builder ----
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

	bld, err := builder.New(builder.Config{
		Root:         root,
		ScaffoldRoot: wsRoot,
		PnpmStoreDir: os.Getenv("BUILDER_E2E_PNPM_STORE"),
		NpmRegistry:  reg.srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	// ---- one artifact, shared by both orgs (see the header) ----
	lock := map[string]string{"tinycld": baseVer}
	deployer := newDeployer(cp.App, root, bld, func(string) {}, nil, slog.Default())
	deployer.minInterval = 0
	built, err := deployer.BuildSet(context.Background(), lock)
	if err != nil {
		t.Fatalf("build artifact: %v", err)
	}
	t.Logf("artifact: %s (cached=%v)", built.RecipeHash, built.Cached)

	// Two identities per org: a PocketBase superuser (to create records over the
	// API) and the app user the browser actually signs in as.
	const adminEmail, adminPass = "browser-e2e-admin@example.com", "BrowserE2E1234!"
	const appUserEmail, appUserPass = "browser-e2e@example.com", "BrowserE2E1234!"
	slugs := []string{"acme", "globex"}
	tenants := make(map[string]*tenantHarness, len(slugs))

	for _, slug := range slugs {
		orgDir := filepath.Join(root, "pb_orgs", slug)
		if err := os.MkdirAll(filepath.Join(orgDir, "pb_data"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := materialize.MaterializeBuild(orgDir, built.Dir); err != nil {
			t.Fatalf("materialize %s: %v", slug, err)
		}

		lfBytes, _ := json.Marshal(lock)
		orgsCol, err := cp.App.FindCollectionByNameOrId("orgs")
		if err != nil {
			t.Fatal(err)
		}
		rec := pbcore.NewRecord(orgsCol)
		rec.Set("slug", slug)
		rec.Set("display_name", slug)
		rec.Set("status", "active")
		rec.Set("data_dir", filepath.Join("pb_orgs", slug))
		rec.Set("lockfile", string(lfBytes))
		rec.Set("recipe_hash", built.RecipeHash)
		if err := cp.App.Save(rec); err != nil {
			t.Fatalf("provision %s: %v", slug, err)
		}

		// Each org gets its own superuser inside its OWN database — the
		// separation the switcher's per-origin sessions depend on.
		upsert := exec.Command(filepath.Join(built.Dir, "tinycld"),
			"superuser", "upsert", adminEmail, adminPass,
			"--dir", filepath.Join(orgDir, "pb_data"),
		)
		upsert.Dir = orgDir
		if out, err := upsert.CombinedOutput(); err != nil {
			t.Fatalf("superuser upsert for %s: %v\n%s", slug, err, out)
		}

		sockDir, err := os.MkdirTemp("", "browser-e2e-"+slug+"-*")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(sockDir) })

		h := &tenantHarness{
			t:            t,
			orgDir:       orgDir,
			buildsDir:    filepath.Join(root, "builds"),
			sock:         filepath.Join(sockDir, "t.sock"),
			ctlSock:      filepath.Join(sockDir, "ctl.sock"),
			resolveBuild: func() (string, error) { return built.RecipeHash, nil },
		}

		ctlLn, err := net.Listen("unix", h.ctlSock)
		if err != nil {
			t.Fatal(err)
		}
		ctlSrv := &http.Server{Handler: deployer.Handler(slug)}
		go func() { _ = ctlSrv.Serve(ctlLn) }()
		t.Cleanup(func() { _ = ctlSrv.Close() })

		if err := h.verify(context.Background(), slug); err != nil {
			t.Fatalf("tenant boot for %s: %v", slug, err)
		}
		t.Cleanup(h.stop)
		tenants[slug] = h

		// The superuser above is a PocketBase admin; the APP's login form
		// authenticates against the `users` collection, which is a different
		// auth collection entirely. Create the user the browser will sign in as,
		// in THIS tenant's own database — the per-org separation the switcher's
		// session isolation depends on.
		createAppUser(t, h.httpClient(), adminEmail, adminPass, appUserEmail, appUserPass)
	}

	// ---- the REAL front router, over TCP so a browser can reach it ----
	handler := server.BuildHandler(server.Params{
		BaseDomain: "localhost",
		ControlPlaneMux: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "control plane not fronted in this test", http.StatusNotImplemented)
		}),
		GetOrg: func(_ context.Context, slug string) (http.Handler, error) {
			h, ok := tenants[slug]
			if !ok {
				return nil, orgerr.ErrOrgNotFound
			}
			return tenantProxy(h.sock), nil
		},
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	port := ln.Addr().(*net.TCPAddr).Port
	base := fmt.Sprintf("http://acme.localhost:%d", port)
	t.Logf("router listening on :%d — %s", port, base)

	waitForOrg(t, fmt.Sprintf("http://acme.localhost:%d/api/health", port))
	waitForOrg(t, fmt.Sprintf("http://globex.localhost:%d/api/health", port))

	// ---- drive the browser against the running stack ----
	runPlaywright(t, wsRoot, port, appUserEmail, appUserPass)
}

// createAppUser adds the user the BROWSER signs in as. The superuser created
// via the artifact binary is a PocketBase admin and cannot log into the app —
// the login form authenticates against the `users` collection.
//
// `role` is required on the create (1940000000_backfill_and_require_users_role)
// and `verified` must be true or the session gates to a verification screen.
func createAppUser(t *testing.T, client *http.Client, suEmail, suPass, email, pass string) {
	t.Helper()
	token := authSuperuser(t, client, suEmail, suPass)

	body, _ := json.Marshal(map[string]any{
		// `username` is required alongside email — the seed script sets both.
		"username":        "browsere2e",
		"email":           email,
		"password":        pass,
		"passwordConfirm": pass,
		"name":            "Browser E2E",
		"emailVisibility": true,
		"verified":        true,
		"role":            "owner",
	})
	req, err := http.NewRequest(http.MethodPost,
		"http://tenant/api/collections/users/records", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	// Fail on ANY non-200. Tolerating 400 as "probably already exists" hid a
	// real validation error (a missing required field) behind a plausible
	// excuse, and the test then failed much later in the browser with a
	// generic "Failed to authenticate". Each tenant DB here is freshly
	// provisioned, so a duplicate is not a legitimate outcome.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create app user = %d: %s", resp.StatusCode, payload)
	}
}

// tenantProxy reverse-proxies to a tenant's unix socket, the same way
// orgmanager's instance handler does.
func tenantProxy(sock string) http.Handler {
	target, _ := url.Parse("http://tenant")
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = target.Host
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
	return proxy
}

func waitForOrg(t *testing.T, healthURL string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		resp, err := http.Get(healthURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s never became healthy", healthURL)
}

// runPlaywright shells out to the app shell's multi-org spec, pointing it at the
// already-running stack. The specs live in the tinycld repo because that is
// where the UI they assert on lives; this test owns the SERVER they run against.
func runPlaywright(t *testing.T, wsRoot string, port int, appUserEmail, appUserPass string) {
	t.Helper()
	appDir := filepath.Join(wsRoot, "tinycld")

	cmd := exec.Command("pnpm", "exec", "playwright", "test",
		"--config", "playwright.multi-org.config.ts")
	cmd.Dir = appDir
	cmd.Env = append(os.Environ(),
		// Point the config at this stack instead of letting it start its own.
		fmt.Sprintf("E2E_MULTI_ORG_PORT=%d", port),
		"E2E_MULTI_ORG_EXTERNAL=1",
		fmt.Sprintf("E2E_MULTI_ORG_EMAIL=%s", appUserEmail),
		fmt.Sprintf("E2E_MULTI_ORG_PASSWORD=%s", appUserPass),
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	t.Log(out.String())
	if err != nil {
		t.Fatalf("playwright: %v", err)
	}
}
