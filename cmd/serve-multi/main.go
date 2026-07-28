// Command serve-multi boots the control plane, the org manager, and the single
// fronting server that hosts many orgs by subdomain.
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"

	"tinycld.org/core/mailproto"
	"tinycld.org/multi-org/internal/controlplane"
	"tinycld.org/multi-org/internal/mailrouter"
	"tinycld.org/multi-org/internal/orgmanager"
	"tinycld.org/multi-org/internal/server"
	"tinycld.org/multi-org/internal/store"
)

func main() {
	// run() owns every deferred cleanup, including reaping tenant processes.
	// main must therefore not call log.Fatal/os.Exit for anything that happens
	// after a tenant could exist — those skip defers and orphan the children.
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	root := getenv("MT_ROOT", "./mt_data")
	baseDomain := getenv("MT_BASE_DOMAIN", "tinycld.org")
	addr := getenv("MT_ADDR", ":443")
	tlsMode := server.TLSMode(getenv("MT_TLS_MODE", string(server.TLSProxy)))
	tlsCert := os.Getenv("MT_TLS_CERT")
	tlsKey := os.Getenv("MT_TLS_KEY")

	// The tenant binary defaults to a sibling of this one, so a deployed pair
	// stays together without configuration.
	tenantBinary := getenv("MT_TENANT_BINARY", defaultTenantBinary())

	cp, err := controlplane.New(filepath.Join(root, "pb_control", "pb_data"))
	if err != nil {
		return fmt.Errorf("control plane: %w", err)
	}
	// Init bootstraps + runs system migrations + applies the control-plane schema
	// (app-scoped, so it never leaks into tenant DBs).
	if err := cp.Init(); err != nil {
		return fmt.Errorf("init control plane: %w", err)
	}
	defer cp.App.ResetBootstrapState()

	if err := ensureSuperuser(cp.App); err != nil {
		return fmt.Errorf("bootstrap superuser: %w", err)
	}

	pkgStore := store.New(root)

	mgr := orgmanager.New(orgmanager.Config{
		Root:           root,
		Store:          pkgStore,
		LookupOrg:      controlplane.OrgLookup(cp.App),
		HooksPool:      15,
		MaxIdle:        30 * time.Minute,
		TenantBinary:   tenantBinary,
		Logger:         slog.Default(),
		CardDAVSources: controlplane.CardDAVSources,
		WebDAVSources:  controlplane.WebDAVSources,
		CalDAVSources:  controlplane.CalDAVSources,
		QuotaSources:   controlplane.QuotaSources,
		PackageSlugs:   controlplane.PackageSlugs,
		// Public scheme is https in every TLS mode: file/autocert terminate
		// here, and proxy mode's documented deployment is behind an external
		// TLS proxy on 443.
		OrgURL:     func(slug string) string { return "https://" + slug + "." + baseDomain },
		BaseDomain: baseDomain,
		// In file/autocert mode this process terminates TLS, so the TCP peer
		// is the end client; in proxy mode the peer is the fronting LB and
		// its forwarded chain names the client (see ForwardedConfig).
		Forwarded: orgmanager.ForwardedConfig{
			Proto:        "https",
			PeerIsClient: tlsMode != server.TLSProxy,
		},
	})
	defer mgr.Shutdown()

	// The mail ports (:993/:465/:25), opt-in via MT_MAIL_PORTS_ENABLED. Same
	// lifetime rules as the manager: started before serving, shut down by
	// defer so tenants' relayed connections aren't orphaned past run().
	mailShutdown, err := startMailPorts(baseDomain, cp, mgr)
	if err != nil {
		return fmt.Errorf("mail ports: %w", err)
	}
	defer mailShutdown()

	// Provision-time verification boots the new org through the manager: the
	// first spawn applies the org's migrations inside the CONFINED tenant
	// process (the control plane never runs tenant JS), and a failure comes
	// back through the readiness handshake with the child's reason.
	prov := controlplane.NewProvisioner(cp.App, root, pkgStore, mgr.Evict,
		func(ctx context.Context, slug string) error {
			_, err := mgr.Get(ctx, slug)
			return err
		})
	prov.RegisterRoutes()

	controlMux, err := apis.BuildServeMux(cp.App, apis.ServeConfig{})
	if err != nil {
		return fmt.Errorf("control-plane mux: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("serve-multi listening on %s for *.%s (tls=%s)", addr, baseDomain, tlsMode)
	err = server.Serve(ctx, addr, server.Params{
		BaseDomain:      baseDomain,
		ControlPlaneMux: controlMux,
		GetOrg: func(ctx context.Context, slug string) (http.Handler, error) {
			inst, err := mgr.Get(ctx, slug)
			if err != nil {
				return nil, err
			}
			return inst.Mux(), nil
		},
		TLSMode:  tlsMode,
		CertFile: tlsCert,
		KeyFile:  tlsKey,
	})
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// startMailPorts boots the mail router (IMAPS/SMTPS SNI demux + the :25 MX
// frontend) when MT_MAIL_PORTS_ENABLED=true. The router must terminate mail
// TLS itself — tenants never hold the wildcard key — so a TLS source is
// REQUIRED here: MT_MAIL_TLS_CERT/MT_MAIL_TLS_KEY, falling back to
// MT_TLS_CERT/MT_TLS_KEY (the file-mode HTTPS pair). Enabled-but-unservable
// is a hard boot error, not a warning: a deployment that asked for mail ports
// and came up healthy on HTTP with mail silently absent is the classic
// misconfiguration mailproto's own startup guards exist for.
//
// Addresses default to :993/:465/:25; MT_IMAPS_ADDR/MT_SMTPS_ADDR/MT_MX_ADDR
// override, and the literal value "off" disables that one listener (e.g. an
// operator using a provider webhook for inbound keeps MX off).
func startMailPorts(baseDomain string, cp *controlplane.ControlPlane, mgr *orgmanager.OrgManager) (func(), error) {
	if os.Getenv("MT_MAIL_PORTS_ENABLED") != "true" {
		return func() {}, nil
	}

	tlsCfg, err := mailproto.ResolveTLSConfig("MT_MAIL_TLS_CERT", "MT_MAIL_TLS_KEY", "MT_TLS_CERT", "MT_TLS_KEY", nil)
	if err != nil {
		return nil, err
	}
	if tlsCfg == nil {
		return nil, fmt.Errorf("MT_MAIL_PORTS_ENABLED=true but no TLS source: "+
			"set MT_MAIL_TLS_CERT/MT_MAIL_TLS_KEY (or the MT_TLS_CERT/MT_TLS_KEY pair) "+
			"to a *.%s cert, or unset MT_MAIL_PORTS_ENABLED", baseDomain)
	}

	addr := func(env, def string) string {
		switch v := os.Getenv(env); v {
		case "":
			return def
		case "off":
			return ""
		default:
			return v
		}
	}

	r := mailrouter.New(mailrouter.Config{
		BaseDomain: baseDomain,
		GetOrg: func(ctx context.Context, slug string) (mailrouter.Tenant, error) {
			inst, err := mgr.Get(ctx, slug)
			if err != nil {
				return nil, err
			}
			return inst, nil
		},
		LookupDomain: controlplane.MailDomainLookup(cp.App),
		TLS:          tlsCfg,
		Logger:       slog.Default(),
		IMAPSAddr:    addr("MT_IMAPS_ADDR", ":993"),
		SMTPSAddr:    addr("MT_SMTPS_ADDR", ":465"),
		MXAddr:       addr("MT_MX_ADDR", ":25"),
		MXHostname:   os.Getenv("MT_MX_HOSTNAME"),
	})
	if err := r.Start(); err != nil {
		return nil, err
	}
	log.Printf("mail ports listening (imaps=%v smtps=%v mx=%v)",
		r.IMAPSListenerAddr(), r.SMTPSListenerAddr(), r.MXListenerAddr())
	return r.Shutdown, nil
}

// defaultTenantBinary resolves serve-org next to this executable.
func defaultTenantBinary() string {
	exe, err := os.Executable()
	if err != nil {
		return "serve-org"
	}
	return filepath.Join(filepath.Dir(exe), "serve-org")
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ensureSuperuser upserts a control-plane superuser from MT_SUPERUSER_EMAIL /
// MT_SUPERUSER_PASSWORD. Without a superuser the provisioning API (all routes
// guarded by RequireSuperuserAuth) is unusable on a fresh mt_data. Upsert (not
// create) keeps first-boot restarts idempotent. Absent env vars are a no-op:
// an operator who forgot them gets a clear log line here instead of silent 401s.
func ensureSuperuser(app core.App) error {
	email := os.Getenv("MT_SUPERUSER_EMAIL")
	password := os.Getenv("MT_SUPERUSER_PASSWORD")
	if email == "" || password == "" {
		log.Printf("MT_SUPERUSER_EMAIL/MT_SUPERUSER_PASSWORD not set; skipping superuser bootstrap (provisioning API will reject unauthenticated calls)")
		return nil
	}

	col, err := app.FindCachedCollectionByNameOrId(core.CollectionNameSuperusers)
	if err != nil {
		return err
	}
	su, err := app.FindAuthRecordByEmail(col, email)
	if err != nil {
		su = core.NewRecord(col)
	}
	su.SetEmail(email)
	su.SetPassword(password)
	if err := app.Save(su); err != nil {
		return err
	}
	log.Printf("control-plane superuser ready: %s", email)
	return nil
}
