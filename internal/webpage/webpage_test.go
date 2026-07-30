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
