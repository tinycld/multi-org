// Package server owns the single http.Server that fronts all orgs: wildcard TLS,
// the front router, and graceful shutdown.
package server

import (
	"context"
	"crypto/tls"
	"net/http"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"tinycld.org/multitenant/internal/frontrouter"
)

// Params configures the fronting server.
type Params struct {
	BaseDomain      string
	ControlPlaneMux http.Handler
	GetOrg          func(slug string) (http.Handler, error)
}

// BuildHandler assembles the front router handler (no server) — unit-testable.
func BuildHandler(p Params) http.Handler {
	return frontrouter.New(frontrouter.Config{
		BaseDomain:      p.BaseDomain,
		ControlPlaneMux: p.ControlPlaneMux,
		GetOrg:          p.GetOrg,
	})
}

// Serve starts the single HTTPS server with wildcard autocert for *.BaseDomain
// and blocks until ctx is cancelled, then gracefully shuts down. Returns nil on
// a clean shutdown.
func Serve(ctx context.Context, addr string, p Params) error {
	handler := BuildHandler(p)

	certManager := &autocert.Manager{
		Prompt: autocert.AcceptTOS,
		Cache:  autocert.DirCache("./pb_control/autocert"),
		// NOTE: a wildcard *.BaseDomain certificate requires a DNS-01 solver;
		// HTTP-01 cannot issue wildcards. Operators must supply a DNS-01 solver
		// or a pre-issued *.BaseDomain cert. HostPolicy permits the apex and any
		// subdomain (the front router decides what is valid per-request).
		HostPolicy: func(_ context.Context, host string) error { return nil },
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      5 * time.Minute,
		ReadHeaderTimeout: time.Minute,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: certManager.GetCertificate},
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	err := srv.ListenAndServeTLS("", "")
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
