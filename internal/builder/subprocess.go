package builder

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"tinycld.org/core/pkgbuild"
	"tinycld.org/multi-org/internal/cgrouplimits"
)

// Subcommand is the hidden argv[1] serve-multi dispatches to ServeJobStdio —
// the same re-exec pattern as manifesteval's subcommand: the child must not
// boot a control plane just to run a build.
const Subcommand = "builder-job"

// DefaultJobTimeout bounds one whole build job. Novel recipes legitimately
// take minutes (pnpm install + Metro export + go build); a job past this is
// wedged, and the org it was for keeps serving its old build regardless.
const DefaultJobTimeout = 45 * time.Minute

// jobLine is the child → parent stdout protocol: one JSON object per line.
// Progress and log lines stream into the parent's ProgressSink; exactly one
// result line ends the run. Non-JSON stdout lines are tolerated as logs — a
// build executes arbitrary package tooling, and a stray print must not break
// the protocol.
type jobLine struct {
	Type    string `json:"type"` // "progress" | "log" | "result"
	Step    string `json:"step,omitempty"`
	Percent int    `json:"percent,omitempty"`
	Message string `json:"message,omitempty"`
	OK      bool   `json:"ok,omitempty"`
	Error   string `json:"error,omitempty"`
}

// SubprocessRunner executes each job in a re-exec'd child process (argv is
// typically {selfExe, Subcommand}), confined on Linux when the host is
// configured for it (see JobConfinement). This is the production Runner: the
// build runs package-author code by design — pnpm lifecycle scripts, Metro,
// member Go compilation — so it gets the same treatment as a tenant: its own
// uid, mount and PID namespaces, and a cgroup, and it never runs in the
// router's own process.
type SubprocessRunner struct {
	// Argv launches the child; element 0 is the binary. Required.
	Argv []string

	// Confinement configures the Linux job jail; the zero value (and any
	// non-Linux host) runs the child unconfined, mirroring the tenant
	// spawner's degraded mode.
	Confinement JobConfinement

	// Timeout bounds one job; zero means DefaultJobTimeout.
	Timeout time.Duration

	// Log receives confinement warnings; nil means slog.Default().
	Log *slog.Logger

	// extraEnv rides on top of the scrubbed child environment. Test seam only
	// (the helper-process pattern dispatches on an env var); production code
	// never sets it — the scrubbed env IS the contract.
	extraEnv []string
}

// JobConfinement mirrors orgmanager.LinuxConfinement for build jobs, with one
// deliberate difference: a single dedicated uid rather than a per-entity
// window. Jobs are serialized by the builder's concurrency cap and each runs
// in a fresh job dir inside fresh namespaces; what the uid boundary protects
// is every TENANT (and the router) from build-executed code, not one build
// from a later one — successive builds already share the pnpm store and npm
// cache as a deliberate reuse choice, exactly like World A's baked store.
type JobConfinement struct {
	// UID is the host uid build jobs run as (0 disables uid separation).
	// Must be outside the tenant uid window.
	UID int

	// CgroupRoot is the cgroup v2 directory to place jobs under; empty
	// disables cgroup placement.
	CgroupRoot string

	// Limits are the cgroup v2 payloads written into the job's cgroup before
	// the child starts work, already canonicalized by cgrouplimits. An empty
	// field writes nothing.
	Limits cgrouplimits.Limits
}

// JobConfinementFromEnv reads the builder confinement settings:
// MT_BUILDER_UID, MT_BUILDER_CGROUP_ROOT, MT_BUILDER_MEMORY_MAX,
// MT_BUILDER_PIDS_MAX, MT_BUILDER_CPU_MAX.
//
// The limits go through cgrouplimits rather than reaching the kernel raw.
// MT_BUILDER_CPU_MAX is a CORE COUNT, but cpu.max wants "<quota> <period>", so
// passing it through unparsed made the kernel reject the write — and because
// placeJobInCgroup stops at the first failed limit, that one rejection left
// build jobs with no memory or pids cap either.
func JobConfinementFromEnv(log *slog.Logger) JobConfinement {
	uid := 0
	if v := os.Getenv("MT_BUILDER_UID"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			uid = n
		}
	}
	return JobConfinement{
		UID:        uid,
		CgroupRoot: os.Getenv("MT_BUILDER_CGROUP_ROOT"),
		Limits: cgrouplimits.FromEnv(log,
			"MT_BUILDER_MEMORY_MAX", "MT_BUILDER_PIDS_MAX", "MT_BUILDER_CPU_MAX"),
	}
}

