package controlplane

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	pbcore "github.com/pocketbase/pocketbase/core"

	"tinycld.org/core/pkgbuild"
	"tinycld.org/core/tenantcfg"
	"tinycld.org/multi-org/internal/builder"
	"tinycld.org/multi-org/internal/materialize"
)

// TestHostedDeployE2E is §7 step 4's Go-level acceptance: the full hosted
// package-agency loop against REAL builds and a REAL tenant process.
//
//  1. a local npm registry serves the sibling checkouts (tinycld base +
//     contacts) so lockfile specs resolve without publishing;
//  2. the real builder produces the base+contacts artifact; an org is
//     provisioned from it (org row + materialized org dir);
//  3. the artifact's dual-mode binary boots as the tenant, with the per-org
//     control socket served by a real Deployer;
//  4. the tenant's boot reconcile mirrors the built-in set into pkg_registry;
//  5. an org admin drives the HOSTED Packages API over the tenant's socket:
//     POST /api/admin/packages/uninstall contacts → warm build (base-only
//     artifact) → snapshot → in-tenant DOWNs → deploy proposal → the router
//     evicts, respawns from the new artifact, and commits;
//  6. the respawned tenant's reconcile finalizes the install-log row
//     (job-status → success) and deletes the contacts registry row; the
//     contacts schema is gone (the DOWN ran).
//
// Two full builds (pnpm install + go build + expo export), so it is opt-in:
//
//	RUN_HOSTED_E2E=1 go test ./internal/controlplane/ -run TestHostedDeployE2E -v -timeout 60m
//
// Env: TINYCLD_WS_ROOT (default ../../..), BUILDER_E2E_PNPM_STORE (reuse a
// warm pnpm store — strongly recommended: `pnpm store path`).
//
// The browser-level acceptance (todo-install.spec.ts verbatim against an org
// subdomain) additionally needs an npm-published todo fixture or a
// registry-fronting runner; this test covers the same machinery at the
// protocol level until that lands.
func TestHostedDeployE2E(t *testing.T) {
	if os.Getenv("RUN_HOSTED_E2E") != "1" {
		t.Skip("set RUN_HOSTED_E2E=1 to run the hosted deploy e2e (two real builds; needs node/pnpm/npm/go)")
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
	for _, member := range []string{"tinycld", "contacts"} {
		if _, err := os.Stat(filepath.Join(wsRoot, member, "package.json")); err != nil {
			t.Fatalf("workspace member %q not checked out under %s: %v", member, wsRoot, err)
		}
	}

	// ---- local npm registry over the sibling checkouts ----
	reg := startLocalNpmRegistry(t,
		filepath.Join(wsRoot, "tinycld"),
		filepath.Join(wsRoot, "contacts"),
	)
	baseVer := reg.version("tinycld")
	contactsVer := reg.version("@tinycld/contacts")
	t.Logf("local registry: tinycld@%s, @tinycld/contacts@%s at %s", baseVer, contactsVer, reg.srv.URL)

	// ---- control plane + real builder + deployer ----
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

	const slug = "acme"
	orgDir := filepath.Join(root, "pb_orgs", slug)
	tenant := &tenantHarness{t: t, orgDir: orgDir, buildsDir: filepath.Join(root, "builds")}

	deployer := newDeployer(cp.App, root, bld, tenant.evict, tenant.verify, slog.Default())
	deployer.minInterval = 0
	tenant.resolveBuild = func() (string, error) {
		rec, err := cp.App.FindFirstRecordByData("orgs", "slug", slug)
		if err != nil {
			return "", err
		}
		return rec.GetString("recipe_hash"), nil
	}

	// ---- build the org's initial artifact (base + contacts) ----
	lockA := map[string]string{"tinycld": baseVer, "@tinycld/contacts": contactsVer}
	resA, err := deployer.BuildSet(context.Background(), lockA)
	if err != nil {
		t.Fatalf("initial build: %v", err)
	}
	t.Logf("initial artifact: %s (cached=%v)", resA.RecipeHash, resA.Cached)

	// ---- provision the org: row + materialized dir ----
	if err := os.MkdirAll(filepath.Join(orgDir, "pb_data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := materialize.MaterializeBuild(orgDir, resA.Dir); err != nil {
		t.Fatal(err)
	}
	lfBytes, _ := json.Marshal(lockA)
	orgsCol, err := cp.App.FindCollectionByNameOrId("orgs")
	if err != nil {
		t.Fatal(err)
	}
	orgRec := pbcore.NewRecord(orgsCol)
	orgRec.Set("slug", slug)
	orgRec.Set("display_name", "Acme")
	orgRec.Set("status", "active")
	orgRec.Set("data_dir", filepath.Join("pb_orgs", slug))
	orgRec.Set("lockfile", string(lfBytes))
	orgRec.Set("recipe_hash", resA.RecipeHash)
	if err := cp.App.Save(orgRec); err != nil {
		t.Fatal(err)
	}

	// ---- org admin: a PB superuser inside the tenant's own DB, created by
	// the artifact binary's host mode before the first tenant boot ----
	const adminEmail, adminPass = "hosted-e2e@example.com", "HostedE2E1234!"
	upsert := exec.Command(filepath.Join(resA.Dir, "tinycld"),
		"superuser", "upsert", adminEmail, adminPass,
		"--dir", filepath.Join(orgDir, "pb_data"),
	)
	upsert.Dir = orgDir
	if out, err := upsert.CombinedOutput(); err != nil {
		t.Fatalf("superuser upsert via artifact binary: %v\n%s", err, out)
	}

	// ---- control socket: a real Deployer.Handler on a unix socket ----
	sockDir, err := os.MkdirTemp("", "hosted-e2e-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	tenant.sock = filepath.Join(sockDir, "t.sock")
	tenant.ctlSock = filepath.Join(sockDir, "ctl.sock")

	ctlLn, err := net.Listen("unix", tenant.ctlSock)
	if err != nil {
		t.Fatal(err)
	}
	ctlSrv := &http.Server{Handler: deployer.Handler(slug)}
	go func() { _ = ctlSrv.Serve(ctlLn) }()
	t.Cleanup(func() { _ = ctlSrv.Close() })

	// ---- first tenant boot (the same spawn the verify hook uses) ----
	if err := tenant.verify(context.Background(), slug); err != nil {
		t.Fatalf("initial tenant boot: %v", err)
	}
	t.Cleanup(tenant.stop)

	client := tenant.httpClient()
	token := authSuperuser(t, client, adminEmail, adminPass)

	// The boot reconcile mirrored the artifact's built-in set into
	// pkg_registry: core (bundled) + contacts (installed, uninstallable).
	regRows := listRegistryRows(t, client, token)
	if regRows["core"] != "bundled" || regRows["contacts"] != "installed" {
		t.Fatalf("registry after boot reconcile = %v, want core:bundled + contacts:installed", regRows)
	}
	// The contacts schema is live (its migrations ran on the boot).
	if !collectionExists(t, client, token, "contacts") {
		t.Fatal("contacts collection should exist before the uninstall")
	}

	// ---- the hosted uninstall, driven exactly like the Packages UI ----
	uninstallBody := bytes.NewReader([]byte(`{"slug":"contacts"}`))
	req, _ := http.NewRequest(http.MethodPost, "http://tenant/api/admin/packages/uninstall", uninstallBody)
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var accepted struct {
		JobID string `json:"jobId"`
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("uninstall = %d: %s", resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, &accepted); err != nil || accepted.JobID == "" {
		t.Fatalf("uninstall response: %s (%v)", body, err)
	}
	t.Logf("hosted uninstall accepted: job %s (base-only build follows)", accepted.JobID)

	// The pipeline warm-builds the base-only artifact (minutes), runs the
	// contacts DOWNs in-tenant, proposes, and the deployer evicts + respawns.
	// Wait for the deploy to settle: the deployer clears its busy slot after
	// the commit/revert decision.
	waitFor(t, 45*time.Minute, "deploy settled", func() bool {
		deployer.mu.Lock()
		defer deployer.mu.Unlock()
		return len(deployer.busy) == 0
	})

	// The respawned tenant serves; its boot reconcile finalized the job row.
	waitFor(t, 2*time.Minute, "job-status success", func() bool {
		st, err := jobStatus(client, token, accepted.JobID)
		if err != nil {
			return false
		}
		if st == "rolled_back" || st == "failed" {
			t.Fatalf("deploy did not commit: job status %q", st)
		}
		return st == "success"
	})

	// Registry: contacts row DELETED (committed uninstall), core intact.
	regRows = listRegistryRows(t, client, token)
	if _, still := regRows["contacts"]; still {
		t.Fatalf("contacts registry row must be deleted after a committed uninstall: %v", regRows)
	}
	if regRows["core"] != "bundled" {
		t.Fatalf("core row must survive: %v", regRows)
	}

	// Schema: the contacts DOWN migrations ran before the proposal.
	if collectionExists(t, client, token, "contacts") {
		t.Fatal("contacts collection should be dropped by the in-tenant DOWN migrations")
	}

	// Control plane: org row repointed to the base-only artifact; the
	// deployment row committed; the snapshot consumed.
	orgRec, err = cp.App.FindFirstRecordByData("orgs", "slug", slug)
	if err != nil {
		t.Fatal(err)
	}
	var lockNow map[string]string
	if err := json.Unmarshal([]byte(orgRec.GetString("lockfile")), &lockNow); err != nil {
		t.Fatal(err)
	}
	if _, still := lockNow["@tinycld/contacts"]; still || lockNow["tinycld"] != baseVer {
		t.Fatalf("org lockfile after deploy = %v", lockNow)
	}
	if orgRec.GetString("prev_recipe_hash") != resA.RecipeHash {
		t.Fatalf("prev_recipe_hash should hold the revert target, got %q", orgRec.GetString("prev_recipe_hash"))
	}
	dep, err := cp.App.FindFirstRecordByFilter("deployments", "status = 'committed'", nil)
	if err != nil || dep == nil {
		t.Fatalf("no committed deployments row: %v", err)
	}
	if dep.GetString("job_id") != accepted.JobID {
		t.Fatalf("deployment job_id = %q, want %s", dep.GetString("job_id"), accepted.JobID)
	}
	if _, err := os.Stat(filepath.Join(orgDir, ".deploy", "backup.db")); !os.IsNotExist(err) {
		t.Fatal("committed deploy must drop the pre-deploy snapshot")
	}
	if _, err := os.Stat(tenantcfg.DeployResultPath(orgDir)); !os.IsNotExist(err) {
		t.Fatal("the respawned tenant must consume deploy-result.json")
	}
}

// ---------- tenant process harness ----------

// tenantHarness owns the (single) tenant child process across deploys: the
// deployer's evict hook kills it, the verify hook respawns it from the org
// row's current build — the same division of labor as the org manager.
type tenantHarness struct {
	t         *testing.T
	orgDir    string
	buildsDir string
	sock      string
	ctlSock   string

	resolveBuild func() (string, error)

	mu  sync.Mutex
	cmd *exec.Cmd
}

func (h *tenantHarness) evict(string) {
	h.stop()
}

func (h *tenantHarness) stop() {
	h.mu.Lock()
	cmd := h.cmd
	h.cmd = nil
	h.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
}

// verify spawns the tenant from the org's CURRENT build and waits for the
// readiness handshake — the deployer's commit/revert decision.
func (h *tenantHarness) verify(ctx context.Context, slug string) error {
	hash, err := h.resolveBuild()
	if err != nil {
		return err
	}
	hexPart := strings.TrimPrefix(hash, "sha256:")
	artifactDir := filepath.Join(h.buildsDir, hexPart)
	binary := filepath.Join(artifactDir, "tinycld")
	if _, err := os.Stat(binary); err != nil {
		return fmt.Errorf("artifact binary for %s: %w", hash, err)
	}

	// Rematerialize hooks/migrations/public from the (possibly new) artifact,
	// as the manager's load path does before spawning.
	if err := materialize.MaterializeBuild(h.orgDir, artifactDir); err != nil {
		return fmt.Errorf("materialize %s: %w", hash, err)
	}

	// The router owns the socket file lifecycle: clear a stale one pre-bind.
	_ = os.Remove(h.sock)

	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		return err
	}
	defer readEnd.Close()

	cmd := exec.Command(binary,
		"--org-dir", h.orgDir,
		"--socket", h.sock,
		"--ready-fd", "3",
		"--slug", slug,
		"--control-socket", h.ctlSock,
	)
	cmd.Dir = h.orgDir
	cmd.ExtraFiles = []*os.File{writeEnd}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		writeEnd.Close()
		return err
	}
	writeEnd.Close()

	lineCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(readEnd)
		if sc.Scan() {
			lineCh <- sc.Text()
		} else {
			lineCh <- ""
		}
	}()
	var ready struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	select {
	case line := <-lineCh:
		if line == "" {
			return fmt.Errorf("tenant exited before readiness")
		}
		if err := json.Unmarshal([]byte(line), &ready); err != nil {
			return fmt.Errorf("ready line %q: %v", line, err)
		}
	case <-time.After(3 * time.Minute):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return fmt.Errorf("tenant readiness timeout")
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return ctx.Err()
	}
	if !ready.OK {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return fmt.Errorf("tenant not ready: %s", ready.Error)
	}

	h.mu.Lock()
	h.cmd = cmd
	h.mu.Unlock()
	return nil
}

func (h *tenantHarness) httpClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", h.sock)
			},
			// Deploys kill the tenant; a pooled conn to the dead process would
			// error the first post-respawn request.
			DisableKeepAlives: true,
		},
		Timeout: 60 * time.Second,
	}
}

