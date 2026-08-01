package controlplane

import (
	"path/filepath"
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

// The MX routing registry these tests pin: org_mail_domains is the control
// plane's authoritative domain→org map. The router's :25 frontend routes every
// RCPT TO through it, so a domain must resolve to exactly one org (uniqueness
// across orgs is what stops a hostile org claiming a sibling's domain and
// stealing its mail), the shape must be a canonical lowercase FQDN (the relay
// compares case-insensitively against it), and only superusers may write it.

// createActiveOrg writes an artifact-backed org row directly: mail-domain
// routing reads only orgs rows, so the fixture needs no builder or artifact.
func createActiveOrg(t *testing.T, app core.App, slug string) {
	t.Helper()
	col, err := app.FindCollectionByNameOrId("orgs")
	if err != nil {
		t.Fatal(err)
	}
	rec := core.NewRecord(col)
	rec.Set("slug", slug)
	rec.Set("status", "active")
	rec.Set("data_dir", "pb_orgs/"+slug)
	rec.Set("lockfile", `{"tinycld":"1.0.0"}`)
	rec.Set("recipe_hash", hashOld)
	if err := app.Save(rec); err != nil {
		t.Fatal(err)
	}
}

// mailDomainFixture builds an initialized control plane with one active org.
func mailDomainFixture(t *testing.T) *ControlPlane {
	t.Helper()
	root := t.TempDir()
	cp, err := New(filepath.Join(root, "pb_control", "pb_data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.App.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { WaitForAppDeploys(cp.App); _ = cp.App.ResetBootstrapState() })
	if err := cpInitForTest(cp); err != nil {
		t.Fatal(err)
	}
	createActiveOrg(t, cp.App, "acme")
	return cp
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
	cp := mailDomainFixture(t)
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
	cp := mailDomainFixture(t)
	createActiveOrg(t, cp.App, "rival")

	if err := addMailDomain(t, cp.App, "acme", "shared.example.com"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := addMailDomain(t, cp.App, "rival", "shared.example.com"); err == nil {
		t.Fatal("a second org claiming an already-owned domain would let it steal that org's mail; must be refused")
	}
}

func TestOrgMailDomains_RejectsNonCanonicalDomains(t *testing.T) {
	cp := mailDomainFixture(t)
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

// mailDomainProvisioner pairs the mail-domain fixture with a Provisioner. The
// domain operations touch only the orgs/org_mail_domains rows, so no builder
// is needed.
func mailDomainProvisioner(t *testing.T) (*ControlPlane, *Provisioner) {
	t.Helper()
	cp := mailDomainFixture(t)
	return cp, NewProvisioner(cp.App, t.TempDir(), func(string) {}, nil)
}

func TestAddMailDomain_RegistersAndLists(t *testing.T) {
	cp, p := mailDomainProvisioner(t)

	if _, err := p.AddMailDomain("acme", "acme-corp.com"); err != nil {
		t.Fatalf("AddMailDomain: %v", err)
	}
	if slug, ok := MailDomainLookup(cp.App)("acme-corp.com"); !ok || slug != "acme" {
		t.Fatalf("lookup = (%q, %v), want (acme, true)", slug, ok)
	}

	domains, err := p.ListMailDomains("acme")
	if err != nil {
		t.Fatalf("ListMailDomains: %v", err)
	}
	if len(domains) != 1 || domains[0] != "acme-corp.com" {
		t.Fatalf("domains = %v, want [acme-corp.com]", domains)
	}
}

// The collection's pattern rejects uppercase outright, so the route must
// canonicalize before the write or an operator typing "Acme-Corp.com" gets a
// validation error for input the relay would have matched fine.
func TestAddMailDomain_CanonicalizesInput(t *testing.T) {
	cp, p := mailDomainProvisioner(t)

	rec, err := p.AddMailDomain("acme", "  Acme-Corp.COM  ")
	if err != nil {
		t.Fatalf("AddMailDomain: %v", err)
	}
	if got := rec.GetString("domain"); got != "acme-corp.com" {
		t.Fatalf("stored domain = %q, want acme-corp.com", got)
	}
	if _, ok := MailDomainLookup(cp.App)("acme-corp.com"); !ok {
		t.Fatal("canonicalized domain must resolve")
	}
}

// The unique index is the anti-theft guard; the route must surface it as an
// error rather than silently repointing the domain at the new claimant.
func TestAddMailDomain_RefusesDomainOwnedByAnotherOrg(t *testing.T) {
	cp, p := mailDomainProvisioner(t)
	createActiveOrg(t, cp.App, "rival")

	if _, err := p.AddMailDomain("acme", "shared.example.com"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if _, err := p.AddMailDomain("rival", "shared.example.com"); err == nil {
		t.Fatal("a second org claiming an owned domain would steal its mail; must be refused")
	}
	if slug, _ := MailDomainLookup(cp.App)("shared.example.com"); slug != "acme" {
		t.Fatalf("domain still owned by %q, want acme", slug)
	}
}

func TestAddMailDomain_UnknownOrgErrors(t *testing.T) {
	_, p := mailDomainProvisioner(t)
	if _, err := p.AddMailDomain("ghost", "ghost.example.com"); err == nil {
		t.Fatal("registering a domain to a nonexistent org must fail")
	}
}

func TestRemoveMailDomain_ReleasesDomain(t *testing.T) {
	cp, p := mailDomainProvisioner(t)
	if _, err := p.AddMailDomain("acme", "acme-corp.com"); err != nil {
		t.Fatalf("AddMailDomain: %v", err)
	}

	// Case-insensitive, matching the add path.
	if err := p.RemoveMailDomain("acme", "ACME-Corp.com"); err != nil {
		t.Fatalf("RemoveMailDomain: %v", err)
	}
	if _, ok := MailDomainLookup(cp.App)("acme-corp.com"); ok {
		t.Fatal("removed domain must stop resolving")
	}
	// Released, so another org may now claim it.
	createActiveOrg(t, cp.App, "rival")
	if _, err := p.AddMailDomain("rival", "acme-corp.com"); err != nil {
		t.Fatalf("released domain should be claimable: %v", err)
	}
}

// A mistyped slug must not delete a domain belonging to someone else.
func TestRemoveMailDomain_RefusesForeignDomain(t *testing.T) {
	cp, p := mailDomainProvisioner(t)
	createActiveOrg(t, cp.App, "rival")
	if _, err := p.AddMailDomain("acme", "acme-corp.com"); err != nil {
		t.Fatalf("AddMailDomain: %v", err)
	}

	if err := p.RemoveMailDomain("rival", "acme-corp.com"); err == nil {
		t.Fatal("removing a domain owned by another org must be refused")
	}
	if slug, ok := MailDomainLookup(cp.App)("acme-corp.com"); !ok || slug != "acme" {
		t.Fatalf("owner changed to (%q,%v) — the refused delete must be a no-op", slug, ok)
	}
}

func TestRemoveMailDomain_UnregisteredErrors(t *testing.T) {
	_, p := mailDomainProvisioner(t)
	if err := p.RemoveMailDomain("acme", "nobody.example.com"); err == nil {
		t.Fatal("removing an unregistered domain must fail")
	}
}

// Archiving an org cascades its domain rows away (CascadeDelete on the
// relation), which releases them for reuse rather than stranding them.
func TestMailDomains_CascadeOnOrgDelete(t *testing.T) {
	cp, p := mailDomainProvisioner(t)
	if _, err := p.AddMailDomain("acme", "acme-corp.com"); err != nil {
		t.Fatalf("AddMailDomain: %v", err)
	}

	org, err := cp.App.FindFirstRecordByData("orgs", "slug", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.App.Delete(org); err != nil {
		t.Fatalf("delete org: %v", err)
	}
	if _, ok := MailDomainLookup(cp.App)("acme-corp.com"); ok {
		t.Fatal("domain must not resolve after its org is deleted")
	}
}
