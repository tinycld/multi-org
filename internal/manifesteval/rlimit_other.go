//go:build !linux

package manifesteval

// applyMemoryLimit is a no-op off Linux: macOS does not reliably enforce
// RLIMIT_AS, and dev hosts are the only non-Linux deployments. The child
// process boundary still contains a hang (the parent's kill timeout) and the
// eval's own interrupt + size caps still apply; only the hard allocation
// ceiling is Linux-specific.
func applyMemoryLimit() {}