// ---------- tenant HTTP helpers ----------

func authSuperuser(t *testing.T, client *http.Client, email, pass string) string {
	t.Helper()
	body := fmt.Sprintf(`{"identity":%q,"password":%q}`, email, pass)
	resp, err := client.Post("http://tenant/api/collections/_superusers/auth-with-password",
		"application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("superuser auth = %d: %s", resp.StatusCode, payload)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(payload, &out); err != nil || out.Token == "" {
		t.Fatalf("auth payload: %s", payload)
	}
	return out.Token
}

func tenantGET(client *http.Client, token, path string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, "http://tenant"+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", token)
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, payload, nil
}

func listRegistryRows(t *testing.T, client *http.Client, token string) map[string]string {
	t.Helper()
	code, payload, err := tenantGET(client, token, "/api/collections/pkg_registry/records?perPage=50")
	if err != nil || code != http.StatusOK {
		t.Fatalf("list pkg_registry = %d (%v): %s", code, err, payload)
	}
	var list struct {
		Items []struct {
			Slug   string `json:"slug"`
			Status string `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(payload, &list); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, item := range list.Items {
		out[item.Slug] = item.Status
	}
	return out
}

func collectionExists(t *testing.T, client *http.Client, token, name string) bool {
	t.Helper()
	code, payload, err := tenantGET(client, token, "/api/collections/"+name)
	if err != nil {
		t.Fatal(err)
	}
	switch code {
	case http.StatusOK:
		return true
	case http.StatusNotFound:
		return false
	default:
		t.Fatalf("collection %s = %d: %s", name, code, payload)
		return false
	}
}

func jobStatus(client *http.Client, token, jobID string) (string, error) {
	code, payload, err := tenantGET(client, token, "/api/admin/packages/job-status/"+jobID)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("job-status = %d: %s", code, payload)
	}
	var out struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return "", err
	}
	return out.Status, nil
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ---------- local npm registry ----------

// localNpmRegistry serves just enough of the npm registry protocol for
// `npm pack <name>@<version>`: a packument per package and its tarball.
type localNpmRegistry struct {
	srv      *httptest.Server
	packages map[string]registryPackage // by npm name
}

type registryPackage struct {
	name, version string
	tarball       []byte
	shasum        string
	integrity     string
}

func (r *localNpmRegistry) version(name string) string {
	return r.packages[name].version
}

// startLocalNpmRegistry packs each checkout dir with the REAL `npm pack` and
// serves the results. Versions come from each package.json.
func startLocalNpmRegistry(t *testing.T, dirs ...string) *localNpmRegistry {
	t.Helper()
	reg := &localNpmRegistry{packages: map[string]registryPackage{}}

	packDir := t.TempDir()
	for _, dir := range dirs {
		name := pkgbuild.PackageJSONName(filepath.Join(dir, "package.json"))
		version := pkgbuild.PackageJSONVersion(filepath.Join(dir, "package.json"))
		if name == "" || version == "" {
			t.Fatalf("unreadable package.json in %s", dir)
		}
		// The produced filename is stdout's only line; npm's "notice" chatter
		// goes to stderr, so don't use CombinedOutput here.
		out, err := exec.Command("npm", "pack", dir, "--pack-destination", packDir).Output()
		if err != nil {
			t.Fatalf("npm pack %s: %v", dir, err)
		}
		lines := strings.Fields(strings.TrimSpace(string(out)))
		tgzName := lines[len(lines)-1]
		tarball, err := os.ReadFile(filepath.Join(packDir, tgzName))
		if err != nil {
			t.Fatal(err)
		}
		sha1sum := sha1.Sum(tarball)
		sha512sum := sha512.Sum512(tarball)
		reg.packages[name] = registryPackage{
			name:      name,
			version:   version,
			tarball:   tarball,
			shasum:    hex.EncodeToString(sha1sum[:]),
			integrity: "sha512-" + base64.StdEncoding.EncodeToString(sha512sum[:]),
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		// Scoped names arrive escaped (@scope%2Fname) or plain (@scope/name).
		if unescaped, err := unescapePath(path); err == nil {
			path = unescaped
		}
		if strings.HasPrefix(path, "tarballs/") {
			name := strings.TrimSuffix(strings.TrimPrefix(path, "tarballs/"), ".tgz")
			pkg, ok := reg.packages[name]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(pkg.tarball)
			return
		}
		pkg, ok := reg.packages[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		manifest := map[string]any{
			"name":    pkg.name,
			"version": pkg.version,
			"dist": map[string]any{
				"tarball":   reg.srv.URL + "/tarballs/" + pkg.name + ".tgz",
				"shasum":    pkg.shasum,
				"integrity": pkg.integrity,
			},
		}
		packument := map[string]any{
			"name":      pkg.name,
			"dist-tags": map[string]string{"latest": pkg.version},
			"versions":  map[string]any{pkg.version: manifest},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(packument)
	})
	reg.srv = httptest.NewServer(mux)
	t.Cleanup(reg.srv.Close)
	return reg
}

func unescapePath(p string) (string, error) {
	return url.PathUnescape(p)
}
