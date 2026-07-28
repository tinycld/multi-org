package controlplane

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"

	"tinycld.org/multi-org/internal/lockfile"
)

// CheckPeerVersions enforces every resolved package's peerVersions ranges
// against the versions in the same resolved set. It is the router-side mirror
// of coreserver's solveCompat (the Setup → Versions screen): the in-app check
// is advisory, and an org's lockfile only becomes real here — so CreateOrg and
// Deploy refuse to materialize an incompatible set rather than boot a tenant
// whose packages disagree about their core.
//
// Semantics match the coreserver solver: a package that ships no manifest.json
// or no peerVersions block declares no requirements; a required peer absent
// from the lockfile is a violation; an unparsable range is itself a violation
// (fail closed — a typo must not read as "anything goes").
func CheckPeerVersions(resolved []lockfile.ResolvedPackage) error {
	versions := make(map[string]string, len(resolved))
	for _, pkg := range resolved {
		versions[pkg.Name] = pkg.Version
	}

	var violations []string
	for _, pkg := range resolved {
		mc, ok, err := readManifestCapabilities(pkg.Dir)
		if err != nil {
			return err
		}
		if !ok || len(mc.PeerVersions) == 0 {
			continue
		}

		peers := make([]string, 0, len(mc.PeerVersions))
		for peer := range mc.PeerVersions {
			peers = append(peers, peer)
		}
		sort.Strings(peers)

		for _, peer := range peers {
			rng := mc.PeerVersions[peer]
			found, present := versions[peer]
			if !present {
				violations = append(violations, fmt.Sprintf(
					"%s requires %s %s, which is not in the lockfile", pkg.Name, peer, rng))
				continue
			}
			constraint, err := semver.NewConstraint(rng)
			if err != nil {
				violations = append(violations, fmt.Sprintf(
					"%s declares an unparsable range for %s: %q", pkg.Name, peer, rng))
				continue
			}
			v, err := semver.NewVersion(found)
			if err != nil {
				violations = append(violations, fmt.Sprintf(
					"%s requires %s %s, but resolved version %q is not valid semver",
					pkg.Name, peer, rng, found))
				continue
			}
			if !constraint.Check(v) {
				violations = append(violations, fmt.Sprintf(
					"%s requires %s %s, found %s", pkg.Name, peer, rng, found))
			}
		}
	}

	if len(violations) > 0 {
		return fmt.Errorf("peer version check failed: %s", strings.Join(violations, "; "))
	}
	return nil
}
