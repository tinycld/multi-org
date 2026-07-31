//go:build !linux

package builder

import "testing"

// The real TestConfinementBuilderJob_* suite is //go:build linux, so on
// darwin `-run TestConfinement` would print "no tests to run" and exit 0 — a
// vacuous pass that reads like coverage. This stub makes the output tell the
// truth: the boundary is only ever proven on Linux with root (see
// .github/workflows/confinement.yml).
func TestConfinement_RequiresLinux(t *testing.T) {
	t.Skip("confinement tests require Linux + root; they run in the confinement CI workflow")
}
