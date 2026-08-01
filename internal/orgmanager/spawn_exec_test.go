package orgmanager

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
		BinaryPath: "/nonexistent/tinycld",
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

// TMPDIR must exist before the tenant starts. Go's os.TempDir() returns $TMPDIR
// without checking it, so a missing directory is not an error the tenant can
// see — it surfaces as ENOENT from os.CreateTemp deep inside PocketBase's
// multipart handling, i.e. an opaque 500 on every upload past the 16MB
// in-memory threshold, on every org.
func TestEnsureTenantDirs_CreatesTmpdir(t *testing.T) {
	orgDir := t.TempDir()

	if err := ensureTenantDirs(orgDir); err != nil {
		t.Fatalf("ensureTenantDirs: %v", err)
	}

	req := SpawnRequest{
		Slug:       "acme",
		OrgDir:     orgDir,
		SocketPath: "/tmp/acme.sock",
		BinaryPath: "/nonexistent/tinycld",
	}
	cmd := buildCmd(req, slog.New(slog.NewTextHandler(io.Discard, nil)))

	var tmpdir string
	for _, kv := range cmd.Env {
		if v, ok := strings.CutPrefix(kv, "TMPDIR="); ok {
			tmpdir = v
		}
	}
	if tmpdir == "" {
		t.Fatal("child env carries no TMPDIR")
	}

	st, err := os.Stat(tmpdir)
	if err != nil {
		t.Fatalf("TMPDIR %s does not exist: every tenant upload over 16MB fails "+
			"with an opaque 500: %v", tmpdir, err)
	}
	if !st.IsDir() {
		t.Fatalf("TMPDIR %s is not a directory", tmpdir)
	}
}

// The tenant writes its own temp files, so the directory has to be writable by
// the tenant uid — but nothing else should be able to read what is spooled
// there (uploads in flight, rereadable request bodies).
func TestEnsureTenantDirs_TmpdirIsPrivate(t *testing.T) {
	orgDir := t.TempDir()
	if err := ensureTenantDirs(orgDir); err != nil {
		t.Fatal(err)
	}

	st, err := os.Stat(filepath.Join(orgDir, "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("tenant tmp dir is group/other-accessible (%o); in-flight uploads "+
			"must not be readable outside the tenant", perm)
	}
}

// Re-running on an existing org must not fail or reset the directory: it runs
// on every cold start, not just at provisioning.
func TestEnsureTenantDirs_Idempotent(t *testing.T) {
	orgDir := t.TempDir()
	if err := ensureTenantDirs(orgDir); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(orgDir, "tmp", "in-flight-upload")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureTenantDirs(orgDir); err != nil {
		t.Fatalf("second call failed: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("existing tmp contents were disturbed: %v", err)
	}
}

// TestPipeToLog_RateLimitsFloods pins M5: a tenant flooding stdout gets its
// lines dropped past the token bucket, with the drop count surfaced, instead
// of saturating the host log sink line-for-line.
func TestPipeToLog_RateLimitsFloods(t *testing.T) {
	var forwarded, droppedSummaries int
	var droppedTotal int64
	h := slogHandlerFunc(func(rec slog.Record) {
		switch rec.Message {
		case "tenant output":
			forwarded++
		case "tenant output rate-limited; lines dropped":
			droppedSummaries++
			rec.Attrs(func(a slog.Attr) bool {
				if a.Key == "dropped" {
					droppedTotal += a.Value.Int64()
				}
				return true
			})
		}
	})

	const total = 500
	input := strings.Repeat("spam line\n", total)
	// Effectively no refill during the test; only the burst of 10 is forwarded.
	pipeToLogRate(strings.NewReader(input), slog.New(h), "acme", slog.LevelInfo, 0.0001, 10)

	if forwarded != 10 {
		t.Errorf("forwarded %d lines, want the burst of 10", forwarded)
	}
	if droppedSummaries == 0 {
		t.Error("expected a dropped-lines summary record")
	}
	if got := int64(total) - int64(forwarded); droppedTotal != got {
		t.Errorf("summaries account for %d dropped lines, want %d", droppedTotal, got)
	}
}

// slogHandlerFunc adapts a func to slog.Handler for test capture.
type slogHandlerFunc func(slog.Record)

func (f slogHandlerFunc) Enabled(context.Context, slog.Level) bool { return true }
func (f slogHandlerFunc) Handle(_ context.Context, r slog.Record) error {
	f(r)
	return nil
}
func (f slogHandlerFunc) WithAttrs([]slog.Attr) slog.Handler { return f }
func (f slogHandlerFunc) WithGroup(string) slog.Handler      { return f }
