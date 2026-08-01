package controlplane

import (
	"fmt"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

// MailDomainLookup resolves a mail domain to the slug of the org that owns it,
// backed by the org_mail_domains registry. Matching is case-insensitive (SMTP
// domains are); storage is canonical lowercase (enforced by the collection's
// pattern). Like OrgLookup, this deliberately does NOT check the org's status:
// the org manager re-checks it, and the MX relay wants the distinction — an
// unknown domain is a permanent refusal (550) while a known domain whose org
// is suspended or crash-looping is a transient one (the sender should retry).
func MailDomainLookup(app core.App) func(domain string) (slug string, ok bool) {
	return func(domain string) (string, bool) {
		rec, err := app.FindFirstRecordByData("org_mail_domains", "domain", strings.ToLower(domain))
		if err != nil || rec == nil {
			return "", false
		}
		org, err := app.FindRecordById("orgs", rec.GetString("org"))
		if err != nil || org == nil {
			return "", false
		}
		return org.GetString("slug"), true
	}
}

// orgBySlug resolves a slug to its orgs record. Mail-domain operations take a
// slug (what an operator knows) rather than the relation's record id.
func (p *Provisioner) orgBySlug(slug string) (*core.Record, error) {
	rec, err := p.app.FindFirstRecordByData("orgs", "slug", slug)
	if err != nil || rec == nil {
		return nil, fmt.Errorf("org %q not found", slug)
	}
	return rec, nil
}

// AddMailDomain claims a mail domain for an org. The domain is lowercased
// before the write: the collection's pattern rejects uppercase rather than
// normalizing it (storage must be canonical for the relay's case-folded
// lookup), so accepting "Acme-Corp.com" from an operator and storing it
// canonically is friendlier than a validation error, and cannot smuggle a
// non-canonical value past the pattern.
//
// The collection's unique index on domain is what refuses a second org
// claiming a domain already owned — the guard against one org stealing a
// sibling's mail — so this deliberately does not pre-check and race it.
func (p *Provisioner) AddMailDomain(slug, domain string) (*core.Record, error) {
	org, err := p.orgBySlug(slug)
	if err != nil {
		return nil, err
	}
	col, err := p.app.FindCollectionByNameOrId("org_mail_domains")
	if err != nil {
		return nil, fmt.Errorf("find org_mail_domains collection: %w", err)
	}
	rec := core.NewRecord(col)
	rec.Set("org", org.Id)
	rec.Set("domain", strings.ToLower(strings.TrimSpace(domain)))
	if err := p.app.Save(rec); err != nil {
		return nil, fmt.Errorf("register mail domain: %w", err)
	}
	return rec, nil
}

// ListMailDomains returns the domains an org owns, as plain strings.
func (p *Provisioner) ListMailDomains(slug string) ([]string, error) {
	org, err := p.orgBySlug(slug)
	if err != nil {
		return nil, err
	}
	recs, err := p.app.FindAllRecords("org_mail_domains",
		dbx.NewExp("org = {:org}", dbx.Params{"org": org.Id}))
	if err != nil {
		return nil, fmt.Errorf("list mail domains: %w", err)
	}
	domains := make([]string, 0, len(recs))
	for _, r := range recs {
		domains = append(domains, r.GetString("domain"))
	}
	return domains, nil
}

// RemoveMailDomain releases a domain. It is scoped to the owning org so a
// mistyped slug cannot delete another org's claim.
func (p *Provisioner) RemoveMailDomain(slug, domain string) error {
	org, err := p.orgBySlug(slug)
	if err != nil {
		return err
	}
	rec, err := p.app.FindFirstRecordByData("org_mail_domains", "domain", strings.ToLower(strings.TrimSpace(domain)))
	if err != nil || rec == nil {
		return fmt.Errorf("domain %q not registered", domain)
	}
	if rec.GetString("org") != org.Id {
		return fmt.Errorf("domain %q is not owned by org %q", domain, slug)
	}
	if err := p.app.Delete(rec); err != nil {
		return fmt.Errorf("remove mail domain: %w", err)
	}
	return nil
}
