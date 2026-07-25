package controlplane

import (
	"encoding/json"
	"os"
	"path/filepath"

	"tinycld.org/core/carddav"
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
