package frontrouter

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tinycld.org/multi-org/internal/orgerr"
)

// htmlGet mimics a browser navigation: GET with a text/html Accept header.
func htmlGet(url string) *http.Request {
	r := httptest.NewRequest("GET", url, nil)
	r.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	return r
}

func TestSubdomain(t *testing.T) {
	cases := []struct{ host, base, want string }{
		{"acme.tinycld.org", "tinycld.org", "acme"},
		{"acme.tinycld.org:443", "tinycld.org", "acme"},
		{"admin.tinycld.org", "tinycld.org", "admin"},
		{"tinycld.org", "tinycld.org", ""},
		{"www.tinycld.org", "tinycld.org", "www"},
	}
	for _, c := range cases {
		if got := Subdomain(c.host, c.base); got != c.want {
			t.Errorf("Subdomain(%q,%q)=%q want %q", c.host, c.base, got, c.want)
		}
	}
}

func TestFrontRouter_DispatchesToOrgAndControlPlane(t *testing.T) {
	adminHit := false
	admin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		adminHit = true
		w.WriteHeader(200)
	})

	orgHit := ""
	fr := New(Config{
		BaseDomain:      "tinycld.org",
		ControlPlaneMux: admin,
		GetOrg: func(ctx context.Context, slug string) (http.Handler, error) {
			orgHit = slug
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }), nil
		},
	})

	rec := httptest.NewRecorder()
	fr.ServeHTTP(rec, httptest.NewRequest("GET", "http://admin.tinycld.org/_/", nil))
	if !adminHit || rec.Code != 200 {
		t.Fatalf("admin dispatch failed: hit=%v code=%d", adminHit, rec.Code)
	}

	rec = httptest.NewRecorder()
	fr.ServeHTTP(rec, httptest.NewRequest("GET", "http://acme.tinycld.org/api/health", nil))
	if orgHit != "acme" || rec.Code != 204 {
		t.Fatalf("org dispatch failed: slug=%q code=%d", orgHit, rec.Code)
	}
}

// routerFor builds a front router whose GetOrg always fails with err.
func routerFor(err error) *FrontRouter {
	return New(Config{
		BaseDomain:      "tinycld.org",
		ControlPlaneMux: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		GetOrg:          func(ctx context.Context, slug string) (http.Handler, error) { return nil, err },
	})
}

func TestFrontRouter_ClassifiesLoadFailures(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"unknown org", fmt.Errorf("%w: %q", orgerr.ErrOrgNotFound, "ghost"), http.StatusNotFound},
		// Suspended deliberately looks identical to unknown: a distinct status
		// would tell an unauthenticated prober which org slugs exist.
		{"suspended org", fmt.Errorf("%w: %q", orgerr.ErrOrgNotActive, "acme"), http.StatusNotFound},
		{"unclassified error", http.ErrNoLocation, http.StatusNotFound},
		// Transient: the org is real and active, its process just is not up.
		{"spawn failure", fmt.Errorf("%w: boom", orgerr.ErrOrgUnavailable), http.StatusServiceUnavailable},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			routerFor(c.err).ServeHTTP(rec, httptest.NewRequest("GET", "http://acme.tinycld.org/", nil))
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d", rec.Code, c.want)
			}
			if c.want == http.StatusServiceUnavailable && rec.Header().Get("Retry-After") == "" {
				t.Fatal("expected a Retry-After header on 503")
			}
			// A non-browser client (SPA fetch, curl) gets a parseable body, not
			// unstyled prose.
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("Content-Type = %q, want application/json for a */* request", ct)
			}
		})
	}
}

// Browser navigations get the branded pages: an auto-refreshing interstitial
// for a transient failure, a dead-end page (no refresh) for an unknown org.
func TestFrontRouter_BrowserGetsBrandedPages(t *testing.T) {
	rec := httptest.NewRecorder()
	routerFor(fmt.Errorf("%w: boom", orgerr.ErrOrgUnavailable)).
		ServeHTTP(rec, htmlGet("http://acme.tinycld.org/"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(rec.Body.String(), `http-equiv="refresh"`) {
		t.Fatal("503 interstitial must auto-refresh")
	}

	rec = httptest.NewRecorder()
	routerFor(fmt.Errorf("%w: %q", orgerr.ErrOrgNotFound, "ghost")).
		ServeHTTP(rec, htmlGet("http://ghost.tinycld.org/"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `http-equiv="refresh"`) {
		t.Fatal("404 page must not auto-refresh")
	}
}

// The apex must serve the org-finder page, NOT redirect: Subdomain() returns
// "" for the apex itself, so the old 302 to https://<base> pointed the apex at
// itself — ERR_TOO_MANY_REDIRECTS on the product's front door.
func TestFrontRouter_ApexServesOrgFinder(t *testing.T) {
	fr := routerFor(nil)
	rec := httptest.NewRecorder()
	fr.ServeHTTP(rec, htmlGet("http://tinycld.org/"))
	if rec.Code != http.StatusOK {
		t.Fatalf("apex status = %d, want 200 (a redirect here loops)", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("apex redirected to %q — redirecting the apex loops", loc)
	}
	if !strings.Contains(rec.Body.String(), "<form") {
		t.Fatal("apex page should offer the go-to-org form")
	}

	// www still redirects — to the apex, which now answers 200, so no loop.
	rec = httptest.NewRecorder()
	fr.ServeHTTP(rec, htmlGet("http://www.tinycld.org/"))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://tinycld.org" {
		t.Fatalf("www: code=%d location=%q, want 302 to https://tinycld.org",
			rec.Code, rec.Header().Get("Location"))
	}
}

// A cold spawn must not hold a browser tab blank for the full spawn timeout:
// after a short bounded wait the router serves the interstitial, whose
// auto-refresh re-joins the (still in-flight) spawn.
func TestFrontRouter_HTMLWaitIsBounded(t *testing.T) {
	orig := htmlWaitBudget
	htmlWaitBudget = 30 * time.Millisecond
	defer func() { htmlWaitBudget = orig }()

	fr := New(Config{
		BaseDomain:      "tinycld.org",
		ControlPlaneMux: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		GetOrg: func(ctx context.Context, slug string) (http.Handler, error) {
			<-ctx.Done() // a spawn that outlives any bounded wait
			return nil, ctx.Err()
		},
	})

	start := time.Now()
	rec := httptest.NewRecorder()
	fr.ServeHTTP(rec, htmlGet("http://acme.tinycld.org/"))
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("browser waited %s — the HTML wait must be bounded", elapsed)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 interstitial", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `http-equiv="refresh"`) {
		t.Fatal("bounded-wait response must auto-refresh into the in-flight spawn")
	}

	// A non-HTML client keeps the blocking behavior: its fetch resolves when
	// the org comes up rather than eating a premature 503. (The request ctx
	// here expires to end the test; the router writes nothing for it.)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	rec = httptest.NewRecorder()
	fr.ServeHTTP(rec, httptest.NewRequest("GET", "http://acme.tinycld.org/api/health", nil).WithContext(ctx))
	if rec.Body.Len() != 0 {
		t.Fatalf("client-abandoned API request should get no body, got %q", rec.Body.String())
	}
}

// A client that hangs up mid-spawn leaves nothing to respond to.
func TestFrontRouter_ClientCancellationWritesNothing(t *testing.T) {
	rec := httptest.NewRecorder()
	routerFor(context.Canceled).ServeHTTP(rec, httptest.NewRequest("GET", "http://acme.tinycld.org/", nil))
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("expected no response written, got code=%d body=%q", rec.Code, rec.Body.String())
	}
}
