//go:build darwin

package orgmanager

import "log/slog"

// NewSpawner returns the darwin tenant spawner.
//
// macOS has no mount/PID namespaces, no cgroups, and no seccomp, so this is a
// plain subprocess: separate PID, scrubbed environment, own data directory —
// and nothing stopping tenant JS from opening any file the host user can read.
// The cross-org ATTACH DATABASE exploit that motivated per-process isolation is
// NOT closed here.
//
// This path exists so the router runs on a development machine. It is not a
// security boundary and must not host untrusted tenants.
func NewSpawner(log *slog.Logger) Spawner {
	log.Warn("tenant processes are NOT confined on darwin: no namespaces, cgroups, or seccomp. " +
		"This is a development fallback and is not a security boundary — do not host untrusted tenants.")
	return execSpawner{}
}
