package orgmanager

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"regexp"
	"strconv"

	"tinycld.org/multi-org/internal/cgrouplimits"
)

// TenantLimits are the cgroup v2 interface-file payloads written into each
// tenant's cgroup before the child is placed in it (spawn_linux.go
// placeInCgroup). An empty field writes nothing — that resource is unlimited
// for the tenant. The fields hold the exact bytes for the kernel file, already
// validated and canonicalized by TenantLimitsFromEnv.
//
// The parsing lives in internal/cgrouplimits so the build-job runner applies
// the identical rules; it once did not, and a raw MT_BUILDER_CPU_MAX left
// every build unconfined.
type TenantLimits = cgrouplimits.Limits

// TenantLimitsFromEnv reads the per-tenant cgroup limits from the
// environment:
//
//	MT_TENANT_MEMORY_MAX  bytes with optional K/M/G/T suffix (e.g. "512M"), or "max"
//	MT_TENANT_PIDS_MAX    positive integer (e.g. "256"), or "max"
//	MT_TENANT_CPU_MAX     cores as a positive decimal (e.g. "1.5"), or "max"
//
// There are deliberately no defaults: unset means unlimited, and NewSpawner
// warns loudly when MT_CGROUP_ROOT is set with no limits at all. An INVALID
// value is logged at Error and treated as unset — the same degraded-but-loud
// mode as the rest of confinement — so a typo cannot take every org down, but
// it cannot pass silently either.
func TenantLimitsFromEnv(log *slog.Logger) TenantLimits {
	return cgrouplimits.FromEnv(log,
		"MT_TENANT_MEMORY_MAX", "MT_TENANT_PIDS_MAX", "MT_TENANT_CPU_MAX")
}

// diskMaxRe matches a byte count with an optional binary-unit suffix, same
// shape as memory.max but resolved to an absolute byte count for quotactl.
var diskMaxRe = regexp.MustCompile(`^([0-9]+)([KMGT]?)$`)

// DiskMaxBytesFromEnv reads MT_TENANT_DISK_MAX — the per-tenant hard filesystem
// quota (block limit) applied to the tenant uid via quotactl, the kernel
// backstop against hostile package Go bypassing app.Save (design §4). It is a
// router-wide ceiling distinct from the per-org soft storage_limit_bytes (the
// commercial plan the app layer enforces): set it comfortably ABOVE any plan so
// the app's own "over limit" error fires first and a tenant only hits EDQUOT on
// genuine runaway. 0 (unset) means no kernel quota. An invalid value is logged
// and treated as unset, matching the cgroup-limit philosophy.
func DiskMaxBytesFromEnv(log *slog.Logger) int64 {
	raw := os.Getenv("MT_TENANT_DISK_MAX")
	if raw == "" {
		return 0
	}
	n, err := parseByteSize(raw)
	if err != nil {
		log.Error("invalid MT_TENANT_DISK_MAX ignored — no kernel disk quota until fixed",
			"value", raw, "error", err)
		return 0
	}
	return n
}

func parseByteSize(v string) (int64, error) {
	m := diskMaxRe.FindStringSubmatch(v)
	if m == nil {
		return 0, fmt.Errorf("want bytes with optional K/M/G/T suffix (e.g. 10G)")
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, err
	}
	mult := int64(1)
	switch m[2] {
	case "K":
		mult = 1 << 10
	case "M":
		mult = 1 << 20
	case "G":
		mult = 1 << 30
	case "T":
		mult = 1 << 40
	}
	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf("value overflows int64 bytes")
	}
	return n * mult, nil
}

// cgroupLimitsWarning returns the operator warning for a limit configuration
// that cannot do what it appears to, or "" when the configuration is coherent.
// NewSpawner logs it at Warn — same degraded-but-loud philosophy as the rest
// of confinement.
func cgroupLimitsWarning(cgroupRoot string, lim TenantLimits) string {
	switch {
	case cgroupRoot != "" && !lim.Any():
		return "MT_CGROUP_ROOT is set but no MT_TENANT_MEMORY_MAX/MT_TENANT_PIDS_MAX/MT_TENANT_CPU_MAX " +
			"is configured: tenant cgroups carry NO limits, so a runaway tenant can still starve the host."
	case cgroupRoot == "" && lim.Any():
		return "tenant limits are configured but MT_CGROUP_ROOT is unset: without a cgroup root the " +
			"limits are never applied."
	}
	return ""
}
