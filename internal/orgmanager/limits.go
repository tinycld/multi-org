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
