package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// M4: the fronting server must not cap how long a response may take to write.
// PocketBase's /api/realtime is an SSE stream that stays open for hours, and
// large drive transfers legitimately outrun any fixed budget — a WriteTimeout
// (or full-request ReadTimeout) silently truncates both, undoing the care the
// tenant proxy takes (no ResponseHeaderTimeout, FlushInterval -1) to keep
// realtime alive. Slow-loris defense stays with ReadHeaderTimeout+IdleTimeout.
func TestServerTimeouts_DoNotKillLongLivedStreams(t *testing.T) {
	srv := newServer(":0", Params{BaseDomain: "tinycld.org"})
	if srv.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %v — it truncates every SSE stream at that age", srv.WriteTimeout)
	}
	if srv.ReadTimeout != 0 {
		t.Fatalf("ReadTimeout = %v — it truncates large uploads at that age", srv.ReadTimeout)
	}
	if srv.ReadHeaderTimeout == 0 {
		t.Fatal("ReadHeaderTimeout unset — nothing bounds a slow-loris header dribble")
	}
	if srv.IdleTimeout == 0 {
		t.Fatal("IdleTimeout unset — dead keep-alive conns accumulate forever")
	}
}

// M5: autocert must only answer for hosts this deployment actually serves.
// A HostPolicy that accepts every SNI lets anyone pointing a hostname at the
// router burn the deployment's Let's Encrypt rate limits (and fill the cache)
// with certificate requests for arbitrary domains.
func TestAutocertHostPolicy_OnlyBaseDomainAndDirectSubdomains(t *testing.T) {
	policy := autocertHostPolicy("tinycld.org")

	for _, allow := range []string{"tinycld.org", "acme.tinycld.org", "www.tinycld.org"} {
		if err := policy(context.Background(), allow); err != nil {
			t.Errorf("policy(%q) = %v, want allowed", allow, err)
		}
	}
	for _, deny := range []string{"evil.example", "tinycld.org.evil.example", "a.b.tinycld.org", ""} {
		if err := policy(context.Background(), deny); err == nil {
			t.Errorf("policy(%q) allowed — free Let's Encrypt requests for anyone", deny)
		}
	}
}

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
