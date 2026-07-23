// Command serve-multi boots the control plane, the org manager, and the single
// fronting server that hosts many orgs by subdomain.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"

	"tinycld.org/multitenant/internal/controlplane"
	"tinycld.org/multitenant/internal/orgmanager"
	"tinycld.org/multitenant/internal/progcache"
	"tinycld.org/multitenant/internal/server"
	"tinycld.org/multitenant/internal/store"
)

func main() {
	root := getenv("MT_ROOT", "./mt_data")
	baseDomain := getenv("MT_BASE_DOMAIN", "tinycld.org")
	addr := getenv("MT_ADDR", ":443")
	tlsMode := server.TLSMode(getenv("MT_TLS_MODE", string(server.TLSProxy)))
	tlsCert := os.Getenv("MT_TLS_CERT")
	tlsKey := os.Getenv("MT_TLS_KEY")

	cp, err := controlplane.New(filepath.Join(root, "pb_control", "pb_data"))
	if err != nil {
		log.Fatalf("control plane: %v", err)
	}
	if err := cp.App.Bootstrap(); err != nil {
		log.Fatalf("bootstrap control plane: %v", err)
	}
	defer cp.App.ResetBootstrapState()
	if err := cp.App.RunAllMigrations(); err != nil {
		log.Fatalf("control-plane migrations: %v", err)
	}

	if err := ensureSuperuser(cp.App); err != nil {
		log.Fatalf("bootstrap superuser: %v", err)
	}

	pkgStore := store.New(root)
	programs := progcache.New()

	mgr := orgmanager.New(orgmanager.Config{
		Root:      root,
		Store:     pkgStore,
		Programs:  programs,
		LookupOrg: controlplane.OrgLookup(cp.App),
		HooksPool: 15,
		MaxIdle:   30 * time.Minute,
	})
	defer mgr.Shutdown()

	prov := controlplane.NewProvisioner(cp.App, root, pkgStore, mgr.Evict)
	prov.RegisterRoutes()

	controlMux, err := apis.BuildServeMux(cp.App, apis.ServeConfig{})
	if err != nil {
		log.Fatalf("control-plane mux: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("serve-multi listening on %s for *.%s (tls=%s)", addr, baseDomain, tlsMode)
	err = server.Serve(ctx, addr, server.Params{
		BaseDomain:      baseDomain,
		ControlPlaneMux: controlMux,
		GetOrg: func(slug string) (http.Handler, error) {
			inst, err := mgr.Get(slug)
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
		log.Fatalf("serve: %v", err)
	}
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
