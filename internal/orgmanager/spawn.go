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

	// IMAPSocketPath / SMTPSocketPath / MXSocketPath are the per-org unix
	// sockets the tenant binds its mail listeners on (beside SocketPath, same
	// dir — the spawner chowns exactly one dir per org). Empty when the org's
	// resolved package set has no mail: the tenant then starts no mail
	// listeners at all. The mail router dials these after demuxing a public
	// :993/:465 connection by SNI or routing a :25 transaction by RCPT TO.
	IMAPSocketPath string
	SMTPSocketPath string
	MXSocketPath   string

	// ControlSocketPath is the ROUTER-bound control socket the tenant dials
	// to propose deploys (design D1). Already bound and serving by the time
	// the child starts; passed so the tenant knows where to dial. Empty when
	// the manager wires no Control handler — the tenant then has no deploy
	// channel.
	ControlSocketPath string

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
	// PackagesConfig is the path to the org's materialized packages.json — the
	// resolved package slugs, surfaced to the child as tenantmain.Extras.
	// PackageSlugs. The dual-mode binary registers its linked feature Go
	// unconditionally (the artifact is the gate), so this no longer gates
	// registration; it remains the org's authoritative slug set for the
	// tenant's package-registry reconcile. Empty when the host wires no
	// PackageSlugs hook.
	PackagesConfig string
	// AppConfig is the path to the org's materialized app.json, carrying the
	// public URL the tenant adopts as Settings().Meta.AppURL at boot. Empty
	// when the host wires no OrgURL hook (the tenant then keeps its stored
	// settings).
	AppConfig string

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

	// Confines reports whether tenants are OS-isolated (distinct uids + mount/PID
	// namespaces). When false — a non-root host, an unset uid window, or the
	// darwin dev fallback — every tenant runs as the same host user, so the
	// 0700 per-org socket-dir identity collapses: any tenant can dial any org's
	// ctl.sock and deploy to it. The manager refuses to bind control sockets in
	// that case, so degraded mode = no tenant-proposed deploys (operator-driven
	// deploys through the control-plane API still work).
	Confines() bool
}
