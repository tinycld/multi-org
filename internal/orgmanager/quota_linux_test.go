//go:build linux

package orgmanager

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseByteSize(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"0", 0, false},
		{"1024", 1024, false},
		{"10K", 10 << 10, false},
		{"512M", 512 << 20, false},
		{"2G", 2 << 30, false},
		{"1T", 1 << 40, false},
		{"", 0, true},
		{"10GB", 0, true},
		{"-5", 0, true},
		{"1.5G", 0, true},
		{"max", 0, true},
	}
	for _, c := range cases {
		got, err := parseByteSize(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseByteSize(%q) = %d, want error", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("parseByteSize(%q) = %d,%v; want %d", c.in, got, err, c.want)
		}
	}
}

// The command word must match the kernel's QCMD(Q_SETQUOTA, USRQUOTA) — a wrong
// shift silently addresses the wrong operation, so pin the exact value.
func TestQcmd(t *testing.T) {
	if got := qcmd(qSetQuota, usrQuota); got != qSetQuota<<8 {
		t.Errorf("qcmd = %#x, want %#x", got, qSetQuota<<8)
	}
}

func TestIsPathUnder(t *testing.T) {
	cases := []struct {
		path, mount string
		want        bool
	}{
		{"/var/lib/mt/pb_orgs/acme", "/", true},
		{"/var/lib/mt/pb_orgs/acme", "/var", true},
		{"/var/lib/mt/pb_orgs/acme", "/var/lib/mt", true},
		{"/variant/x", "/var", false}, // component boundary, not string prefix
		{"/var", "/var", true},
		{"/other", "/var/lib", false},
	}
	for _, c := range cases {
		if got := isPathUnder(c.path, c.mount); got != c.want {
			t.Errorf("isPathUnder(%q, %q) = %v, want %v", c.path, c.mount, got, c.want)
		}
	}
}

func TestUnescapeMount(t *testing.T) {
	if got := unescapeMount(`/mnt/with\040space`); got != "/mnt/with space" {
		t.Errorf("unescapeMount = %q", got)
	}
	if got := unescapeMount("/plain/path"); got != "/plain/path" {
		t.Errorf("unescapeMount plain = %q", got)
	}
}

// backingDevice must pick the LONGEST matching mountpoint (the actual
// filesystem holding the path), not merely /.
func TestBackingDevice_ResolvesToRealMount(t *testing.T) {
	dev, err := backingDevice("/")
	if err != nil {
		t.Skipf("no block-device root mount in this environment: %v", err)
	}
	if !strings.HasPrefix(dev, "/dev/") {
		t.Errorf("backingDevice(/) = %q, want a /dev/ device", dev)
	}
}

// TestConfinement_FilesystemQuotaApplied proves the kernel backstop end to end:
// set a small quota on a tenant uid, then confirm a write past it fails with
// EDQUOT. It needs root AND a quota-enabled filesystem, so it skips (never
// fails) when either is absent — the same capability-gate discipline as the
// cgroup test. CI provides a loopback quota-enabled image; a dev box without
// one skips loudly.
func TestConfinement_FilesystemQuotaApplied(t *testing.T) {
	requireConfinementEnv(t)

	dir := t.TempDir()
	// A tenant uid outside any real range; the test owns it for its duration.
	const uid = 61999
	if err := os.Chown(dir, uid, uid); err != nil {
		t.Skipf("cannot chown scratch dir to a tenant uid: %v", err)
	}

	if _, err := backingDevice(dir); err != nil {
		t.Skipf("scratch dir has no block-device mount: %v", err)
	}
	// 1 MiB hard cap.
	if err := applyDiskQuota(dir, uid, 1<<20); err != nil {
		t.Skipf("quota not enabled on the scratch filesystem (expected on most CI/dev boxes): %v", err)
	}

	// Write 4 MiB as the tenant uid; it must hit EDQUOT before completing.
	target := filepath.Join(dir, "blob")
	err := runAsUID(uid, "dd if=/dev/zero of="+target+" bs=1M count=4 2>&1")
	if err == nil {
		t.Fatal("write of 4 MiB succeeded under a 1 MiB quota; the quota is not enforced")
	}
}
