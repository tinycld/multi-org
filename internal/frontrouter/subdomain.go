package frontrouter

import "strings"

// Subdomain returns the left-most label of host relative to base, or "" if host
// equals base (apex). Port is stripped. "acme.tinycld.org"/"tinycld.org" -> "acme".
func Subdomain(host, base string) string {
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	host = strings.TrimSuffix(host, ".")
	if host == base {
		return ""
	}
	suffix := "." + base
	if !strings.HasSuffix(host, suffix) {
		return ""
	}
	label := strings.TrimSuffix(host, suffix)
	// only the left-most label (ignore deeper nesting for now)
	if i := strings.LastIndexByte(label, '.'); i >= 0 {
		label = label[i+1:]
	}
	return label
}
