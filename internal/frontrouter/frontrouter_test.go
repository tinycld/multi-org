package frontrouter

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"tinycld.org/multi-org/internal/orgerr"
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
		})
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
