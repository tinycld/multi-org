package orgmanager

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"regexp"
	"strconv"
)

// TenantLimits are the cgroup v2 interface-file payloads written into each
// tenant's cgroup before the child is placed in it (spawn_linux.go
// placeInCgroup). An empty field writes nothing — that resource is unlimited
// for the tenant. The fields hold the exact bytes for the kernel file, already
// validated and canonicalized by TenantLimitsFromEnv.
type TenantLimits struct {
	// MemoryMax is the payload for memory.max: bytes with an optional
	// K/M/G/T suffix, or "max".
	MemoryMax string
	// PidsMax is the payload for pids.max: a positive integer or "max".
	PidsMax string
	// CPUMax is the payload for cpu.max: "<quota> <period>" in microseconds,
	// or "max". TenantLimitsFromEnv derives it from a core count.
	CPUMax string
}

// Any reports whether at least one limit is configured.
func (l TenantLimits) Any() bool {
	return l.MemoryMax != "" || l.PidsMax != "" || l.CPUMax != ""
}

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
	var lim TenantLimits
	lim.MemoryMax = parseLimitEnv(log, "MT_TENANT_MEMORY_MAX", parseMemoryMax)
	lim.PidsMax = parseLimitEnv(log, "MT_TENANT_PIDS_MAX", parsePidsMax)
	lim.CPUMax = parseLimitEnv(log, "MT_TENANT_CPU_MAX", parseCPUMax)
	return lim
}

// parseLimitEnv reads one limit variable through its parser, logging and
// dropping invalid values.
func parseLimitEnv(log *slog.Logger, key string, parse func(string) (string, error)) string {
	raw := os.Getenv(key)
	if raw == "" {
		return ""
	}
	val, err := parse(raw)
	if err != nil {
		log.Error("invalid tenant limit ignored — this resource is UNLIMITED until fixed",
			"var", key, "value", raw, "error", err)
		return ""
	}
	return val
}

// memoryMaxRe matches what the kernel accepts for memory.max writes: a byte
// count with an optional binary-unit suffix.
var memoryMaxRe = regexp.MustCompile(`^[0-9]+[KMGT]?$`)

func parseMemoryMax(v string) (string, error) {
	if v == "max" {
		return "max", nil
	}
	if !memoryMaxRe.MatchString(v) {
		return "", fmt.Errorf("want bytes with optional K/M/G/T suffix (e.g. 512M) or \"max\"")
	}
	return v, nil
}

func parsePidsMax(v string) (string, error) {
	if v == "max" {
		return "max", nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return "", fmt.Errorf("want a positive integer or \"max\"")
	}
	return strconv.Itoa(n), nil
}

// cpuPeriodMicros is the cpu.max accounting period. 100ms is the kernel
// default and what container runtimes use; the quota scales against it.
const cpuPeriodMicros = 100_000

func parseCPUMax(v string) (string, error) {
	if v == "max" {
		return "max", nil
	}
	cores, err := strconv.ParseFloat(v, 64)
	if err != nil || cores <= 0 || math.IsInf(cores, 0) || math.IsNaN(cores) || cores > 4096 {
		return "", fmt.Errorf("want cores as a positive decimal (e.g. 1.5) or \"max\"")
	}
	quota := int64(math.Round(cores * cpuPeriodMicros))
	if quota <= 0 {
		return "", fmt.Errorf("cores value %q rounds to a zero quota", v)
	}
	return fmt.Sprintf("%d %d", quota, cpuPeriodMicros), nil
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
