package builder

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tinycld.org/core/pkgbuild"
)

// The child is this test binary re-exec'd (the same helper-process pattern as
// manifesteval), so these tests exercise the real spawn ⇄ stdio protocol
// without building serve-multi or needing toolchains: the helper runs a fake
// job body selected by BUILDER_JOB_HELPER.
func TestMain(m *testing.M) {
	switch os.Getenv("BUILDER_JOB_HELPER") {
	case "ok":
		os.Exit(ServeJobStdio(func(_ context.Context, spec JobSpec, sink pkgbuild.ProgressSink) error {
			sink.Progress("Installing dependencies", 45, "pnpm install")
			sink.Logf("member %s: pretending", "tinycld")
			fmt.Println("stray non-JSON output from some build tool")
			return os.MkdirAll(spec.ArtifactDir, 0o755)
		}))
	case "fail":
		os.Exit(ServeJobStdio(func(context.Context, JobSpec, pkgbuild.ProgressSink) error {
			return fmt.Errorf("pnpm install: exit status 1")
		}))
	case "hang":
		os.Exit(ServeJobStdio(func(context.Context, JobSpec, pkgbuild.ProgressSink) error {
			time.Sleep(time.Hour)
			return nil
		}))
	case "die":
		fmt.Fprintln(os.Stderr, "OOM-killed, say")
		os.Exit(137)
	case "uid":
		// Reports the identity the child observes — the confinement tests'
		// discriminators: pid 1 proves CLONE_NEWPID, uid 0 with files
		// host-owned by the builder uid proves the single-uid userns mapping.
		os.Exit(ServeJobStdio(func(_ context.Context, spec JobSpec, sink pkgbuild.ProgressSink) error {
			sink.Logf("identity pid=%d uid=%d", os.Getpid(), os.Getuid())
			if err := os.MkdirAll(spec.ArtifactDir, 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(spec.ArtifactDir, "marker"), []byte("x"), 0o644)
		}))
	}
	os.Exit(m.Run())
}

// recordingSink captures sink events for assertions.
type recordingSink struct {
	progress []string
	logs     []string
}

func (s *recordingSink) Progress(step string, percent int, message string) {
	s.progress = append(s.progress, fmt.Sprintf("%s@%d: %s", step, percent, message))
}

func (s *recordingSink) Logf(format string, args ...any) {
	s.logs = append(s.logs, fmt.Sprintf(format, args...))
}

func helperSpec(t *testing.T) JobSpec {
	t.Helper()
	// t.TempDir() is 0700 and, under `go test` as root, sits beneath a
	// likewise-private parent. A confined job runs as an unprivileged builder
	// uid and cannot traverse either, so it fails to create its own build dir.
	// Widen traversal on the whole chain — the dirs hold only fixture data.
	base := t.TempDir()
	for p := base; p != "/" && p != "."; p = filepath.Dir(p) {
		if err := os.Chmod(p, 0o711); err != nil {
			break // outside our temp root; nothing further to widen
		}
	}
	jobDir := filepath.Join(base, "job")
	if err := os.MkdirAll(jobDir, 0o777); err != nil {
		t.Fatal(err)
	}
	return JobSpec{
		BuildID:     "recipe-test",
		BuildDir:    filepath.Join(jobDir, "build"),
		ArtifactDir: filepath.Join(jobDir, "artifact"),
		// Production always has a store dir (Builder defaults it from Root).
		// The confined path chowns it to the job uid, and MkdirAll("") fails
		// with a bare `mkdir :` — so an unset one breaks confinement setup
		// before the child ever starts.
		PnpmStoreDir: filepath.Join(base, "pnpm-store"),
	}
}

func helperRunner(t *testing.T, mode string) SubprocessRunner {
	t.Helper()
	// The job re-execs this test binary. Under `sudo go test` it lives in
	// root's private build cache (0700 dirs, 0700 file), which an
	// unprivileged builder uid cannot exec — so make the binary and its
	// parent chain traversable. Best effort: unconfined runs never need it.
	self := os.Args[0]
	_ = os.Chmod(self, 0o755)
	for p := filepath.Dir(self); p != "/" && p != "."; p = filepath.Dir(p) {
		if err := os.Chmod(p, 0o711); err != nil {
			break
		}
	}
	return SubprocessRunner{
		Argv:     []string{self, "-test.run=TestMain"},
		extraEnv: []string{"BUILDER_JOB_HELPER=" + mode},
	}
}

func TestSubprocessRunner_StreamsProgressAndSucceeds(t *testing.T) {
	spec := helperSpec(t)
	sink := &recordingSink{}
	if err := helperRunner(t, "ok").Run(context.Background(), spec, sink); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(spec.ArtifactDir); err != nil {
		t.Fatalf("child did not stage artifact dir: %v", err)
	}
	if len(sink.progress) != 1 || sink.progress[0] != "Installing dependencies@45: pnpm install" {
		t.Fatalf("progress = %v", sink.progress)
	}
	// Both the child's Logf line and its stray non-JSON stdout arrive as logs.
	wantLogs := map[string]bool{}
	for _, l := range sink.logs {
		wantLogs[l] = true
	}
	if !wantLogs["member tinycld: pretending"] || !wantLogs["stray non-JSON output from some build tool"] {
		t.Fatalf("logs = %v", sink.logs)
	}
}

func TestSubprocessRunner_SurfacesJobFailure(t *testing.T) {
	err := helperRunner(t, "fail").Run(context.Background(), helperSpec(t), nil)
	if err == nil || !contains(err.Error(), "pnpm install: exit status 1") {
		t.Fatalf("err = %v, want the job's own failure surfaced", err)
	}
}

func TestSubprocessRunner_SurfacesChildDeath(t *testing.T) {
	err := helperRunner(t, "die").Run(context.Background(), helperSpec(t), nil)
	if err == nil || !contains(err.Error(), "OOM-killed") {
		t.Fatalf("err = %v, want the child's stderr surfaced", err)
	}
}

func TestSubprocessRunner_TimeoutKillsTheJob(t *testing.T) {
	r := helperRunner(t, "hang")
	r.Timeout = 200 * time.Millisecond
	start := time.Now()
	err := r.Run(context.Background(), helperSpec(t), nil)
	if err == nil || !contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("timeout took %s — the process group was not killed", elapsed)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
