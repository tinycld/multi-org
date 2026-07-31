// Package webpage renders the branded pages the router serves when it cannot
// (or will not) hand a request to a tenant: the cold-start / restart / crash
// interstitials, the unknown-org page, and the apex org-finder.
//
// Every writer negotiates on the request: a browser navigation (GET/HEAD with
// text/html in Accept) gets a styled HTML page, everything else — the SPA's
// fetches, DAV clients, curl — gets a JSON body in PocketBase's error shape so
// existing client error handling parses it. Before this package the router
// answered raw `text/plain` prose, which a blank-tab user and the SPA alike
// rendered as nothing.
package webpage

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"tinycld.org/core/orgcookie"
)

// WantsHTML reports whether the request reads as a browser navigation: only
// those get an HTML page. Method is checked because an auto-refreshing page in
// response to a POST would replay as a GET and lose the submission; fetch()
// defaults to `Accept: */*`, so the SPA's API calls never match.
func WantsHTML(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// Unavailable answers for an org that exists but is not serving right now —
// spawning, wedged, or in crash backoff. The HTML interstitial auto-refreshes:
// each refresh re-enters the front router and joins the in-flight spawn, so
// the page "turns into" the app the moment the org is up.
func Unavailable(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", "5")
	write(w, r, http.StatusServiceUnavailable, "organization temporarily unavailable", pageData{
		Title:   "Starting up — TinyCld",
		Heading: "Starting up",
		Message: "This organization is waking up. The page refreshes automatically — hang tight.",
		Refresh: 3,
		Spinner: true,
	})
}

// Restarting answers requests that race an instance's shutdown; the successor
// spawns on the next request, which the auto-refresh supplies.
func Restarting(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", "5")
	write(w, r, http.StatusServiceUnavailable, "organization is restarting", pageData{
		Title:   "Restarting — TinyCld",
		Heading: "Restarting",
		Message: "This organization is restarting. The page refreshes automatically — hang tight.",
		Refresh: 3,
		Spinner: true,
	})
}

// BackendUnavailable answers when the tenant process stopped responding
// mid-proxy (it likely crashed). Auto-refresh is still right: the supervisor
// drops a crashed instance from the map and the next request respawns it.
func BackendUnavailable(w http.ResponseWriter, r *http.Request) {
	write(w, r, http.StatusBadGateway, "organization backend unavailable", pageData{
		Title:   "Temporarily unreachable — TinyCld",
		Heading: "Temporarily unreachable",
		Message: "The organization's server did not answer. The page refreshes automatically; if this keeps happening, contact your administrator.",
		Refresh: 5,
		Spinner: true,
	})
}

// NotFound answers for a subdomain that maps to no active org. Deliberately
// identical for unknown and suspended slugs (the caller collapses both), and
// deliberately not auto-refreshing — the org will not appear. baseDomain links
// the reader to the apex org finder; empty omits the link.
func NotFound(w http.ResponseWriter, r *http.Request, baseDomain string) {
	data := pageData{
		Title:   "Organization not found — TinyCld",
		Heading: "Organization not found",
		Message: "There is no organization at this address. Check the address for typos — organization addresses are the part before the first dot.",
	}
	if baseDomain != "" {
		data.LinkHref = "https://" + baseDomain
		data.LinkText = "Find your organization"
	}
	write(w, r, http.StatusNotFound, "no such organization", data)
}

// ServeApex serves the org-finder page at the base domain. It exists so the
// apex is a recovery point rather than a redirect loop: a user who knows only
// "it's on tinycld.org" can land here, see the orgs this browser has signed
// into (the parent-domain switcher cookie), or type their org's name.
//
// The page's script derives every org URL from location.hostname — never from
// the cookie, which is client-writable and must not supply navigation targets
// (see internal/orgcookie). Slugs are validated as single DNS labels for the
// same reason.
func ServeApex(w http.ResponseWriter, r *http.Request, baseDomain string) {
	var body strings.Builder
	if err := apexBody.Execute(&body, apexData{CookieName: orgcookie.Name, BaseDomain: baseDomain}); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeHTML(w, http.StatusOK, pageData{
		Title:   "TinyCld",
		Heading: "Find your organization",
		Message: "Every organization lives at its own address. Pick one this browser knows, or enter your organization's name.",
		Body:    template.HTML(body.String()),
	})
}

// write negotiates the body shape on the request. The JSON messages keep the
// exact strings the router used to send as text/plain, so operator tooling
// matching on them keeps working.
func write(w http.ResponseWriter, r *http.Request, status int, message string, data pageData) {
	if WantsHTML(r) {
		writeHTML(w, status, data)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// PocketBase's error shape ({code, message, data}) so the SPA's existing
	// response handling parses it instead of choking on prose.
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code":    status,
		"message": message,
		"data":    struct{}{},
	})
}

