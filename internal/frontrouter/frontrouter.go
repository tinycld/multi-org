// Package frontrouter dispatches an incoming request to the control-plane app or
// a tenant org handler based on the request's subdomain.
package frontrouter

import "net/http"

type Config struct {
	BaseDomain      string
	ControlPlaneMux http.Handler
	// GetOrg returns the org's handler (lazily loading it) or an error if the
	// org is unknown / not active.
	GetOrg func(slug string) (http.Handler, error)
}

type FrontRouter struct {
	cfg Config
}

func New(cfg Config) *FrontRouter { return &FrontRouter{cfg: cfg} }

func (f *FrontRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sub := Subdomain(r.Host, f.cfg.BaseDomain)
	switch sub {
	case "", "www":
		http.Redirect(w, r, "https://"+f.cfg.BaseDomain, http.StatusFound)
		return
	case "admin":
		f.cfg.ControlPlaneMux.ServeHTTP(w, r)
		return
	default:
		h, err := f.cfg.GetOrg(sub)
		if err != nil || h == nil {
			http.Error(w, "no such organization", http.StatusNotFound)
			return
		}
		h.ServeHTTP(w, r)
	}
}
