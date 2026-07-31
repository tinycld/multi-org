package builder

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"tinycld.org/core/pkgbuild"
)

// TestBuilderE2E_BuildsArtifactAndTenantBoots is step 2's acceptance check
// (DESIGN-org-package-agency §7): drive a REAL build — npm pack of the sibling
// checkouts, pnpm install, the generator, go build, expo export — through the
// builder, then boot a tenant from the committed artifact's dual-mode binary
// and health-check it over the unix socket.
//
// It needs the full JS toolchain and takes minutes, so it is opt-in:
//
//	RUN_BUILDER_E2E=1 go test ./internal/builder/ -run TestBuilderE2E -v -timeout 45m
//
// Env:
//
//	TINYCLD_WS_ROOT       workspace root holding the sibling checkouts
//	                      (default: parent of this repo) — also the scaffold source
//	BUILDER_E2E_WITH      comma-separated feature slugs to include beside the
//	                      base (default: mail,contacts,calendar,drive,calc,text —
//	                      the default set; set to "none" for a lean-shell build)
//	BUILDER_E2E_PNPM_STORE reuse an existing pnpm store (default: a cold one
//	                      under the test root; pass `pnpm store path` for speed)
func TestBuilderE2E_BuildsArtifactAndTenantBoots(t *testing.T) {
	if os.Getenv("RUN_BUILDER_E2E") != "1" {
		t.Skip("set RUN_BUILDER_E2E=1 to run the full builder e2e (needs node/pnpm/npm/go, takes minutes)")
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
	if _, err := os.Stat(filepath.Join(wsRoot, "tinycld", "core", "package.json")); err != nil {
		t.Fatalf("TINYCLD_WS_ROOT %q does not hold the tinycld shell checkout: %v", wsRoot, err)
	}

	refs := []PackageRef{{Spec: filepath.Join(wsRoot, "tinycld")}}
	with := os.Getenv("BUILDER_E2E_WITH")
	if with == "" {
		with = "mail,contacts,calendar,drive,calc,text"
	}
	if with != "none" {
		for _, slug := range strings.Split(with, ",") {
			slug = strings.TrimSpace(slug)
			dir := filepath.Join(wsRoot, slug)
			if _, err := os.Stat(filepath.Join(dir, "manifest.ts")); err != nil {
				t.Fatalf("feature %q is not checked out at %s: %v", slug, dir, err)
			}
			refs = append(refs, PackageRef{Spec: dir})
		}
	}

	root := t.TempDir()
	cfg := Config{
		Root:          root,
		ScaffoldRoot:  wsRoot,
		PnpmStoreDir:  os.Getenv("BUILDER_E2E_PNPM_STORE"),
		AllowDirSpecs: true,
	}
	b, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	res, err := b.Build(context.Background(), refs, testLogSink{t})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Logf("built %s -> %s (cached=%v)", res.RecipeHash, res.Dir, res.Cached)

	for _, rel := range []string{"tinycld", "pb_hooks", "pb_migrations", filepath.Join("pb_public", "app.html"), RecipeFile} {
		if _, err := os.Stat(filepath.Join(res.Dir, rel)); err != nil {
			t.Fatalf("artifact missing %s: %v", rel, err)
		}
	}

	bootTenantFromArtifact(t, res.Dir)
}

// bootTenantFromArtifact assembles a minimal org dir the way the router's
// materialize step would (hooks + migrations from the artifact, empty
// pb_data), spawns the artifact's dual-mode binary in tenant mode over the
// standard tenant flag contract, waits for the ready handshake, and
// health-checks over the unix socket.
func bootTenantFromArtifact(t *testing.T, artifactDir string) {
	t.Helper()
	orgDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(orgDir, "pb_data"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pb_hooks", "pb_migrations"} {
		if err := pkgbuild.CopyDir(filepath.Join(artifactDir, name), filepath.Join(orgDir, name)); err != nil {
			t.Fatal(err)
		}
	}

	sockDir, err := os.MkdirTemp("", "builder-e2e-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "t.sock")

	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readEnd.Close()

	cmd := exec.Command(filepath.Join(artifactDir, "tinycld"),
		"--org-dir", orgDir,
		"--socket", sock,
		"--ready-fd", "3",
		"--slug", "e2e",
	)
	cmd.Dir = orgDir
	cmd.ExtraFiles = []*os.File{writeEnd}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	writeEnd.Close()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	})

	// One JSON line on the ready pipe — the same handshake the router awaits.
	type readyMsg struct {
		OK    bool   `json:"ok"`
		PID   int    `json:"pid"`
		Error string `json:"error"`
	}
	lineCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(readEnd)
		if sc.Scan() {
			lineCh <- sc.Text()
		} else {
			lineCh <- ""
		}
	}()
	var ready readyMsg
	select {
	case line := <-lineCh:
		if line == "" {
			t.Fatal("tenant exited before reporting readiness")
		}
		if err := json.Unmarshal([]byte(line), &ready); err != nil {
			t.Fatalf("ready line %q: %v", line, err)
		}
	case <-time.After(120 * time.Second):
		t.Fatal("tenant did not report readiness in 120s")
	}
	if !ready.OK {
		t.Fatalf("tenant reported not ready: %s", ready.Error)
	}

	client := http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
		Timeout: 10 * time.Second,
	}
	resp, err := client.Get("http://tenant/api/health")
	if err != nil {
		t.Fatalf("health check over %s: %v", sock, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health = %d", resp.StatusCode)
	}
	t.Logf("tenant booted from artifact and serves /api/health on %s", sock)
}

// testLogSink streams build progress into the test log so a long e2e is
// visibly alive.
type testLogSink struct{ t *testing.T }

func (s testLogSink) Progress(step string, percent int, message string) {
	s.t.Logf("[%3d%%] %s: %s", percent, step, message)
}

func (s testLogSink) Logf(format string, args ...any) {
	s.t.Log(fmt.Sprintf(format, args...))
}