func writeHTML(w http.ResponseWriter, status int, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := shell.Execute(w, data); err != nil {
		// Headers are gone; nothing useful left to do.
		_, _ = fmt.Fprint(w, data.Message)
	}
}

// pageData feeds the shared shell template.
type pageData struct {
	Title    string
	Heading  string
	Message  string
	Refresh  int  // seconds between auto-refreshes; 0 = none
	Spinner  bool // show the loading spinner above the heading
	LinkHref string
	LinkText string
	// Body is extra page-specific markup (the apex finder's list + form). It
	// is rendered from a template in this package, never from request data.
	Body template.HTML
}

// shell is the one branded document every page shares: system font stack,
// centered card, brand teal, and both color schemes via prefers-color-scheme.
var shell = template.Must(template.New("shell").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
{{if .Refresh}}<meta http-equiv="refresh" content="{{.Refresh}}">
{{end}}<title>{{.Title}}</title>
<style>
:root {
  color-scheme: light dark;
  --brand: #0b4f4a;
  --bg: #f7f7f5;
  --card: #ffffff;
  --fg: #1c1c1a;
  --muted: #6b6b66;
  --border: #e4e4e0;
}
@media (prefers-color-scheme: dark) {
  :root {
    --brand: #7fc7bf;
    --bg: #161615;
    --card: #202020;
    --fg: #ececea;
    --muted: #9a9a94;
    --border: #33332f;
  }
}
* { box-sizing: border-box; }
body {
  margin: 0;
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  background: var(--bg);
  color: var(--fg);
  font: 16px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
  padding: 24px;
}
main {
  width: 100%;
  max-width: 420px;
  background: var(--card);
  border: 1px solid var(--border);
  border-radius: 12px;
  padding: 32px;
  text-align: center;
}
.brand {
  color: var(--brand);
  font-weight: 700;
  letter-spacing: 0.02em;
  margin-bottom: 20px;
}
h1 { font-size: 20px; margin: 0 0 8px; }
p { margin: 0; color: var(--muted); font-size: 14px; }
.spinner {
  width: 28px;
  height: 28px;
  margin: 0 auto 16px;
  border: 3px solid var(--border);
  border-top-color: var(--brand);
  border-radius: 50%;
  animation: spin 0.9s linear infinite;
}
@keyframes spin { to { transform: rotate(360deg); } }
a { color: var(--brand); }
.footlink { display: inline-block; margin-top: 16px; font-size: 14px; }
.orgs { margin: 20px 0 4px; text-align: left; }
.orgs a {
  display: block;
  padding: 10px 12px;
  margin-top: 6px;
  border: 1px solid var(--border);
  border-radius: 8px;
  text-decoration: none;
  color: var(--fg);
  font-weight: 600;
}
.orgs a span { display: block; font-weight: 400; font-size: 12px; color: var(--muted); }
.orgs a:hover { border-color: var(--brand); }
.orgs-title {
  text-align: left;
  font-size: 12px;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--muted);
  margin-top: 20px;
}
form { margin-top: 20px; text-align: left; }
label { font-size: 13px; font-weight: 600; display: block; margin-bottom: 6px; }
.row { display: flex; gap: 8px; align-items: center; }
input {
  flex: 1;
  min-width: 0;
  padding: 9px 10px;
  border: 1px solid var(--border);
  border-radius: 8px;
  background: var(--bg);
  color: var(--fg);
  font-size: 14px;
}
.suffix { color: var(--muted); font-size: 14px; white-space: nowrap; }
button {
  padding: 9px 16px;
  border: 0;
  border-radius: 8px;
  background: var(--brand);
  color: #fff;
  font-size: 14px;
  font-weight: 600;
  cursor: pointer;
}
.hint { margin-top: 16px; font-size: 13px; }
[hidden] { display: none; }
.bad { color: #b3261e; font-size: 13px; margin-top: 6px; }
</style>
</head>
<body>
<main>
<div class="brand">TinyCld</div>
{{if .Spinner}}<div class="spinner" aria-hidden="true"></div>
{{end}}<h1>{{.Heading}}</h1>
<p>{{.Message}}</p>
{{.Body}}{{if .LinkHref}}<a class="footlink" href="{{.LinkHref}}">{{.LinkText}}</a>
{{end}}</main>
</body>
</html>
`))

// apexData feeds the apex finder's body template. CookieName is compile-time
// coupled to orgcookie.Name; BaseDomain only labels the input's suffix — the
// script itself uses location.hostname, so the rendered value never becomes a
// navigation target.
type apexData struct {
	CookieName string
	BaseDomain string
}

var apexBody = template.Must(template.New("apex").Parse(`
<div class="orgs-title" id="orgs-title" hidden>Your organizations</div>
<div class="orgs" id="orgs"></div>
<form id="go">
<label for="slug">Go to your organization</label>
<div class="row">
<input id="slug" name="slug" autocomplete="off" autocapitalize="none" spellcheck="false" placeholder="your-org">
<span class="suffix">.{{.BaseDomain}}</span>
<button type="submit">Go</button>
</div>
<p class="bad" id="bad" hidden>Organization names are lowercase letters, digits, and hyphens.</p>
</form>
<p class="hint">Not sure of the address? It is in your invitation email, or ask your organization's administrator.</p>
<script>
(function () {
  // Mirrors the slug validation in internal/orgcookie: exactly one lowercase
  // DNS label. The slug becomes the leftmost label of a URL this page
  // navigates to, so anything else is a planted entry.
  var SLUG = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;
  function urlFor(slug) { return 'https://' + slug + '.' + location.hostname; }

  var prefix = '{{.CookieName}}=';
  var raw = document.cookie.split(';').map(function (s) { return s.trim(); })
      .filter(function (s) { return s.indexOf(prefix) === 0; })[0];
  var entries = [];
  if (raw) {
    try { entries = JSON.parse(decodeURIComponent(raw.slice(prefix.length))); } catch (err) {}
  }
  if (!Array.isArray(entries)) entries = [];

  var box = document.getElementById('orgs');
  var shown = 0;
  entries.forEach(function (e) {
    if (shown >= 20 || !e || typeof e.slug !== 'string' || !SLUG.test(e.slug)) return;
    var a = document.createElement('a');
    a.href = urlFor(e.slug);
    a.textContent = (typeof e.name === 'string' && e.name !== '') ? e.name : e.slug;
    var addr = document.createElement('span');
    addr.textContent = e.slug + '.' + location.hostname;
    a.appendChild(addr);
    box.appendChild(a);
    shown++;
  });
  if (shown > 0) document.getElementById('orgs-title').hidden = false;

  document.getElementById('go').addEventListener('submit', function (ev) {
    ev.preventDefault();
    var slug = document.getElementById('slug').value.trim().toLowerCase();
    if (!SLUG.test(slug)) { document.getElementById('bad').hidden = false; return; }
    location.href = urlFor(slug);
  });
})();
</script>
`))
