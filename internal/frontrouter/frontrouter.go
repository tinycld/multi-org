// Package frontrouter dispatches an incoming request to the control-plane app or
// a tenant org handler based on the request's subdomain.
package frontrouter

import (
	"context"
	"errors"
	"net/http"

	"tinycld.org/multi-org/internal/orgerr"
)

type Config struct {
	BaseDomain      string
	ControlPlaneMux http.Handler
	// GetOrg returns the org's handler, spawning its process if necessary, or
	// an error if the org is unknown, inactive, or cannot be brought up.
	//
	// The context carries the caller's cancellation: bringing up a cold org
	// takes seconds, and a client that hangs up in the meantime should release
	// its wait (without aborting the spawn for everyone else).
	GetOrg func(ctx context.Context, slug string) (http.Handler, error)
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
		h, err := f.cfg.GetOrg(r.Context(), sub)
		switch {
		case err == nil && h != nil:
			h.ServeHTTP(w, r)
		case errors.Is(err, orgerr.ErrOrgUnavailable):
			// Transient: the org exists and is active but its process is not
			// serving right now (spawning, wedged, or in crash backoff).
			w.Header().Set("Retry-After", "5")
			http.Error(w, "organization temporarily unavailable", http.StatusServiceUnavailable)
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			// The client gave up while we were bringing the org up. There is
			// nobody left to write a response to.
			return
		default:
			// Unknown and suspended orgs deliberately share this response: a
			// distinct status would tell an unauthenticated prober which slugs
			// exist.
			http.Error(w, "no such organization", http.StatusNotFound)
		}
	}
}
