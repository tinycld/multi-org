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

	"tinycld.org/multi-org/internal/controlplane"
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
	})
	defer mgr.Shutdown()

	prov := controlplane.NewProvisioner(cp.App, root, pkgStore, mgr.Evict)
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
