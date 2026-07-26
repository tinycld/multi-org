// Package davconfig is the wire format for the CardDAV source list the host
// hands to a tenant process.
//
// It mirrors tinycld.org/core/carddav's Source/VCardMap/NameMap rather than
// marshalling those types directly: they carry no JSON tags, so encoding them
// would silently depend on Go field names staying stable across a repo
// boundary. controlplane/capabilities.go already mirrors the manifest shape for
// the same reason.
package davconfig

import "tinycld.org/core/carddav"

type Source struct {
	Slug            string   `json:"slug"`
	Collection      string   `json:"collection"`
	ListFilter      string   `json:"listFilter"`
	Sort            string   `json:"sort"`
	OwnerField      string   `json:"ownerField"`
	UIDField        string   `json:"uidField"`
	SoftDeleteField string   `json:"softDeleteField"`
	VCard           VCardMap `json:"vcard"`
}

type VCardMap struct {
	Version  string            `json:"version"`
	Name     NameMap           `json:"name"`
	Simple   map[string]string `json:"simple"`
	RevField string            `json:"revField"`
}

type NameMap struct {
	Given  string `json:"given"`
	Family string `json:"family"`
}

// Encode converts core's Sources into the wire form.
func Encode(sources []carddav.Source) []Source {
	out := make([]Source, 0, len(sources))
	for _, s := range sources {
		out = append(out, Source{
			Slug:            s.Slug,
			Collection:      s.Collection,
			ListFilter:      s.ListFilter,
			Sort:            s.Sort,
			OwnerField:      s.OwnerField,
			UIDField:        s.UIDField,
			SoftDeleteField: s.SoftDeleteField,
			VCard: VCardMap{
				Version:  s.VCard.Version,
				Name:     NameMap{Given: s.VCard.Name.Given, Family: s.VCard.Name.Family},
				Simple:   s.VCard.Simple,
				RevField: s.VCard.RevField,
			},
		})
	}
	return out
}

// Decode converts the wire form back into core's Sources.
func Decode(sources []Source) []carddav.Source {
	out := make([]carddav.Source, 0, len(sources))
	for _, s := range sources {
		out = append(out, carddav.Source{
			Slug:            s.Slug,
			Collection:      s.Collection,
			ListFilter:      s.ListFilter,
			Sort:            s.Sort,
			OwnerField:      s.OwnerField,
			UIDField:        s.UIDField,
			SoftDeleteField: s.SoftDeleteField,
			VCard: carddav.VCardMap{
				Version:  s.VCard.Version,
				Name:     carddav.NameMap{Given: s.VCard.Name.Given, Family: s.VCard.Name.Family},
				Simple:   s.VCard.Simple,
				RevField: s.VCard.RevField,
			},
		})
	}
	return out
}
