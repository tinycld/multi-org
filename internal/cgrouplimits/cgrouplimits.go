// Package cgrouplimits parses and canonicalizes cgroup v2 limit payloads.
//
// It exists because the tenant spawner and the build-job runner both write the
// same three interface files (memory.max, pids.max, cpu.max) and must agree on
// what an operator-supplied value means. They did not: the tenant path ran
// every value through a parser while the builder passed raw environment
// strings to the kernel, so MT_BUILDER_CPU_MAX=3 — the natural reading of
// "3 cores", and what the reference installer shipped — reached cpu.max as
// "3" and was rejected with EINVAL.
//
// That failure was not confined to the CPU limit. Both placement routines
// write limits before adding the pid, returning on the first error, so a
// rejected cpu.max meant the job was never placed in its cgroup at all and ran
// with NO memory or pids cap either. A single unparsed value silently removed
// every resource bound on code the router does not trust.
//
// The philosophy is the tenant path's, kept verbatim: an invalid value is
// logged at Error and treated as unset. A typo degrades one resource to
// unlimited and says so, rather than taking the host down or passing silently.
package cgrouplimits

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"regexp"
	"strconv"
)

// Limits are the cgroup v2 interface-file payloads written into a cgroup
// before a process is placed in it. An empty field writes nothing — that
// resource is unlimited. The fields hold the exact bytes for the kernel file,
// already validated and canonicalized.
type Limits struct {
	// MemoryMax is the payload for memory.max: bytes with an optional
	// K/M/G/T suffix, or "max".
	MemoryMax string
	// PidsMax is the payload for pids.max: a positive integer or "max".
	PidsMax string
	// CPUMax is the payload for cpu.max: "<quota> <period>" in microseconds,
	// or "max". Derived from a core count.
	CPUMax string
}

// Any reports whether at least one limit is configured.
func (l Limits) Any() bool {
	return l.MemoryMax != "" || l.PidsMax != "" || l.CPUMax != ""
}

// FromEnv reads a memory/pids/cpu limit triple from the environment, given the
// three variable names. Invalid values are logged and dropped.
//
//	memory  bytes with optional K/M/G/T suffix (e.g. "512M"), or "max"
//	pids    positive integer (e.g. "256"), or "max"
//	cpu     cores as a positive decimal (e.g. "1.5"), or "max"
//
// There are deliberately no defaults: unset means unlimited.
func FromEnv(log *slog.Logger, memoryVar, pidsVar, cpuVar string) Limits {
	return Limits{
		MemoryMax: parseEnv(log, memoryVar, ParseMemoryMax),
		PidsMax:   parseEnv(log, pidsVar, ParsePidsMax),
		CPUMax:    parseEnv(log, cpuVar, ParseCPUMax),
	}
}

// parseEnv reads one limit variable through its parser, logging and dropping
// invalid values.
func parseEnv(log *slog.Logger, key string, parse func(string) (string, error)) string {
	raw := os.Getenv(key)
	if raw == "" {
		return ""
	}
	val, err := parse(raw)
	if err != nil {
		if log == nil {
			log = slog.Default()
		}
		log.Error("invalid cgroup limit ignored — this resource is UNLIMITED until fixed",
			"var", key, "value", raw, "error", err)
		return ""
	}
	return val
}

// memoryMaxRe matches what the kernel accepts for memory.max writes: a byte
// count with an optional binary-unit suffix.
var memoryMaxRe = regexp.MustCompile(`^[0-9]+[KMGT]?$`)

// ParseMemoryMax canonicalizes a memory.max payload.
func ParseMemoryMax(v string) (string, error) {
	if v == "max" {
		return "max", nil
	}
	if !memoryMaxRe.MatchString(v) {
		return "", fmt.Errorf("want bytes with optional K/M/G/T suffix (e.g. 512M) or \"max\"")
	}
	return v, nil
}

// ParsePidsMax canonicalizes a pids.max payload.
func ParsePidsMax(v string) (string, error) {
	if v == "max" {
		return "max", nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return "", fmt.Errorf("want a positive integer or \"max\"")
	}
	return strconv.Itoa(n), nil
}

// CPUPeriodMicros is the cpu.max accounting period. 100ms is the kernel
// default and what container runtimes use; the quota scales against it.
const CPUPeriodMicros = 100_000

// ParseCPUMax converts a core count into a cpu.max payload. The kernel wants
// "<quota> <period>", so a bare core count like "3" is EINVAL — converting it
// here is the whole reason this package exists.
func ParseCPUMax(v string) (string, error) {
	if v == "max" {
		return "max", nil
	}
	cores, err := strconv.ParseFloat(v, 64)
	if err != nil || cores <= 0 || math.IsInf(cores, 0) || math.IsNaN(cores) || cores > 4096 {
		return "", fmt.Errorf("want cores as a positive decimal (e.g. 1.5) or \"max\"")
	}
	quota := int64(math.Round(cores * CPUPeriodMicros))
	if quota <= 0 {
		return "", fmt.Errorf("cores value %q rounds to a zero quota", v)
	}
	return fmt.Sprintf("%d %d", quota, CPUPeriodMicros), nil
}
