//go:build linux

package orgmanager

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Per-uid disk quota via quotactl(2). The tenant uid owns exactly the org dir
// (chownTree) and nothing else on the host, so a block quota on that uid bounds
// precisely this org's on-disk footprint — including everything app-layer
// accounting cannot see: the SQLite WAL, the tmp/ upload spool, and raw writes
// from hostile package Go that never reach app.Save. This is the kernel
// backstop design §4 calls for.
//
// quotactl has no typed wrapper in x/sys, so the command word and the on-disk
// block are built by hand against the stable kernel ABI.
const (
	// QCMD(Q_SETQUOTA, USRQUOTA): subcmd in the high 16 bits, type in the low.
	qSetQuota = 0x800008 // Q_SETQUOTA
	usrQuota  = 0        // USRQUOTA

	// dqbBLimitsSet marks the block soft/hard limits as the fields to apply
	// (QIF_BLIMITS), leaving inode limits and usage untouched.
	dqbBLimitsSet = 0x01 // QIF_BLIMITS

	// quotaBlockSize is the fixed 1 KiB unit quotactl block limits are counted
	// in, independent of the filesystem's own block size.
	quotaBlockSize = 1024
)

func qcmd(cmd, typ int) uint {
	return uint((cmd << 8) | (typ & 0x00ff))
}

// ifDqblk mirrors struct if_dqblk (linux/quota.h) — the layout quotactl reads
// for Q_SETQUOTA. Field order and widths are ABI; only the block-limit fields
// and the valid-flags mask are set here.
type ifDqblk struct {
	BHardlimit uint64
	BSoftlimit uint64
	CurSpace   uint64
	IHardlimit uint64
	ISoftlimit uint64
	CurInodes  uint64
	BTime      uint64
	ITime      uint64
	Valid      uint32
	_          uint32 // padding to 8-byte alignment
}

// applyDiskQuota sets a hard block quota of maxBytes for uid on the filesystem
// backing orgDir. Returns an error (the caller warns, never fails the spawn)
// when the backing filesystem has no quota support enabled.
func applyDiskQuota(orgDir string, uid int, maxBytes int64) error {
	device, err := backingDevice(orgDir)
	if err != nil {
		return err
	}

	blocks := uint64(maxBytes) / quotaBlockSize
	if uint64(maxBytes)%quotaBlockSize != 0 {
		blocks++ // never round a limit DOWN
	}
	dq := ifDqblk{
		BHardlimit: blocks,
		BSoftlimit: blocks,
		Valid:      dqbBLimitsSet,
	}

	devPtr, err := unix.BytePtrFromString(device)
	if err != nil {
		return err
	}
	_, _, errno := unix.Syscall6(
		unix.SYS_QUOTACTL,
		uintptr(qcmd(qSetQuota, usrQuota)),
		uintptr(unsafe.Pointer(devPtr)),
		uintptr(uid),
		uintptr(unsafe.Pointer(&dq)),
		0, 0,
	)
	if errno != 0 {
		return fmt.Errorf("quotactl Q_SETQUOTA on %s for uid %d: %w (is user quota enabled on that filesystem?)", device, uid, errno)
	}
	return nil
}

// backingDevice resolves the block device of the filesystem holding path by
// walking /proc/mounts for the longest mountpoint that is a prefix of path.
// quotactl addresses a quota by its backing device, not by a path.
func backingDevice(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return "", err
	}
	defer f.Close()

	var (
		bestDevice string
		bestLen    = -1
	)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		device, mountpoint := fields[0], unescapeMount(fields[1])
		if !strings.HasPrefix(device, "/dev/") {
			continue // skip tmpfs/proc/overlay and friends — no block quota
		}
		if isPathUnder(abs, mountpoint) && len(mountpoint) > bestLen {
			bestDevice, bestLen = device, len(mountpoint)
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	if bestDevice == "" {
		return "", fmt.Errorf("no block-device mount found for %s", abs)
	}
	return bestDevice, nil
}

// isPathUnder reports whether path is at or below mountpoint, comparing whole
// path components so /var is not treated as a prefix of /variant.
func isPathUnder(path, mountpoint string) bool {
	if mountpoint == "/" {
		return true
	}
	return path == mountpoint || strings.HasPrefix(path, mountpoint+"/")
}

// unescapeMount decodes the octal escapes /proc/mounts uses for spaces (\040)
// and other special characters in mountpoint paths.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			var v int
			if _, err := fmt.Sscanf(s[i+1:i+4], "%03o", &v); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
