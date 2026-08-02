package cgrouplimits

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func testLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func TestParseCPUMax(t *testing.T) {
	// A bare core count is the whole point: the kernel rejects it, so it must
	// be converted here rather than passed through.
	for _, tc := range []struct{ in, want string }{
		{"1", "100000 100000"},
		{"2", "200000 100000"},
		{"3", "300000 100000"},
		{"0.5", "50000 100000"},
		{"1.5", "150000 100000"},
		{"max", "max"},
	} {
		got, err := ParseCPUMax(tc.in)
		if err != nil {
			t.Errorf("ParseCPUMax(%q) errored: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseCPUMax(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	for _, bad := range []string{"three", "0", "-1", "", "1e999", "NaN", "5000"} {
		if got, err := ParseCPUMax(bad); err == nil {
			t.Errorf("ParseCPUMax(%q) = %q, want an error", bad, got)
		}
	}
}

func TestParseMemoryMax(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"512M", "512M"},
		{"8G", "8G"},
		{"67108864", "67108864"},
		{"max", "max"},
	} {
		got, err := ParseMemoryMax(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseMemoryMax(%q) = %q, %v", tc.in, got, err)
		}
	}
	for _, bad := range []string{"512MB", "eight gigs", "", "-1", "1.5G"} {
		if _, err := ParseMemoryMax(bad); err == nil {
			t.Errorf("ParseMemoryMax(%q) should error", bad)
		}
	}
}

func TestParsePidsMax(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"512", "512"},
		{"1", "1"},
		{"max", "max"},
	} {
		got, err := ParsePidsMax(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParsePidsMax(%q) = %q, %v", tc.in, got, err)
		}
	}
	for _, bad := range []string{"0", "-5", "lots", "", "1.5"} {
		if _, err := ParsePidsMax(bad); err == nil {
			t.Errorf("ParsePidsMax(%q) should error", bad)
		}
	}
}

func TestFromEnv_DropsInvalidValueAndKeepsSiblings(t *testing.T) {
	log, buf := testLogger()
	t.Setenv("X_MEM", "8G")
	t.Setenv("X_PIDS", "4096")
	t.Setenv("X_CPU", "three")

	lim := FromEnv(log, "X_MEM", "X_PIDS", "X_CPU")
	if lim.CPUMax != "" {
		t.Errorf("invalid cpu must be dropped, got %q", lim.CPUMax)
	}
	if lim.MemoryMax != "8G" || lim.PidsMax != "4096" {
		t.Errorf("one bad value must not drop the others: %+v", lim)
	}
	if !strings.Contains(buf.String(), "X_CPU") {
		t.Errorf("the dropped variable must be named in the log: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "UNLIMITED") {
		t.Errorf("the log must say the resource is now unlimited: %s", buf.String())
	}
}

func TestFromEnv_UnsetIsSilent(t *testing.T) {
	log, buf := testLogger()
	t.Setenv("X_MEM", "")
	t.Setenv("X_PIDS", "")
	t.Setenv("X_CPU", "")

	lim := FromEnv(log, "X_MEM", "X_PIDS", "X_CPU")
	if lim.Any() {
		t.Errorf("expected no limits, got %+v", lim)
	}
	if buf.Len() != 0 {
		t.Errorf("unset is not an error: %s", buf.String())
	}
}
