package orgmanager

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPrefixMux_RoutesByPath(t *testing.T) {
	hit := func(name string, seen *string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			*seen = name
			w.WriteHeader(http.StatusNoContent)
		})
	}

	var got string
	mux := &prefixMux{
		dav:   hit("dav", &got),
		stock: hit("stock", &got),
	}

	cases := []struct{ path, want string }{
		{"/carddav/", "dav"},
		{"/carddav", "dav"},
		{"/carddav/u/ab/default/", "dav"},
		{"/.well-known/carddav", "dav"},
		{"/api/health", "stock"},
		{"/", "stock"},
		{"/api/collections/contacts/records", "stock"},
	}
	for _, tc := range cases {
		got = ""
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "http://acme.tinycld.org"+tc.path, nil))
		if got != tc.want {
			t.Errorf("path %q routed to %q, want %q", tc.path, got, tc.want)
		}
	}
}
