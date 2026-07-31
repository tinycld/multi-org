package controlplane

import (
	"github.com/pocketbase/pocketbase/core"

	"tinycld.org/multi-org/internal/orgmanager"
)

// OrgLookup returns an orgmanager.LookupFunc backed by the control-plane orgs
// collection. The manager itself re-checks status, so this returns the record
// regardless of status; a missing org returns (zero, false).
func OrgLookup(app core.App) orgmanager.LookupFunc {
	return func(slug string) (orgmanager.OrgRecord, bool) {
		rec, err := app.FindFirstRecordByData("orgs", "slug", slug)
		if err != nil || rec == nil {
			return orgmanager.OrgRecord{}, false
		}
		return orgmanager.OrgRecord{
			Slug:              rec.GetString("slug"),
			Status:            rec.GetString("status"),
			Lockfile:          []byte(rec.GetString("lockfile")),
			RecipeHash:        rec.GetString("recipe_hash"),
			DisplayName:       rec.GetString("display_name"),
			StorageLimitBytes: int64(rec.GetInt("storage_limit_bytes")),
		}, true
	}
}
