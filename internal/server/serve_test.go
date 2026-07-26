package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBuildHandler_RoutesAdminAndOrg(t *testing.T) {
	adminHit := false
	h := BuildHandler(Params{
		BaseDomain:      "tinycld.org",
		ControlPlaneMux: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { adminHit = true }),
		GetOrg: func(ctx context.Context, slug string) (http.Handler, error) {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }), nil
		},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://admin.tinycld.org/_/", nil))
	if !adminHit {
		t.Fatal("admin not routed")
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://acme.tinycld.org/api/health", nil))
	if rec.Code != 204 {
		t.Fatalf("org route failed: %d", rec.Code)
	}
}
