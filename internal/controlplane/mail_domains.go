package controlplane

import (
	"strings"

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
