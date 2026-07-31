package controlplane

import (
	"encoding/json"
	"fmt"

	"github.com/pocketbase/pocketbase/core"
)

// The control-plane settings live in the one-row control_settings collection.
// The row is created on first write; zero rows reads as zero values, so a
// fresh control plane needs no seeding step.

// DefaultLockfile returns the operator-editable package set new orgs copy
// when CreateOrg is called without one (design D7). Nil when none is
// configured — CreateOrg then provisions a zero-package org, exactly the
// pre-template behavior.
func DefaultLockfile(app core.App) (map[string]string, error) {
	rec, err := settingsRecord(app, false)
	if err != nil || rec == nil {
		return nil, err
	}
	raw := rec.GetString("default_lockfile")
	if raw == "" || raw == "null" {
		return nil, nil
	}
	var lock map[string]string
	if err := json.Unmarshal([]byte(raw), &lock); err != nil {
		return nil, fmt.Errorf("control_settings.default_lockfile: %w", err)
	}
	if len(lock) == 0 {
		return nil, nil
	}
	return lock, nil
}

// SetDefaultLockfile stores the default package set. Affects new orgs only —
// an org owns its lockfile from the moment it is copied (D7's fork
// semantics).
func SetDefaultLockfile(app core.App, lock map[string]string) error {
	rec, err := settingsRecord(app, true)
	if err != nil {
		return err
	}
	body, err := json.Marshal(lock)
	if err != nil {
		return err
	}
	rec.Set("default_lockfile", string(body))
	return app.Save(rec)
}

// settingsRecord returns the singleton settings row, creating it when create
// is set. (nil, nil) when absent and not creating.
func settingsRecord(app core.App, create bool) (*core.Record, error) {
	col, err := app.FindCollectionByNameOrId("control_settings")
	if err != nil {
		return nil, err
	}
	recs, err := app.FindRecordsByFilter(col, "1=1", "-updated", 1, 0)
	if err != nil {
		return nil, err
	}
	if len(recs) > 0 {
		return recs[0], nil
	}
	if !create {
		return nil, nil
	}
	return core.NewRecord(col), nil
}
