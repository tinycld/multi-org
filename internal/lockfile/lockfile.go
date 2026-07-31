// Package lockfile parses and marshals a per-org lockfile: the authoritative
// "installed set + versions" for one org. The set always names the app shell
// (the base member) — it is built into a shared artifact by the trusted
// builder, and the org boots from that artifact (design D4).
package lockfile

import (
	"encoding/json"
)

// OrgLockfile maps package name -> version.
type OrgLockfile map[string]string

// ResolvedPackage is one feature member of an org's committed build, with Dir
// pointing at the directory holding the member's evaluated manifest (the
// artifact's manifests/<slug>/ tree) — what the host-side capability readers
// consume.
type ResolvedPackage struct {
	Name    string
	Version string
	Dir     string
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
