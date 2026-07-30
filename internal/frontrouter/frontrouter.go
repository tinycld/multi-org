// Package frontrouter dispatches an incoming request to the control-plane app or
// a tenant org handler based on the request's subdomain.
package frontrouter

import (
	"context"
	"errors"
	"net/http"
	"time"

	"tinycld.org/multi-org/internal/orgerr"
	"tinycld.org/multi-org/internal/webpage"
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

// htmlWaitBudget bounds how long a browser navigation blocks on a cold org
// before it gets the auto-refreshing interstitial instead. The spawn keeps
// running (it is not on the request's context), and each refresh re-joins it —
// so the user sees a branded "starting up" page within seconds instead of a
// blank tab for the full spawn timeout. Var, not const, for tests.
var htmlWaitBudget = 3 * time.Second

func (f *FrontRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sub := Subdomain(r.Host, f.cfg.BaseDomain)
	switch sub {
	case "":
		// The apex itself. Redirecting here loops (the redirect target IS the
		// apex); serve the org-finder page instead.
		webpage.ServeApex(w, r, f.cfg.BaseDomain)
	case "www":
		http.Redirect(w, r, "https://"+f.cfg.BaseDomain, http.StatusFound)
	case "admin":
		f.cfg.ControlPlaneMux.ServeHTTP(w, r)
	default:
		f.serveOrg(w, r, sub)
	}
}

func (f *FrontRouter) serveOrg(w http.ResponseWriter, r *http.Request, slug string) {
	ctx := r.Context()
	if webpage.WantsHTML(r) {
		bctx, cancel := context.WithTimeout(ctx, htmlWaitBudget)
		defer cancel()
		ctx = bctx
	}

	h, err := f.cfg.GetOrg(ctx, slug)
	switch {
	case err == nil && h != nil:
		h.ServeHTTP(w, r)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		if r.Context().Err() == nil && ctx.Err() != nil {
			// Our own HTML wait budget expired while the org spawns; the
			// client is still there. Show the interstitial — its refresh
			// re-enters here and joins the in-flight spawn.
			webpage.Unavailable(w, r)
			return
		}
		// The client gave up while we were bringing the org up. There is
		// nobody left to write a response to.
		return
	case errors.Is(err, orgerr.ErrOrgUnavailable):
		// Transient: the org exists and is active but its process is not
		// serving right now (spawning, wedged, or in crash backoff).
		webpage.Unavailable(w, r)
	default:
		// Unknown and suspended orgs deliberately share this response: a
		// distinct status would tell an unauthenticated prober which slugs
		// exist.
		webpage.NotFound(w, r, f.cfg.BaseDomain)
	}
}
