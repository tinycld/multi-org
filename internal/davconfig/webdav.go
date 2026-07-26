package davconfig

import (
	"tinycld.org/core/quota"
	"tinycld.org/core/webdav"
)

// The WebDAV wire format, mirroring tinycld.org/core/webdav's Source/FieldMap
// for the same reason the CardDAV mirror exists: core's types carry no JSON
// tags, so marshalling them directly would make the wire format depend on Go
// field names staying stable across a repo boundary.
//
// Note what is NOT carried: webdav.Source.Hooks. Those are Go callbacks
// (authorization, quota, versioning) and cannot cross a process boundary. A
// tenant therefore serves the tree with core's default access model — which is
// "authenticated", since a nil hook means unrestricted. Any package whose tree
// needs per-item authorization must either express it in the collection's
// PocketBase rules or accept that a tenant-served tree is broader than the
// single-tenant one. Drive is in that position today; see HANDOFF.

type WebDAVSource struct {
	Slug       string         `json:"slug"`
	Prefix     string         `json:"prefix"`
	Collection string         `json:"collection"`
	Fields     WebDAVFieldMap `json:"fields"`
}

type WebDAVFieldMap struct {
	Name     string `json:"name"`
	Parent   string `json:"parent"`
	IsFolder string `json:"isFolder"`
	Size     string `json:"size"`
	MimeType string `json:"mimeType"`
	File     string `json:"file"`
	Owner    string `json:"owner"`
	Updated  string `json:"updated"`
}

// EncodeWebDAV converts core's Sources into the wire form.
func EncodeWebDAV(sources []webdav.Source) []WebDAVSource {
	out := make([]WebDAVSource, 0, len(sources))
	for _, s := range sources {
		out = append(out, WebDAVSource{
			Slug:       s.Slug,
			Prefix:     s.Prefix,
			Collection: s.Collection,
			Fields: WebDAVFieldMap{
				Name:     s.Fields.Name,
				Parent:   s.Fields.Parent,
				IsFolder: s.Fields.IsFolder,
				Size:     s.Fields.Size,
				MimeType: s.Fields.MimeType,
				File:     s.Fields.File,
				Owner:    s.Fields.Owner,
				Updated:  s.Fields.Updated,
			},
		})
	}
	return out
}

// DecodeWebDAV converts the wire form back into core's Sources.
func DecodeWebDAV(sources []WebDAVSource) []webdav.Source {
	out := make([]webdav.Source, 0, len(sources))
	for _, s := range sources {
		out = append(out, webdav.Source{
			Slug:       s.Slug,
			Prefix:     s.Prefix,
			Collection: s.Collection,
			Fields: webdav.FieldMap{
				Name:     s.Fields.Name,
				Parent:   s.Fields.Parent,
				IsFolder: s.Fields.IsFolder,
				Size:     s.Fields.Size,
				MimeType: s.Fields.MimeType,
				File:     s.Fields.File,
				Owner:    s.Fields.Owner,
				Updated:  s.Fields.Updated,
			},
		})
	}
	return out
}

// The quota wire format. Mirrored for the same reason as the DAV ones:
// tinycld.org/core/quota's Source carries no JSON tags, so marshalling it
// directly would make this file's shape depend on Go field names staying stable
// across a repo boundary.
type QuotaSource struct {
	Slug       string `json:"slug"`
	Collection string `json:"collection"`
	SizeField  string `json:"sizeField"`
	OwnerField string `json:"ownerField"`
}

// EncodeQuota converts core's Sources into the wire form.
func EncodeQuota(sources []quota.Source) []QuotaSource {
	out := make([]QuotaSource, 0, len(sources))
	for _, s := range sources {
		out = append(out, QuotaSource{
			Slug:       s.Slug,
			Collection: s.Collection,
			SizeField:  s.SizeField,
			OwnerField: s.OwnerField,
		})
	}
	return out
}

// DecodeQuota converts the wire form back into core's Sources.
func DecodeQuota(sources []QuotaSource) []quota.Source {
	out := make([]quota.Source, 0, len(sources))
	for _, s := range sources {
		out = append(out, quota.Source{
			Slug:       s.Slug,
			Collection: s.Collection,
			SizeField:  s.SizeField,
			OwnerField: s.OwnerField,
		})
	}
	return out
}
