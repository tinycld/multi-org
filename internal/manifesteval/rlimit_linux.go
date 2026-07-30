//go:build linux

package manifesteval

import (
	"fmt"
	"os"
	"syscall"
)

// memoryLimitBytes caps the evaluator child's address space. 2 GiB leaves
// the Go runtime's own virtual reservations comfortable while making a
// gigabytes-in-microseconds allocation bomb die in the child instead of
// OOMing the router. Production deployments are Linux; see rlimit_other.go
// for what a dev host gets.
const memoryLimitBytes = 2 << 30

func applyMemoryLimit() {
	limit := &syscall.Rlimit{Cur: memoryLimitBytes, Max: memoryLimitBytes}
	if err := syscall.Setrlimit(syscall.RLIMIT_AS, limit); err != nil {
		// Refuse to evaluate untrusted input unconfined: the whole point of
		// the subprocess is the limit.
		fmt.Fprintf(os.Stderr, "set memory limit: %v\n", err)
		os.Exit(1)
	}
}
