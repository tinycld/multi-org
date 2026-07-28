package orgmanager

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// limitsTestLogger returns a logger whose output the test can inspect.
func limitsTestLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func setLimitEnv(t *testing.T, mem, pids, cpu string) {
	t.Helper()
	t.Setenv("MT_TENANT_MEMORY_MAX", mem)
	t.Setenv("MT_TENANT_PIDS_MAX", pids)
	t.Setenv("MT_TENANT_CPU_MAX", cpu)
}

func TestTenantLimitsFromEnv_ParsesAndCanonicalizes(t *testing.T) {
	log, buf := limitsTestLogger()
	setLimitEnv(t, "512M", "256", "1.5")

	lim := TenantLimitsFromEnv(log)
	want := TenantLimits{MemoryMax: "512M", PidsMax: "256", CPUMax: "150000 100000"}
	if lim != want {
		t.Fatalf("got %+v, want %+v", lim, want)
	}
	if !lim.Any() {
		t.Fatal("Any() must report configured limits")
	}
	if buf.Len() != 0 {
		t.Fatalf("valid config must not log: %s", buf.String())
	}
}

func TestTenantLimitsFromEnv_MaxPassesThrough(t *testing.T) {
	log, _ := limitsTestLogger()
	setLimitEnv(t, "max", "max", "max")

	lim := TenantLimitsFromEnv(log)
	want := TenantLimits{MemoryMax: "max", PidsMax: "max", CPUMax: "max"}
	if lim != want {
		t.Fatalf("got %+v, want %+v", lim, want)
	}
}

func TestTenantLimitsFromEnv_FractionalCores(t *testing.T) {
	log, _ := limitsTestLogger()
	setLimitEnv(t, "", "", "0.5")

	lim := TenantLimitsFromEnv(log)
	if lim.CPUMax != "50000 100000" {
		t.Fatalf("0.5 cores should canonicalize to half quota, got %q", lim.CPUMax)
	}
	if lim.MemoryMax != "" || lim.PidsMax != "" {
		t.Fatalf("unset vars must stay unset: %+v", lim)
	}
}

func TestTenantLimitsFromEnv_UnsetMeansUnlimited(t *testing.T) {
	log, buf := limitsTestLogger()
	setLimitEnv(t, "", "", "")

	lim := TenantLimitsFromEnv(log)
	if lim.Any() {
		t.Fatalf("expected no limits, got %+v", lim)
	}
	if buf.Len() != 0 {
		t.Fatalf("unset config must not log: %s", buf.String())
	}
}

// An invalid value must be LOUD (Error log naming the variable) and treated as
// unset — a typo must neither pass silently nor take every org down.
func TestTenantLimitsFromEnv_InvalidValuesAreLoudAndUnset(t *testing.T) {
	cases := []struct {
		name             string
		mem, pids, cpu   string
		wantLogSubstring string
	}{
		{"memory suffix", "12X", "", "", "MT_TENANT_MEMORY_MAX"},
		{"memory negative", "-1G", "", "", "MT_TENANT_MEMORY_MAX"},
		{"pids negative", "", "-3", "", "MT_TENANT_PIDS_MAX"},
		{"pids zero", "", "0", "", "MT_TENANT_PIDS_MAX"},
		{"pids junk", "", "many", "", "MT_TENANT_PIDS_MAX"},
		{"cpu zero", "", "", "0", "MT_TENANT_CPU_MAX"},
		{"cpu negative", "", "", "-1", "MT_TENANT_CPU_MAX"},
		{"cpu junk", "", "", "abc", "MT_TENANT_CPU_MAX"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log, buf := limitsTestLogger()
			setLimitEnv(t, tc.mem, tc.pids, tc.cpu)

			lim := TenantLimitsFromEnv(log)
			if lim.Any() {
				t.Fatalf("invalid value must be treated as unset, got %+v", lim)
			}
			out := buf.String()
			if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, tc.wantLogSubstring) {
				t.Fatalf("expected an ERROR log naming %s, got: %s", tc.wantLogSubstring, out)
			}
		})
	}
}

func TestCgroupLimitsWarning(t *testing.T) {
	limits := TenantLimits{MemoryMax: "512M"}

	// Cgroup root with no limits: the group exists but constrains nothing —
	// the exact gap P5-2 exists to close must not be silent.
	if msg := cgroupLimitsWarning("/sys/fs/cgroup/mt", TenantLimits{}); !strings.Contains(msg, "no MT_TENANT") {
		t.Fatalf("expected a no-limits warning, got %q", msg)
	}
	// Limits with no cgroup root: nothing will ever apply them.
	if msg := cgroupLimitsWarning("", limits); !strings.Contains(msg, "MT_CGROUP_ROOT") {
		t.Fatalf("expected an unapplied-limits warning, got %q", msg)
	}
	// Coherent configurations warn about nothing.
	if msg := cgroupLimitsWarning("/sys/fs/cgroup/mt", limits); msg != "" {
		t.Fatalf("coherent config must not warn, got %q", msg)
	}
	if msg := cgroupLimitsWarning("", TenantLimits{}); msg != "" {
		t.Fatalf("cgroups unused must not warn, got %q", msg)
	}
}