func (r SubprocessRunner) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func (r SubprocessRunner) Run(ctx context.Context, spec JobSpec, sink pkgbuild.ProgressSink) error {
	if len(r.Argv) == 0 {
		return fmt.Errorf("builder: SubprocessRunner has no argv")
	}
	sink = sinkOrNop(sink)
	timeout := r.Timeout
	if timeout == 0 {
		timeout = DefaultJobTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	input, err := json.Marshal(spec)
	if err != nil {
		return err
	}

	jobDir := filepath.Dir(spec.BuildDir)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		return err
	}

	cmd := exec.Command(r.Argv[0], r.Argv[1:]...)
	cmd.Stdin = strings.NewReader(string(input))
	// The toolchains (go, node, pnpm, npm) live wherever the operator put
	// them, so unlike a tenant's fixed /usr/bin:/bin the job inherits the
	// ROUTER's PATH — operator-controlled either way. Everything else is
	// scrubbed; HOME doubles as npm's cache root so per-job state dies with
	// the job dir.
	jobTmp := filepath.Join(jobDir, "tmp")
	cmd.Env = append([]string{
		"HOME=" + jobDir,
		"TMPDIR=" + jobTmp,
		"PATH=" + os.Getenv("PATH"),
		// A git-spec member is cloned by `npm pack` inside the job. When the
		// job is confined its uid is mapped to 0 in a user namespace, so every
		// file outside the mapping — including an operator's local checkout —
		// appears owned by the unmapped overflow uid (65534). git reads that as
		// "dubious ownership" and refuses, failing the fetch. HOME is a fresh
		// per-job dir with no gitconfig, so the exemption cannot come from a
		// user's config; it has to ride in the environment. Scoped to git's
		// own config mechanism rather than a global setting on the host.
		//
		// This is safe here in a way it would not be on a developer box: the
		// specs reaching a build are operator- or tenant-proposed strings that
		// ValidatePackageSpec already constrained, the clone runs confined, and
		// --ignore-scripts means the cloned tree is never executed. The check
		// git is performing (does another *user* own this repo) is answering a
		// question the job's confinement already answers differently.
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=safe.directory",
		"GIT_CONFIG_VALUE_0=*",
	}, r.extraEnv...)
	if err := os.MkdirAll(jobTmp, 0o755); err != nil {
		return err
	}
	// Unix sockets created under TMPDIR (tsx's IPC pipe during the workspace
	// postinstall, most notably) are bound by sun_path (~104 bytes on macOS,
	// 108 on Linux). A deep builder root pushes the job TMPDIR past it and the
	// build dies with an obscure `listen EINVAL` inside pnpm install — warn
	// with the real cause so the operator shortens MT_ROOT instead of chasing
	// a phantom pnpm failure.
	if len(jobTmp) > 75 {
		r.log().Warn("build job TMPDIR is long; tools creating unix sockets there may fail with listen EINVAL (sun_path limit) — use a shorter builder root",
			"tmpdir", jobTmp, "len", len(jobTmp))
	}
	cmd.Dir = jobDir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	confined, err := confineJobCmd(cmd, jobDir, spec, r.Confinement, r.log())
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start build job: %w", err)
	}
	if confined && r.Confinement.CgroupRoot != "" {
		if err := placeJobInCgroup(r.Confinement, spec.BuildID, cmd.Process.Pid); err != nil {
			r.log().Warn("could not place build job in cgroup — it runs with NO resource limits",
				"buildId", spec.BuildID, "error", err)
		}
	}
	// The context backstop kills the whole process GROUP: the job's real work
	// happens in grandchildren (pnpm, node, go) that a plain Process.Kill
	// would orphan mid-write.
	killed := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		case <-killed:
		}
	}()
	defer close(killed)

	result, readErr := streamJobLines(stdout, sink)
	waitErr := cmd.Wait()

	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("build job timed out or was canceled: %w", ctx.Err())
	case result != nil && !result.OK:
		// The child reported its own failure before exiting nonzero — its
		// structured reason beats the bare exit status. Append whatever the
		// child wrote to stderr: the structured reason is one wrapped line
		// ("assemble: fetch X: exit status 1") while the underlying tool's
		// actual complaint (a permission denial, a git refusal) only ever
		// reaches stderr. Dropping it turns a one-look diagnosis into a hunt.
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("build failed: %s\njob stderr:\n%s", result.Error, firstLines(msg, 20))
		}
		return fmt.Errorf("build failed: %s", result.Error)
	case waitErr != nil:
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = waitErr.Error()
		}
		return fmt.Errorf("build job died: %s", firstLines(msg, 8))
	case readErr != nil:
		return fmt.Errorf("read build job output: %w", readErr)
	case result == nil:
		return fmt.Errorf("build job exited without reporting a result")
	}
	return nil
}

