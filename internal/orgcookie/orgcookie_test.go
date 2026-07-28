package orgcookie

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func decodeValue(t *testing.T, value string) []Entry {
	t.Helper()
	raw, err := url.QueryUnescape(value)
	if err != nil {
		t.Fatalf("unescape: %v", err)
	}
	var entries []Entry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		t.Fatalf("unmarshal %q: %v", raw, err)
	}
	return entries
}

func TestMerge_AddsToEmpty(t *testing.T) {
	v := Merge("", Entry{Slug: "acme", Name: "Acme Inc", URL: "https://acme.tinycld.org"})
	entries := decodeValue(t, v)
	if len(entries) != 1 || entries[0].Slug != "acme" || entries[0].Name != "Acme Inc" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestMerge_UpsertsAndFronts(t *testing.T) {
	v := Merge("", Entry{Slug: "acme", Name: "Old Name", URL: "https://acme.tinycld.org"})
	v = Merge(v, Entry{Slug: "beta", Name: "Beta", URL: "https://beta.tinycld.org"})
	// Re-auth on acme with a renamed org: entry updates and moves to front.
	v = Merge(v, Entry{Slug: "acme", Name: "New Name", URL: "https://acme.tinycld.org"})

	entries := decodeValue(t, v)
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %+v", entries)
	}
	if entries[0].Slug != "acme" || entries[0].Name != "New Name" {
		t.Fatalf("upserted entry should lead with the new name: %+v", entries[0])
	}
	if entries[1].Slug != "beta" {
		t.Fatalf("existing entry lost: %+v", entries)
	}
}

func TestMerge_DiscardsMalformedExisting(t *testing.T) {
	v := Merge("%%%not-a-cookie", Entry{Slug: "acme", Name: "Acme", URL: "https://acme.tinycld.org"})
	entries := decodeValue(t, v)
	if len(entries) != 1 || entries[0].Slug != "acme" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestMerge_CapsEntries(t *testing.T) {
	v := ""
	for i := 0; i < 30; i++ {
		slug := "org" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		v = Merge(v, Entry{Slug: slug, Name: slug, URL: "https://" + slug + ".tinycld.org"})
	}
	if entries := decodeValue(t, v); len(entries) > 20 {
		t.Fatalf("cookie grew past the cap: %d entries", len(entries))
	}
}

func TestCookie_ParentDomainScopedAndReadable(t *testing.T) {
	c := Cookie("v", "tinycld.org")
	if c.Domain != ".tinycld.org" || c.Path != "/" {
		t.Fatalf("cookie scope: %+v", c)
	}
	if c.HttpOnly {
		t.Fatal("must NOT be HttpOnly — the switcher UI reads it from document.cookie")
	}
	if !c.Secure {
		t.Fatal("must be Secure — it only travels over the router's https origins")
	}
	if !strings.Contains(c.String(), Name+"=") {
		t.Fatalf("serialized cookie: %s", c.String())
	}
}
