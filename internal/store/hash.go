package store

import (
	"crypto/sha256"
	"encoding/hex"
)

// ContentHash returns the hex sha256 of b — the content-address key for a
// package version and (via the source string) the shared program cache.
func ContentHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
