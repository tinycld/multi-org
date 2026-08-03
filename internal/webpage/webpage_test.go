package webpage

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func htmlReq(url string) *http.Request {
	r := httptest.NewRequest("GET", url, nil)
	r.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	return r
}

func apiReq(url string) *http.Request {
	return httptest.NewRequest("GET", url, nil) // fetch's default Accept: */*
}

func TestWantsHTML(t *testing.T) {
	if !WantsHTML(htmlReq("http://acme.tinycld.org/")) {
		t.Fatal("browser navigation should want HTML")
	}
	if WantsHTML(apiReq("http://acme.tinycld.org/api/health")) {
		t.Fatal("Accept: */* is not a browser navigation")
	}
	post := httptest.NewRequest("POST", "http://acme.tinycld.org/", nil)
	post.Header.Set("Accept", "text/html")
	if WantsHTML(post) {
		t.Fatal("a POST must not receive an auto-refreshing page")
	}
}

// Every branded page must carry the pieces that make it an interstitial rather
// than a dead end: branding, no-store, and (where transient) an auto-refresh.
func TestUnavailable_HTMLInterstitial(t *testing.T) {
	rec := httptest.NewRecorder()
	Unavailable(rec, htmlReq("http://acme.tinycld.org/"))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("Cache-Control = %q, want no-store (a cached 503 outlives the outage)", cc)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatal("interstitial must auto-refresh")
	}
	if !strings.Contains(body, "TinyCld") {
		t.Fatal("interstitial must be branded")
	}
}

func TestUnavailable_JSONForAPIClients(t *testing.T) {
	rec := httptest.NewRecorder()
	Unavailable(rec, apiReq("http://acme.tinycld.org/api/collections/x/records"))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var body struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body.Code != 503 || body.Message == "" {
		t.Fatalf("body = %+v, want code 503 and a message", body)
	}
}

func TestRestartingAndBackendUnavailable(t *testing.T) {
	rec := httptest.NewRecorder()
	Restarting(rec, htmlReq("http://acme.tinycld.org/"))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `http-equiv="refresh"`) {
		t.Fatalf("restarting: code=%d, want auto-refreshing 503", rec.Code)
	}

	rec = httptest.NewRecorder()
	BackendUnavailable(rec, htmlReq("http://acme.tinycld.org/"))
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), `http-equiv="refresh"`) {
		t.Fatalf("backend unavailable: code=%d, want auto-refreshing 502", rec.Code)
	}

	rec = httptest.NewRecorder()
	BackendUnavailable(rec, apiReq("http://acme.tinycld.org/api/x"))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("API client got Content-Type %q, want application/json", ct)
	}
}

func TestNotFound(t *testing.T) {
	rec := httptest.NewRecorder()
	NotFound(rec, htmlReq("http://ghost.tinycld.org/"), "tinycld.org")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatal("a 404 must not auto-refresh — the org will not appear")
	}
	if !strings.Contains(body, "https://tinycld.org") {
		t.Fatal("404 page should link to the apex org finder")
	}

	rec = httptest.NewRecorder()
	NotFound(rec, apiReq("http://ghost.tinycld.org/api/x"), "tinycld.org")
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("API client got Content-Type %q, want application/json", ct)
	}
}

// The apex finder page is what breaks the old apex→apex redirect loop: it must
// answer 200 with a page that can list the browser's known orgs (switcher
// cookie) and navigate to a typed slug.
func TestServeApex(t *testing.T) {
	rec := httptest.NewRecorder()
	ServeApex(rec, htmlReq("http://tinycld.org/"), "tinycld.org")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"tinycld_orgs", "<form", "tinycld.org"} {
		if !strings.Contains(body, want) {
			t.Fatalf("apex page missing %q", want)
		}
	}
}

// The page derives org URLs from location.hostname, never from the cookie —
// the cookie is client-writable, so a stored URL would be an attacker-
// controlled navigation target (same invariant as orgcookie/org-cookie.ts).
func TestServeApex_DerivesURLsFromOwnHost(t *testing.T) {
	rec := httptest.NewRecorder()
	ServeApex(rec, htmlReq("http://tinycld.org/"), "tinycld.org")
	body := rec.Body.String()
	if !strings.Contains(body, "location.hostname") {
		t.Fatal("apex page must derive org URLs from its own hostname")
	}
	if strings.Contains(body, "e.url") || strings.Contains(body, "entry.url") {
		t.Fatal("apex page must not read a url field out of the cookie")
	}
}

