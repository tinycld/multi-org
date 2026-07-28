// Package testsupport holds helpers shared by test files across the router's
// packages. Production code must not import it.
package testsupport

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

var (
	tenantBinOnce sync.Once
	tenantBinPath string
	tenantBinErr  error
)

// BuildTenantBinary compiles cmd/serve-org once per test run and returns its
// path. Tests that exec the real tenant binary (the e2e and confinement
// suites, and controlplane's CardDAV integration) share the one build.
// Skipped under -short.
func BuildTenantBinary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping real-binary test in short mode")
	}

	tenantBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "serve-org-bin")
		if err != nil {
			tenantBinErr = err
			return
		}
		out := filepath.Join(dir, "serve-org")
		cmd := exec.Command("go", "build", "-o", out, "tinycld.org/multi-org/cmd/serve-org")
		if combined, err := cmd.CombinedOutput(); err != nil {
			tenantBinErr = fmt.Errorf("build serve-org: %v\n%s", err, combined)
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
