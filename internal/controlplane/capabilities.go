package controlplane

import (
	"encoding/json"
	"os"
	"path/filepath"

	"tinycld.org/core/caldav"
	"tinycld.org/core/carddav"
	"tinycld.org/core/quota"
	"tinycld.org/core/webdav"
	"tinycld.org/multi-org/internal/lockfile"
)

// manifestCapabilities is the subset of a package manifest the host reads to wire
// host-side protocol capabilities. Mirrors the tinycld manifest `carddav` block.
type manifestCapabilities struct {
	Slug    string `json:"slug"`
	CardDAV *struct {
		Collection      string `json:"collection"`
		ListFilter      string `json:"listFilter"`
		Sort            string `json:"sort"`
		OwnerField      string `json:"ownerField"`
		UIDField        string `json:"uidField"`
		SoftDeleteField string `json:"softDeleteField"`
		VCard           struct {
			Version string `json:"version"`
			Name    struct {
				Given  string `json:"given"`
				Family string `json:"family"`
			} `json:"name"`
			Simple   map[string]string `json:"simple"`
			RevField string            `json:"revField"`
		} `json:"vcard"`
	} `json:"carddav"`
	WebDAV *struct {
		Prefix     string `json:"prefix"`
		Collection string `json:"collection"`
		Fields     struct {
			Name     string `json:"name"`
			Parent   string `json:"parent"`
			IsFolder string `json:"isFolder"`
			Size     string `json:"size"`
			MimeType string `json:"mimeType"`
			File     string `json:"file"`
			Owner    string `json:"owner"`
			Updated  string `json:"updated"`
		} `json:"fields"`
	} `json:"webdav"`
	CalDAV *struct {
		Prefix             string `json:"prefix"`
		CalendarCollection string `json:"calendarCollection"`
		EventCollection    string `json:"eventCollection"`
		Calendar           struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"calendar"`
		Event struct {
			Calendar    string         `json:"calendar"`
			UID         string         `json:"uid"`
			Owner       string         `json:"owner"`
			Title       string         `json:"title"`
			Description string         `json:"description"`
			Location    string         `json:"location"`
			Start       string         `json:"start"`
			End         string         `json:"end"`
			AllDay      string         `json:"allDay"`
			Recurrence  string         `json:"recurrence"`
			Guests      string         `json:"guests"`
			Reminder    string         `json:"reminder"`
			BusyStatus  string         `json:"busyStatus"`
			Visibility  string         `json:"visibility"`
			Updated     string         `json:"updated"`
			Created     string         `json:"created"`
			Defaults    map[string]any `json:"defaults"`
		} `json:"event"`
	} `json:"caldav"`
	Quota []struct {
		Collection string `json:"collection"`
		SizeField  string `json:"sizeField"`
		OwnerField string `json:"ownerField"`
	} `json:"quota"`
}

// CardDAVSources reads each resolved package's materialized manifest.json and
// returns the carddav.Source for every package that declares a `carddav` block.
// This is the orgmanager.Config.CardDAVSources hook: the host serves CardDAV over
// the tenant's own DB (single-org scope), driven purely by package config — no
// feature Go. Packages without a manifest.json or a carddav block are skipped.
func CardDAVSources(resolved []lockfile.ResolvedPackage) ([]carddav.Source, error) {
	var sources []carddav.Source
	for _, pkg := range resolved {
		mc, ok, err := readManifestCapabilities(pkg.Dir)
		if err != nil {
			return nil, err
		}
		if !ok || mc.CardDAV == nil {
			continue
		}
		cd := mc.CardDAV
		slug := mc.Slug
		if slug == "" {
			slug = pkg.Name
		}
		sources = append(sources, carddav.Source{
			Slug:            slug,
			Collection:      cd.Collection,
			ListFilter:      cd.ListFilter,
			Sort:            cd.Sort,
			OwnerField:      cd.OwnerField,
			UIDField:        cd.UIDField,
			SoftDeleteField: cd.SoftDeleteField,
			VCard: carddav.VCardMap{
				Version: cd.VCard.Version,
				Name: carddav.NameMap{
					Given:  cd.VCard.Name.Given,
					Family: cd.VCard.Name.Family,
				},
				Simple:   cd.VCard.Simple,
				RevField: cd.VCard.RevField,
			},
		})
	}
	return sources, nil
}

// WebDAVSources reads each resolved package's materialized manifest.json and
// returns the webdav.Source for every package that declares a `webdav` block.
// This is the orgmanager.Config.WebDAVSources hook.
//
// The Sources returned carry no Hooks, because a Go closure cannot cross a
// process boundary. That no longer costs authorization or quota: core evaluates
// the collection's own PocketBase rules (a string, which travels in the schema)
// and core/quota enforces ceilings as record hooks, so a tenant enforces exactly
// what the single-org app does. What a tenant loses is the version snapshot on
// overwrite — history, not safety. See HANDOFF.
func WebDAVSources(resolved []lockfile.ResolvedPackage) ([]webdav.Source, error) {
	var sources []webdav.Source
	for _, pkg := range resolved {
		mc, ok, err := readManifestCapabilities(pkg.Dir)
		if err != nil {
			return nil, err
		}
		if !ok || mc.WebDAV == nil {
			continue
		}
		wd := mc.WebDAV
		slug := mc.Slug
		if slug == "" {
			slug = pkg.Name
		}
		sources = append(sources, webdav.Source{
			Slug:       slug,
			Prefix:     wd.Prefix,
			Collection: wd.Collection,
			Fields: webdav.FieldMap{
				Name:     wd.Fields.Name,
				Parent:   wd.Fields.Parent,
				IsFolder: wd.Fields.IsFolder,
				Size:     wd.Fields.Size,
				MimeType: wd.Fields.MimeType,
				File:     wd.Fields.File,
				Owner:    wd.Fields.Owner,
				Updated:  wd.Fields.Updated,
			},
		})
	}
	return sources, nil
}

// defaultCalDAVPrefix is where a calendar tree mounts when a manifest omits
// `prefix`. Clients probe /.well-known/caldav and follow the redirect, so the
// value only has to be stable, but it must be non-empty: core builds every
// calendar and object URL from it.
const defaultCalDAVPrefix = "/caldav"

// CalDAVSources reads each resolved package's materialized manifest.json and
// returns the caldav.Source for every package that declares a `caldav` block.
// This is the orgmanager.Config.CalDAVSources hook.
//
// As with WebDAV, the Sources carry no Go callbacks: authorization comes from
// the calendar/event collections' own PocketBase rules, which core evaluates
// with app.CanAccessRecord, so a tenant enforces the same membership and
// viewer/editor split as the single-org app. Source.OnError is likewise absent —
// a tenant has no Sentry hub to report into.
func CalDAVSources(resolved []lockfile.ResolvedPackage) ([]caldav.Source, error) {
	var sources []caldav.Source
	for _, pkg := range resolved {
		mc, ok, err := readManifestCapabilities(pkg.Dir)
		if err != nil {
			return nil, err
		}
		if !ok || mc.CalDAV == nil {
			continue
		}
		cd := mc.CalDAV
		slug := mc.Slug
		if slug == "" {
			slug = pkg.Name
		}
		prefix := cd.Prefix
		if prefix == "" {
			prefix = defaultCalDAVPrefix
		}
		sources = append(sources, caldav.Source{
			Slug:               slug,
			Prefix:             prefix,
			CalendarCollection: cd.CalendarCollection,
			EventCollection:    cd.EventCollection,
			Calendar: caldav.CalendarMap{
				Name:        cd.Calendar.Name,
				Description: cd.Calendar.Description,
			},
			Event: caldav.EventMap{
				Calendar:    cd.Event.Calendar,
				UID:         cd.Event.UID,
				Owner:       cd.Event.Owner,
				Title:       cd.Event.Title,
				Description: cd.Event.Description,
				Location:    cd.Event.Location,
				Start:       cd.Event.Start,
				End:         cd.Event.End,
				AllDay:      cd.Event.AllDay,
				Recurrence:  cd.Event.Recurrence,
				Guests:      cd.Event.Guests,
				Reminder:    cd.Event.Reminder,
				BusyStatus:  cd.Event.BusyStatus,
				Visibility:  cd.Event.Visibility,
				Updated:     cd.Event.Updated,
				Created:     cd.Event.Created,
				Defaults:    cd.Event.Defaults,
			},
		})
	}
	return sources, nil
}

// QuotaSources reads each resolved package's materialized manifest.json and
// returns the quota.Source for every collection a package declares as
// storage-bearing.
//
// Unlike the DAV sources these are pure data with no Go counterpart, so a
// tenant enforces exactly what the single-org app does. A source with no
// ownerField counts toward the org ceiling only — shared data has nobody to
// charge.
func QuotaSources(resolved []lockfile.ResolvedPackage) ([]quota.Source, error) {
	var sources []quota.Source
	for _, pkg := range resolved {
		mc, ok, err := readManifestCapabilities(pkg.Dir)
		if err != nil {
			return nil, err
		}
		if !ok || len(mc.Quota) == 0 {
			continue
		}
		slug := mc.Slug
		if slug == "" {
			slug = pkg.Name
		}
		for _, q := range mc.Quota {
			sources = append(sources, quota.Source{
				Slug:       slug,
				Collection: q.Collection,
				SizeField:  q.SizeField,
				OwnerField: q.OwnerField,
			})
		}
	}
	return sources, nil
}

// readManifestCapabilities loads and parses <pkgDir>/manifest.json. The bool is
// false (no error) when the file is absent — a package need not ship one.
func readManifestCapabilities(pkgDir string) (manifestCapabilities, bool, error) {
	data, err := os.ReadFile(filepath.Join(pkgDir, manifestJSONFile))
	if err != nil {
		if os.IsNotExist(err) {
			return manifestCapabilities{}, false, nil
		}
		return manifestCapabilities{}, false, err
	}
	var mc manifestCapabilities
	if err := json.Unmarshal(data, &mc); err != nil {
		return manifestCapabilities{}, false, err
	}
	return mc, true, nil
}
