//go:build !linux

package builder

import (
	"log/slog"
	"os/exec"
)

// confineJobCmd is a no-op off Linux: build jobs run as an ordinary child
// process. Production is Linux; this is the dev-host degraded mode, and it
// says so.
func confineJobCmd(_ *exec.Cmd, _ string, _ JobSpec, conf JobConfinement, log *slog.Logger) (bool, error) {
	if conf.UID != 0 || conf.CgroupRoot != "" {
		log.Warn("builder job confinement is configured but only supported on linux — jobs run UNCONFINED")
	}
	return false, nil
}

// placeJobInCgroup is never reached off Linux (confineJobCmd reports
// unconfined); it exists so subprocess.go compiles everywhere.
func placeJobInCgroup(JobConfinement, string, int) error { return nil }