// streamJobLines forwards the child's progress/log lines into sink and
// returns the final result line, if any.
func streamJobLines(r io.Reader, sink pkgbuild.ProgressSink) (*jobLine, error) {
	var result *jobLine
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		raw := sc.Text()
		var line jobLine
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			sink.Logf("%s", raw)
			continue
		}
		switch line.Type {
		case "progress":
			sink.Progress(line.Step, line.Percent, line.Message)
		case "log":
			sink.Logf("%s", line.Message)
		case "result":
			l := line
			result = &l
		default:
			sink.Logf("%s", raw)
		}
	}
	return result, sc.Err()
}

// ServeJobStdio is the child side: read one JobSpec from stdin, run the job,
// stream progress as JSON lines on stdout, and end with a result line. run is
// the job body — serve-multi passes InProcessRunner{}.Run (the real
// pipeline); tests pass fakes. Called before any other init, like
// manifesteval.ServeStdio.
func ServeJobStdio(run func(ctx context.Context, spec JobSpec, sink pkgbuild.ProgressSink) error) int {
	body, err := io.ReadAll(io.LimitReader(os.Stdin, 8<<20))
	if err != nil {
		fmt.Fprintf(os.Stderr, "read job spec: %v\n", err)
		return 1
	}
	var spec JobSpec
	if err := json.Unmarshal(body, &spec); err != nil {
		fmt.Fprintf(os.Stderr, "decode job spec: %v\n", err)
		return 1
	}
	out := json.NewEncoder(os.Stdout)
	sink := stdioSink{enc: out}
	if err := run(context.Background(), spec, sink); err != nil {
		_ = out.Encode(jobLine{Type: "result", OK: false, Error: err.Error()})
		return 1
	}
	if err := out.Encode(jobLine{Type: "result", OK: true}); err != nil {
		fmt.Fprintf(os.Stderr, "write result: %v\n", err)
		return 1
	}
	return 0
}

// stdioSink writes ProgressSink events as protocol lines. json.Encoder is not
// concurrency-safe, but pkgbuild delivers sink events from one goroutine at a
// time (RunCmdStreaming's single reader), matching its own throttle's
// assumption.
type stdioSink struct {
	enc *json.Encoder
}

func (s stdioSink) Progress(step string, percent int, message string) {
	_ = s.enc.Encode(jobLine{Type: "progress", Step: step, Percent: percent, Message: message})
}

func (s stdioSink) Logf(format string, args ...any) {
	_ = s.enc.Encode(jobLine{Type: "log", Message: fmt.Sprintf(format, args...)})
}

func sinkOrNop(sink pkgbuild.ProgressSink) pkgbuild.ProgressSink {
	if sink == nil {
		return pkgbuild.NopSink()
	}
	return sink
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		return strings.Join(lines[:n], "\n") + " …"
	}
	return s
}
