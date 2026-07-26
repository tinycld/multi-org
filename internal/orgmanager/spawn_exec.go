package orgmanager

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// readyFD is the child descriptor the readiness pipe lands on. Go places the
// first ExtraFiles entry at fd 3.
const readyFD = 3

// execProcess adapts *exec.Cmd to the Process interface.
type execProcess struct {
	cmd *exec.Cmd
}

func (p *execProcess) Signal(sig os.Signal) error { return p.cmd.Process.Signal(sig) }
func (p *execProcess) Wait() error                { return p.cmd.Wait() }
func (p *execProcess) Pid() int                   { return p.cmd.Process.Pid }

// Kill terminates the child's whole process group, not just the child. Without
// Setpgid + a negative pid, a tenant that spawned anything would leave orphans.
// Under a PID namespace on Linux this is mostly redundant — killing the
// namespace's PID 1 reaps everything — but it is the correctness backstop on
// darwin, which has no namespaces.
func (p *execProcess) Kill() error {
	if err := syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL); err != nil {
		// Fall back to the bare process if the group is already gone.
		return p.cmd.Process.Kill()
	}
	return nil
}

// buildCmd assembles the argv, environment, and fd wiring shared by every
// platform. The Linux spawner layers confinement on top of what this returns.
func buildCmd(ctx context.Context, req SpawnRequest, log *slog.Logger) *exec.Cmd {
	args := []string{
		"--org-dir", req.OrgDir,
		"--socket", req.SocketPath,
		"--ready-fd", strconv.Itoa(readyFD),
		"--slug", req.Slug,
		"--hooks-pool", strconv.Itoa(req.HooksPool),
		"--drain", req.Drain.String(),
	}
	if req.CardDAVConfig != "" {
		args = append(args, "--carddav-config", req.CardDAVConfig)
	}
	if req.WebDAVConfig != "" {
		args = append(args, "--webdav-config", req.WebDAVConfig)
	}
	if req.ConfinePackages {
		args = append(args, "--confine-packages", req.PackagesDir)
	}

	// NOT exec.CommandContext: ctx bounds the readiness handshake, not the
	// child's lifetime. Tying the process to it would kill every tenant the
	// moment its spawn context was cancelled — i.e. immediately after it came
	// up. The manager owns the child's lifetime via Evict and the supervisor.
	cmd := exec.Command(req.BinaryPath, args...)

	// The environment is an allowlist built from nothing, NOT a filter over
	// os.Environ(). The host holds MT_SUPERUSER_PASSWORD, MT_TLS_KEY and
	// friends, and a blocklist would leak every variable added in future. The
	// sandbox audits already taught this lesson four times over: things
	// re-enter through the surfaces you kept.
	cmd.Env = []string{
		"HOME=" + req.OrgDir,
		"TMPDIR=" + filepath.Join(req.OrgDir, "tmp"),
		"PATH=/usr/bin:/bin",
	}

	cmd.Dir = req.OrgDir
	cmd.ExtraFiles = []*os.File{req.ReadyFile}

	// Own process group so Kill can take out the whole tree.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err == nil {
		go pipeToLog(stdout, log, req.Slug, slog.LevelInfo)
	}
	stderr, err := cmd.StderrPipe()
	if err == nil {
		go pipeToLog(stderr, log, req.Slug, slog.LevelWarn)
	}

	return cmd
}

// pipeToLog forwards a child's output into the host logger, tagged by slug, so
// tenant output is attributable and never interleaves mid-line.
func pipeToLog(r io.Reader, log *slog.Logger, slug string, level slog.Level) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		log.Log(context.Background(), level, "tenant output", "slug", slug, "line", scanner.Text())
	}
}

// execSpawner runs the tenant as a plain subprocess with no OS confinement.
// It is the darwin implementation and the base the Linux one builds on.
type execSpawner struct{}

func (execSpawner) Spawn(ctx context.Context, req SpawnRequest, log *slog.Logger) (Process, error) {
	cmd := buildCmd(ctx, req, log)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start tenant %s: %w", req.Slug, err)
	}
	return &execProcess{cmd: cmd}, nil
}
