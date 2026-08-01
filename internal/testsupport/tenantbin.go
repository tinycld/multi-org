// Package testsupport holds helpers shared by test files across the router's
// packages. Production code must not import it.
package testsupport

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

var (
	tenantBinOnce sync.Once
	tenantBinPath string
	tenantBinErr  error
)

// BuildTenantBinary compiles the app shell's dual-mode binary once per test run
// and returns its path. That binary — tinycld.org/tinycld's `main`, which
// dispatches to tenant mode on --org-dir — is the REAL production tenant
// binary; a per-org build artifact places the same binary at <artifact>/tinycld.
// The router's own module can no longer compile a tenant binary of its own
// (the pinned-menu serve-org stand-in is gone), so the real-binary tests (the
// e2e and confinement suites, and controlplane's CardDAV integration) build the
// app shell from the sibling `../tinycld/server` checkout, which the confinement
// CI workflow already clones alongside this repo. Skipped under -short.
func BuildTenantBinary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping real-binary test in short mode")
	}

	tenantBinOnce.Do(func() {
		serverDir, err := appShellServerDir()
		if err != nil {
			tenantBinErr = err
			return
		}
		dir, err := os.MkdirTemp("", "tenant-bin")
		if err != nil {
			tenantBinErr = err
			return
		}
		// Production's BuildRef.Binary is <artifact>/tinycld; the artifact
		// fixtures link this build in under that name, so keep the basename.
		out := filepath.Join(dir, "tinycld")
		// cmd.Dir = the app shell's server dir so its go.work resolves the core
		// and feature members; `go build -o <out> .` builds that package's main.
		cmd := exec.Command("go", "build", "-o", out, ".")
		cmd.Dir = serverDir
		// Strip -mod from any inherited GOFLAGS. serverDir is in workspace
		// mode (its go.work is what resolves core + the feature members), and
		// Go rejects -mod=mod there outright — so a caller that legitimately
		// sets it for THIS module (the CI confinement step runs as root with
		// GOFLAGS=-mod=mod against a read-only module cache) would otherwise
		// break the nested build it has no say over.
		cmd.Env = append(os.Environ(), "GOFLAGS="+stripModFlag(os.Getenv("GOFLAGS")))
		if combined, err := cmd.CombinedOutput(); err != nil {
			tenantBinErr = fmt.Errorf("build app-shell tenant binary in %s: %v\n%s", serverDir, err, combined)
			return
		}
		// MkdirTemp creates the dir 0700. The confinement tests exec this
		// binary as a tenant uid, which needs traversal on the dir and read
		// on the binary.
		if err := os.Chmod(dir, 0o755); err != nil {
			tenantBinErr = err
			return
		}
		if err := os.Chmod(out, 0o755); err != nil {
			tenantBinErr = err
			return
		}
		tenantBinPath = out
	})
	if tenantBinErr != nil {
		t.Fatal(tenantBinErr)
	}
	return tenantBinPath
}

// stripModFlag removes any -mod=... entry from a GOFLAGS value, preserving the
// rest (GOMODCACHE-adjacent flags, -buildvcs, …). GOFLAGS is space-separated.
func stripModFlag(goflags string) string {
	kept := make([]string, 0, 4)
	for _, f := range strings.Fields(goflags) {
		if f == "-mod" || strings.HasPrefix(f, "-mod=") {
			continue
		}
		kept = append(kept, f)
	}
	return strings.Join(kept, " ")
}

// appShellServerDir resolves the sibling app-shell server checkout
// (../tinycld/server relative to the router repo root) independently of the
// test's working directory. This file lives at internal/testsupport, so the
// repo root is two levels up from its own directory; the sibling checkout sits
// beside the repo root. A missing checkout fails loudly with the resolved path
// rather than letting `go build` emit an opaque error.
func appShellServerDir() (string, error) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot resolve testsupport source path")
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(self)))
	serverDir := filepath.Join(repoRoot, "..", "tinycld", "server")
	if _, err := os.Stat(filepath.Join(serverDir, "main.go")); err != nil {
		return "", fmt.Errorf("app-shell server checkout not found at %s (the confinement workflow clones the tinycld sibling): %w", serverDir, err)
	}
	return serverDir, nil
}
