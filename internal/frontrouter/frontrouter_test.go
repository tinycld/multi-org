package frontrouter

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

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
		GetOrg: func(slug string) (http.Handler, error) {
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

func TestFrontRouter_UnknownOrg404(t *testing.T) {
	fr := New(Config{
		BaseDomain:      "tinycld.org",
		ControlPlaneMux: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		GetOrg:          func(slug string) (http.Handler, error) { return nil, http.ErrNoLocation },
	})
	rec := httptest.NewRecorder()
	fr.ServeHTTP(rec, httptest.NewRequest("GET", "http://ghost.tinycld.org/", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown org, got %d", rec.Code)
	}
}
