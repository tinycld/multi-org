package controlplane

import (
	"encoding/json"
	"os"
	"path/filepath"

	"tinycld.org/core/carddav"
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
// The Sources returned carry no Hooks: authorization, quota and versioning are
// Go callbacks that cannot cross the process boundary, so a tenant serves the
// tree with core's default access model (authenticated, unrestricted per item).
// A package needing per-item authorization inside a tenant must express it in
// the collection's PocketBase rules. See HANDOFF.
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
