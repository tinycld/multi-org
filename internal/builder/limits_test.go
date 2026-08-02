package builder

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func builderTestLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func setBuilderLimitEnv(t *testing.T, mem, pids, cpu string) {
	t.Helper()
	t.Setenv("MT_BUILDER_MEMORY_MAX", mem)
	t.Setenv("MT_BUILDER_PIDS_MAX", pids)
	t.Setenv("MT_BUILDER_CPU_MAX", cpu)
}

// The regression this file exists for. MT_BUILDER_CPU_MAX is a CORE COUNT, but
// cpu.max wants "<quota> <period>" — the builder used to hand the raw string to
// the kernel, which rejected it, and because placement stopped at the first bad
// write the job also lost its memory and pids caps and escaped its cgroup.
// The reference installer ships MT_BUILDER_CPU_MAX=3, so this was the default.
func TestJobConfinementFromEnv_CPUMaxIsCanonicalizedFromCores(t *testing.T) {
	log, buf := builderTestLogger()
	setBuilderLimitEnv(t, "8G", "4096", "3")

	conf := JobConfinementFromEnv(log)
	if conf.Limits.CPUMax != "300000 100000" {
		t.Fatalf("3 cores must canonicalize to a quota/period pair the kernel accepts, got %q",
			conf.Limits.CPUMax)
	}
	if conf.Limits.MemoryMax != "8G" || conf.Limits.PidsMax != "4096" {
		t.Fatalf("memory and pids must pass through: %+v", conf.Limits)
	}
	if buf.Len() != 0 {
		t.Fatalf("valid config must not log: %s", buf.String())
	}
}

func TestJobConfinementFromEnv_FractionalCores(t *testing.T) {
	log, _ := builderTestLogger()
	setBuilderLimitEnv(t, "", "", "0.5")

	conf := JobConfinementFromEnv(log)
	if conf.Limits.CPUMax != "50000 100000" {
		t.Fatalf("0.5 cores should be half quota, got %q", conf.Limits.CPUMax)
	}
	if conf.Limits.MemoryMax != "" || conf.Limits.PidsMax != "" {
		t.Fatalf("unset vars must stay unset: %+v", conf.Limits)
	}
}

func TestJobConfinementFromEnv_MaxPassesThrough(t *testing.T) {
	log, _ := builderTestLogger()
	setBuilderLimitEnv(t, "max", "max", "max")

	conf := JobConfinementFromEnv(log)
	if conf.Limits.MemoryMax != "max" || conf.Limits.PidsMax != "max" || conf.Limits.CPUMax != "max" {
		t.Fatalf(`"max" must pass through: %+v`, conf.Limits)
	}
}

// An invalid value drops THAT resource and says so, rather than aborting the
// build or silently disabling every limit.
func TestJobConfinementFromEnv_InvalidValueIsDroppedAndLogged(t *testing.T) {
	log, buf := builderTestLogger()
	setBuilderLimitEnv(t, "8G", "4096", "three")

	conf := JobConfinementFromEnv(log)
	if conf.Limits.CPUMax != "" {
		t.Fatalf("unparsable cores must be dropped, got %q", conf.Limits.CPUMax)
	}
	if conf.Limits.MemoryMax != "8G" || conf.Limits.PidsMax != "4096" {
		t.Fatalf("one bad value must not take the others down: %+v", conf.Limits)
	}
	if !strings.Contains(buf.String(), "MT_BUILDER_CPU_MAX") {
		t.Fatalf("a dropped limit must name the variable: %s", buf.String())
	}
}

func TestJobConfinementFromEnv_UnsetMeansUnlimited(t *testing.T) {
	log, buf := builderTestLogger()
	setBuilderLimitEnv(t, "", "", "")

	conf := JobConfinementFromEnv(log)
	if conf.Limits.Any() {
		t.Fatalf("expected no limits, got %+v", conf.Limits)
	}
	if buf.Len() != 0 {
		t.Fatalf("unset is not an error: %s", buf.String())
	}
}

func TestJobConfinementFromEnv_ReadsUIDAndCgroupRoot(t *testing.T) {
	log, _ := builderTestLogger()
	setBuilderLimitEnv(t, "", "", "")
	t.Setenv("MT_BUILDER_UID", "900")
	t.Setenv("MT_BUILDER_CGROUP_ROOT", "/sys/fs/cgroup/tinycld-builder")

	conf := JobConfinementFromEnv(log)
	if conf.UID != 900 {
		t.Fatalf("UID = %d, want 900", conf.UID)
	}
	if conf.CgroupRoot != "/sys/fs/cgroup/tinycld-builder" {
		t.Fatalf("CgroupRoot = %q", conf.CgroupRoot)
	}
}
