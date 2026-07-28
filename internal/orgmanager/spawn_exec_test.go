package orgmanager

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

// TestBuildCmd_EnvIsAllowlistOnly pins the property the e2e secret test cannot
// observe: the child environment is CONSTRUCTED from an allowlist, never
// derived from the host's. TestTenant_DoesNotInheritHostSecrets reads
// process.env through a hook, but the fork's sandbox empties process.env in
// every VM — so that test stays green even if cmd.Env were swapped for
// os.Environ(). This one goes red: it asserts on the exact env buildCmd hands
// the child, with a host secret set in the parent's environment.
// (The OS-level proof is TestConfinement_ChildEnvironmentHoldsNoHostSecrets,
// which needs Linux + root and runs in the confinement CI workflow.)
func TestBuildCmd_EnvIsAllowlistOnly(t *testing.T) {
	t.Setenv("MT_SUPERUSER_PASSWORD", "super-secret-value")
	t.Setenv("MT_TLS_KEY", "another-secret")

	req := SpawnRequest{
		Slug:       "acme",
		OrgDir:     t.TempDir(),
		SocketPath: "/tmp/acme.sock",
		BinaryPath: "/nonexistent/serve-org",
	}
	cmd := buildCmd(req, slog.New(slog.NewTextHandler(io.Discard, nil)))

	allowed := map[string]bool{"HOME": true, "TMPDIR": true, "PATH": true}
	for _, kv := range cmd.Env {
		key, value, _ := strings.Cut(kv, "=")
		if !allowed[key] {
			t.Errorf("child env carries non-allowlisted variable %q", key)
		}
		if strings.Contains(value, "super-secret-value") || strings.Contains(value, "another-secret") {
			t.Errorf("child env leaks a host secret: %s", kv)
		}
	}
	if len(cmd.Env) != len(allowed) {
		t.Errorf("child env has %d entries, want exactly %d (the allowlist): %v",
			len(cmd.Env), len(allowed), cmd.Env)
	}

	// The allowlisted values must point inside the org dir, not at the host's.
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "HOME=") && !strings.HasPrefix(kv, "HOME="+req.OrgDir) {
			t.Errorf("HOME points outside the org dir: %s", kv)
		}
	}
}
