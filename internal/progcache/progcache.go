// Package progcache provides a process-wide, content-addressed cache of compiled
// sobek programs shared across all tenant apps. It implements the open
// jsvm.ProgramSource interface so identical hook/callback source across orgs
// resolves to a single *sobek.Program in memory.
//
// The cache has no eviction: entries are bounded by the number of DISTINCT
// program sources across all installed packages, which is expected to be small
// and static (identical source across many orgs collapses to one entry). Callers
// must not feed per-tenant-unique source (e.g. with an interpolated org id) into
// it, which would defeat sharing and make growth unbounded.
package progcache

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"

	"github.com/grafana/sobek"
	"github.com/pocketbase/pocketbase/plugins/jsvm"
)

type SharedProgramCache struct {
	mu    sync.RWMutex
	progs map[string]*sobek.Program // (strict, sha256(src)) -> program
}

func New() *SharedProgramCache {
	return &SharedProgramCache{progs: make(map[string]*sobek.Program)}
}

// Compile satisfies jsvm.ProgramSource. It keys the shared cache on
// (strict, sha256(src)) so identical program source across orgs resolves to a
// single *sobek.Program. strict is part of the key because the same source
// compiled sloppy vs. strict yields different programs.
func (c *SharedProgramCache) Compile(name, src string, strict bool) (*sobek.Program, error) {
	// name is a constant (defaultScriptPath) supplied by the fork and carries no
	// distinguishing information, so it is deliberately not part of the key.
	key := strictPrefix(strict) + sha256Hex(src)

	c.mu.RLock()
	if p, ok := c.progs[key]; ok {
		c.mu.RUnlock()
		return p, nil
	}
	c.mu.RUnlock()

	// Compile outside the write lock, then store under lock (idempotent — last
	// writer wins with an identical program; sobek programs are immutable).
	p, err := sobek.Compile(name, src, strict)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	if existing, ok := c.progs[key]; ok {
		c.mu.Unlock()
		return existing, nil
	}
	c.progs[key] = p
	c.mu.Unlock()
	return p, nil
}

func (c *SharedProgramCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.progs)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// strictPrefix namespaces the cache key by strict mode so the same source
// compiled sloppy vs. strict never collides.
func strictPrefix(strict bool) string {
	if strict {
		return "s:"
	}
	return "n:"
}

// Ensure SharedProgramCache implements the fork's jsvm.ProgramSource interface.
// Placed here so a signature drift in the fork breaks this build loudly.
var _ jsvm.ProgramSource = (*SharedProgramCache)(nil)
