package controlplane

import (
	"path/filepath"
	"testing"

	"github.com/pocketbase/pocketbase/core"

	"tinycld.org/multi-org/internal/store"
)

// The MX routing registry these tests pin: org_mail_domains is the control
// plane's authoritative domain→org map. The router's :25 frontend routes every
// RCPT TO through it, so a domain must resolve to exactly one org (uniqueness
// across orgs is what stops a hostile org claiming a sibling's domain and
// stealing its mail), the shape must be a canonical lowercase FQDN (the relay
// compares case-insensitively against it), and only superusers may write it.

// mailDomainFixture builds an initialized control plane with one active org.
func mailDomainFixture(t *testing.T) (*ControlPlane, string) {
	t.Helper()
	root := t.TempDir()
	cp, _ := New(filepath.Join(root, "pb_control", "pb_data"))
	if err := cp.App.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cp.App.ResetBootstrapState() })
	if err := cpInitForTest(cp); err != nil {
		t.Fatal(err)
	}
	s := store.New(root)
	_ = s.Publish("@tinycld/core", "1.0.0", map[string][]byte{"server/a.pb.js": []byte("1")})
	p := NewProvisioner(cp.App, root, s, func(string) {}, nil)
	if _, err := p.CreateOrg("acme", "Acme", map[string]string{"@tinycld/core": "1.0.0"}); err != nil {
		t.Fatal(err)
	}
	return cp, root
}

func addMailDomain(t *testing.T, app core.App, slug, domain string) error {
	t.Helper()
	org, err := app.FindFirstRecordByData("orgs", "slug", slug)
	if err != nil {
		t.Fatalf("find org %s: %v", slug, err)
	}
	col, err := app.FindCollectionByNameOrId("org_mail_domains")
	if err != nil {
		t.Fatalf("org_mail_domains collection missing: %v", err)
	}
	rec := core.NewRecord(col)
	rec.Set("org", org.Id)
	rec.Set("domain", domain)
	return app.Save(rec)
}

func TestMailDomainLookup_ResolvesOwnerOrgCaseInsensitively(t *testing.T) {
	cp, _ := mailDomainFixture(t)
	if err := addMailDomain(t, cp.App, "acme", "mail.acme-corp.com"); err != nil {
		t.Fatalf("register domain: %v", err)
	}

	lookup := MailDomainLookup(cp.App)
	slug, ok := lookup("Mail.ACME-Corp.COM")
	if !ok || slug != "acme" {
		t.Fatalf("lookup = (%q, %v), want (acme, true)", slug, ok)
	}
	if _, ok := lookup("nobody.example"); ok {
		t.Fatal("unregistered domain must not resolve")
	}
}

func TestOrgMailDomains_DomainUniqueAcrossOrgs(t *testing.T) {
	cp, root := mailDomainFixture(t)
	s := store.New(root)
	p := NewProvisioner(cp.App, root, s, func(string) {}, nil)
	if _, err := p.CreateOrg("rival", "Rival", map[string]string{"@tinycld/core": "1.0.0"}); err != nil {
		t.Fatal(err)
	}

	if err := addMailDomain(t, cp.App, "acme", "shared.example.com"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := addMailDomain(t, cp.App, "rival", "shared.example.com"); err == nil {
		t.Fatal("a second org claiming an already-owned domain would let it steal that org's mail; must be refused")
	}
}

func TestOrgMailDomains_RejectsNonCanonicalDomains(t *testing.T) {
	cp, _ := mailDomainFixture(t)
	for _, bad := range []string{
		"UPPER.example.com", // the relay matches lowercase; storage must be canonical
		"no-dot",
		"user@host.com",
		"spaced domain.com",
		".leading.dot",
		"trailing.dot.",
	} {
		if err := addMailDomain(t, cp.App, "acme", bad); err == nil {
			t.Errorf("domain %q must be rejected", bad)
		}
	}
}
