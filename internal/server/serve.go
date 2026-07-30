// Package server owns the single http.Server that fronts all orgs: wildcard TLS,
// the front router, and graceful shutdown.
package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"tinycld.org/multi-org/internal/frontrouter"
)

// TLSMode selects how the fronting server terminates (or defers) TLS.
type TLSMode string

const (
	// TLSProxy serves plain HTTP; an upstream reverse proxy / load balancer
	// terminates TLS. This is the default and the simplest real deployment.
	TLSProxy TLSMode = "proxy"
	// TLSFile serves HTTPS from a pre-issued cert+key on disk (e.g. a wildcard
	// *.BaseDomain cert). CertFile/KeyFile are required.
	TLSFile TLSMode = "file"
	// TLSAutocert serves HTTPS via autocert. NOTE: a wildcard *.BaseDomain cert
	// requires a DNS-01 solver; HTTP-01 cannot issue wildcards.
	TLSAutocert TLSMode = "autocert"
)

// Params configures the fronting server.
type Params struct {
	BaseDomain      string
	ControlPlaneMux http.Handler
	GetOrg          func(ctx context.Context, slug string) (http.Handler, error)

	// TLSMode selects TLS termination; empty defaults to TLSProxy.
	TLSMode TLSMode
	// CertFile/KeyFile are required when TLSMode is TLSFile.
	CertFile string
	KeyFile  string
	// AutocertCacheDir is where autocert stores issued certs. Empty defaults
	// to a directory relative to the working directory — real deployments
	// should pass a path under MT_ROOT so a service restart from a different
	// CWD does not silently re-issue everything.
	AutocertCacheDir string
}

// BuildHandler assembles the front router handler (no server) — unit-testable.
func BuildHandler(p Params) http.Handler {
	return frontrouter.New(frontrouter.Config{
		BaseDomain:      p.BaseDomain,
		ControlPlaneMux: p.ControlPlaneMux,
		GetOrg:          p.GetOrg,
	})
}

// Serve starts the single fronting server for *.BaseDomain and blocks until ctx
// is cancelled, then gracefully shuts down. TLS termination is chosen by
// p.TLSMode (proxy / file / autocert; empty ⇒ proxy). Returns nil on a clean
// shutdown.
func Serve(ctx context.Context, addr string, p Params) error {
	srv := newServer(addr, p)

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	return listen(srv, p)
}

// newServer builds the http.Server with the timeout profile a
// many-tenants-front-door needs. No ReadTimeout or WriteTimeout: PocketBase's
// /api/realtime is an SSE stream that legitimately stays open for hours, and
// drive transfers outrun any fixed budget — a whole-request timer silently
// truncates both, undoing the tenant proxy's own care (no
// ResponseHeaderTimeout, immediate flush) to keep realtime alive. Slow-loris
// defense comes from ReadHeaderTimeout; IdleTimeout reaps dead keep-alives.
func newServer(addr string, p Params) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           BuildHandler(p),
		ReadHeaderTimeout: time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
}

// autocertHostPolicy admits only the hosts this deployment serves: the apex
// and one label beneath it. Accepting every SNI would let anyone who points a
// hostname at the router spend the deployment's Let's Encrypt rate limits on
// certificates for arbitrary domains.
func autocertHostPolicy(baseDomain string) autocert.HostPolicy {
	return func(_ context.Context, host string) error {
		host = strings.TrimSuffix(host, ".")
		if host == baseDomain {
			return nil
		}
		label, ok := strings.CutSuffix(host, "."+baseDomain)
		if !ok || label == "" || strings.Contains(label, ".") {
			return fmt.Errorf("server: refusing certificate for %q: not a %s host", host, baseDomain)
		}
		return nil
	}
}

// listen starts the server with the transport dictated by p.TLSMode.
func listen(srv *http.Server, p Params) error {
	mode := p.TLSMode
	if mode == "" {
		mode = TLSProxy
	}

	var err error
	switch mode {
	case TLSProxy:
		err = srv.ListenAndServe()
	case TLSFile:
		if p.CertFile == "" || p.KeyFile == "" {
			return fmt.Errorf("server: TLS mode %q requires both CertFile and KeyFile", mode)
		}
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		err = srv.ListenAndServeTLS(p.CertFile, p.KeyFile)
	case TLSAutocert:
		// NOTE: a wildcard *.BaseDomain certificate requires a DNS-01 solver;
		// HTTP-01 cannot issue wildcards. Operators must supply a DNS-01 solver
		// or a pre-issued *.BaseDomain cert (use TLSFile for the latter).
		cacheDir := p.AutocertCacheDir
		if cacheDir == "" {
			cacheDir = "./pb_control/autocert"
		}
		certManager := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			Cache:      autocert.DirCache(cacheDir),
			HostPolicy: autocertHostPolicy(p.BaseDomain),
		}
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: certManager.GetCertificate}
		err = srv.ListenAndServeTLS("", "")
	default:
		return fmt.Errorf("server: unknown TLS mode %q (want proxy, file, or autocert)", mode)
	}

	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