// The apex used to answer the org-finder page with 200 for EVERY path, /api/*
// included. That made it indistinguishable from a healthy server: a client
// probing /api/health got 200 back, admitted the apex as its server, and then
// fed every subsequent API call HTML instead of JSON.
func TestServeApex_APIPathsAreNotThePage(t *testing.T) {
	for _, path := range []string{"/api/health", "/api/org-info", "/api/collections/x/records", "/api"} {
		rec := httptest.NewRecorder()
		ServeApex(rec, apiReq("http://tinycld.org"+path), "tinycld.org")

		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404 (the apex hosts no API)", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s: Content-Type = %q, want application/json", path, ct)
		}
		if body := rec.Body.String(); strings.Contains(body, "<!doctype") {
			t.Errorf("%s: answered with the HTML page", path)
		}
	}
}

// A client probing an unknown address must tell three cases apart: a real
// server, an apex, and a host that is down or wrong. A bare 404 collapses the
// last two, so the apex carries a marker the client can key on.
func TestServeApex_APIErrorCarriesApexMarker(t *testing.T) {
	rec := httptest.NewRecorder()
	ServeApex(rec, apiReq("http://tinycld.org/api/org-info"), "tinycld.org")

	var body struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Kind string `json:"kind"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if body.Data.Kind != ApexMarker {
		t.Fatalf("data.kind = %q, want %q", body.Data.Kind, ApexMarker)
	}
	// Still a well-formed PocketBase error, so existing client handling parses it.
	if body.Code != http.StatusNotFound || body.Message == "" {
		t.Fatalf("not a PocketBase-shaped error: %+v", body)
	}
}

// An /api/* path is an API call whatever the caller claims to accept — a
// browser typing the URL must not get the page there either.
func TestServeApex_APIPathIgnoresAcceptHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	ServeApex(rec, htmlReq("http://tinycld.org/api/org-info"), "tinycld.org")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 even for Accept: text/html", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
}

// The page itself must be unaffected — non-API paths still serve the finder.
func TestServeApex_NonAPIPathsStillServeThePage(t *testing.T) {
	for _, path := range []string{"/", "/anything", "/apifoo"} {
		rec := httptest.NewRecorder()
		ServeApex(rec, htmlReq("http://tinycld.org"+path), "tinycld.org")
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "<form") {
			t.Errorf("%s: should serve the org-finder page", path)
		}
	}
}

// The apex marker is a CROSS-REPO WIRE CONTRACT, not an internal name. The app
// shell matches this exact string (APEX_MARKER in core/lib/apex.ts) to tell an
// apex apart from a host that is merely wrong or down.
//
// Asserted as a LITERAL rather than against the constant on purpose: a test
// that reads `body.Data.Kind != ApexMarker` passes no matter what the constant
// is changed to, so renaming the value would silently break every client while
// the suite stayed green. Changing this string is a breaking protocol change —
// old apps must keep working, so ship the new value alongside the old one and
// only retire it once no deployed client relies on it.
func TestApexMarker_WireValueIsStable(t *testing.T) {
	const wireValue = "multi_org_apex"

	if ApexMarker != wireValue {
		t.Fatalf("ApexMarker = %q, want %q — the app shell matches this literal "+
			"(core/lib/apex.ts APEX_MARKER); changing it breaks every deployed client",
			ApexMarker, wireValue)
	}

	// And the value actually reaches the wire in the field the client reads.
	rec := httptest.NewRecorder()
	ServeApex(rec, apiReq("http://tinycld.org/api/org-info"), "tinycld.org")

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("apex API body is not valid JSON: %v", err)
	}
	data, ok := body["data"].(map[string]any)
	if !ok {
		t.Fatalf("no `data` object in apex API body: %s", rec.Body.String())
	}
	if data["kind"] != wireValue {
		t.Fatalf("data.kind = %v, want %q", data["kind"], wireValue)
	}
}
