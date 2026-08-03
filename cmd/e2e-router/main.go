// Command e2e-router fronts several already-running tenant backends behind the
// REAL front router, so a browser can be driven across two orgs on two
// subdomains without provisioning, building artifacts, or spawning processes.
//
// Why this exists: the switcher UI is about moving between origins, and no
// single-origin stack can exercise that. The full hosted path
// (controlplane.TestHostedDeployE2E) does two real package builds and provisions
// ONE org — minutes of work, and still not two subdomains. This keeps the piece
// that actually matters for the UI (subdomain dispatch through
// frontrouter.New) and fakes the piece that does not (how a tenant came to be
// listening), by reverse-proxying to backends the caller already started.
//
// Base domain is `localhost`, so `acme.localhost` and `globex.localhost` both
// resolve to loopback in Chrome with no DNS or TLS setup — verified, and the
// reason this needs no /etc/hosts entries.
//
// Usage:
//
//	e2e-router --addr 127.0.0.1:7300 --org acme=http://127.0.0.1:7201 \
//	           --org globex=http://127.0.0.1:7202
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"

	"tinycld.org/multi-org/internal/orgerr"
	"tinycld.org/multi-org/internal/server"
)

// orgFlag collects repeated --org slug=url pairs.
type orgFlag map[string]string

func (o orgFlag) String() string { return fmt.Sprint(map[string]string(o)) }

func (o orgFlag) Set(v string) error {
	slug, raw, ok := strings.Cut(v, "=")
	if !ok {
		return fmt.Errorf("expected slug=url, got %q", v)
	}
	if _, err := url.Parse(raw); err != nil {
		return fmt.Errorf("org %q: %w", slug, err)
	}
	o[slug] = raw
	return nil
}

func main() {
	addr := flag.String("addr", "127.0.0.1:7300", "listen address")
	baseDomain := flag.String("base-domain", "localhost", "base domain for subdomain dispatch")
	orgs := orgFlag{}
	flag.Var(&orgs, "org", "slug=backendURL (repeatable)")
	flag.Parse()

	if len(orgs) == 0 {
		log.Fatal("at least one --org slug=url is required")
	}

	handlers := make(map[string]http.Handler, len(orgs))
	for slug, raw := range orgs {
		target, err := url.Parse(raw)
		if err != nil {
			log.Fatalf("org %q: %v", slug, err)
		}
		handlers[slug] = &httputil.ReverseProxy{
			Rewrite: func(r *httputil.ProxyRequest) {
				r.SetURL(target)
				// SetURL leaves the inbound Host in place; the tenant must see
				// its OWN host. PocketBase builds absolute URLs from it, so a
				// mismatch sends the SPA looking for its API on a host that is
				// not serving one.
				r.Out.Host = target.Host
				// Preserve the original host for anything that wants to know
				// which org the browser actually asked for.
				r.SetXForwarded()
			},
		}
	}

	handler := server.BuildHandler(server.Params{
		BaseDomain: *baseDomain,
		ControlPlaneMux: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "control plane not served in the e2e harness", http.StatusNotImplemented)
		}),
		GetOrg: func(_ context.Context, slug string) (http.Handler, error) {
			h, ok := handlers[slug]
			if !ok {
				// The real sentinel, so the front router renders its genuine
				// unknown-org page rather than a generic 500 — that page is
				// part of what a cross-org test may want to assert on.
				return nil, orgerr.ErrOrgNotFound
			}
			return h, nil
		},
	})

	fmt.Fprintf(os.Stderr, "[e2e-router] listening on %s, base domain %q\n", *addr, *baseDomain)
	for slug, raw := range orgs {
		fmt.Fprintf(os.Stderr, "[e2e-router]   %s.%s -> %s\n", slug, *baseDomain, raw)
	}

	if err := http.ListenAndServe(*addr, handler); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
