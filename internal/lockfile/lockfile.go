// Package lockfile parses and resolves a per-org lockfile: the authoritative
// "installed set + versions" for one org. Resolution checks each referenced
// version exists in the package store and returns resolved store paths in a
// stable (name-sorted) order for deterministic materialization.
package lockfile

import (
	"encoding/json"
	"sort"

	"tinycld.org/multi-org/internal/store"
)

// OrgLockfile maps package name -> version.
type OrgLockfile map[string]string

// ResolvedPackage is a lockfile entry resolved to its on-disk store directory.
type ResolvedPackage struct {
	Name    string
	Version string
	Dir     string // absolute store dir for this version
}

func Parse(b []byte) (OrgLockfile, error) {
	var lf OrgLockfile
	if err := json.Unmarshal(b, &lf); err != nil {
		return nil, err
	}
	return lf, nil
}

func (lf OrgLockfile) Marshal() ([]byte, error) {
	return json.Marshal(lf)
}

// Resolve verifies every referenced version exists in the store and returns the
// resolved packages sorted by name (stable order for deterministic materialize).
func (lf OrgLockfile) Resolve(s *store.PackageStore) ([]ResolvedPackage, error) {
	names := make([]string, 0, len(lf))
	for n := range lf {
		names = append(names, n)
	}
	sort.Strings(names)

	out := make([]ResolvedPackage, 0, len(lf))
	for _, name := range names {
		version := lf[name]
		dir, err := s.VersionDir(name, version)
		if err != nil {
			return nil, err
		}
		out = append(out, ResolvedPackage{Name: name, Version: version, Dir: dir})
	}
	return out, nil
}
