package builder

import (
	"time"
)

// Sweep removes committed artifacts that are not live, keeping any entry
// younger than grace. It returns the recipe hashes it removed.
//
// Liveness policy belongs to the CALLER (the router layer, which can see the
// orgs table): per the design, an artifact is live while any org's lockfile
// resolves to it, plus each org's previous build as its revert target. This
// method only supplies the mechanism — enumerate, consult, delete.
//
// The grace period is measured from BuiltAt and is load-bearing, not
// politeness: a freshly-committed artifact is unreferenced until the deploy
// that requested it repoints the org's row, so a sweep racing that window
// would delete the artifact the org is about to boot from. Callers pass a
// grace comfortably longer than a deploy takes.
func (s *Store) Sweep(live func(recipeHash string) bool, grace time.Duration, now time.Time) ([]string, error) {
	entries, err := s.List()
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, e := range entries {
		if now.Sub(e.BuiltAt) < grace {
			continue
		}
		if live(e.RecipeHash) {
			continue
		}
		if err := s.Remove(e.RecipeHash); err != nil {
			return removed, err
		}
		removed = append(removed, e.RecipeHash)
	}
	return removed, nil
}
