package orgmanager

import (
	"context"
	"log/slog"
	"os"
	"time"
)

// Process is the host's handle on a running tenant. It is deliberately narrow —
// signal, kill, reap, identify — so tests can substitute an in-process fake
// without simulating os/exec.
type Process interface {
	Signal(os.Signal) error
	Kill() error
	// Wait blocks until the process exits and returns its exit status. It is
	// called exactly once per process, by the supervisor goroutine.
	Wait() error
	Pid() int
}

// SpawnRequest is everything a tenant process needs to come up. Every path is
// passed explicitly; Slug is for logging and identification only and carries no
// authority, so a malformed slug cannot traverse anywhere.
type SpawnRequest struct {
	Slug       string
	OrgDir     string
	SocketPath string
	BinaryPath string

	// CardDAVConfig is the path to the JSON source list the child registers
	// CardDAV routes from, or "" when the org's packages contribute none.
	CardDAVConfig string
	// WebDAVConfig is the path to the org's materialized webdav.json, or empty
	// when no package declares a webdav block.
	WebDAVConfig string
	// CalDAVConfig is the path to the org's materialized caldav.json, or empty
	// when no package declares a caldav block.
	CalDAVConfig string
	// QuotaConfig is the path to the org's materialized quota.json, carrying
	// the storage ceiling the operator assigned.
	QuotaConfig string

	HooksPool int
	Drain     time.Duration

	// ReadyFile is the write end of the readiness pipe, handed to the child as
	// fd 3. The child writes one JSON line and closes it.
	ReadyFile *os.File

	// PackagesDir is the store root the org's materialized symlinks point into.
	// Confinement must expose it read-only at this same absolute path or every
	// symlink breaks.
	PackagesDir string

	// ConfinePackages asks the child to remount PackagesDir read-only inside
	// its own mount namespace before serving. Set only by the Linux spawner:
	// Go cannot run code between fork and exec, so the steps that must happen
	// inside the new namespace are performed by the child at startup.
	ConfinePackages bool
}

// Spawner starts a tenant process. The production implementations differ only
// in how much confinement they apply; the argv, environment, and pipe wiring
// are shared (see spawn_exec.go).
type Spawner interface {
	Spawn(ctx context.Context, req SpawnRequest, log *slog.Logger) (Process, error)
}
