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

	log.Printf("serve-multi listening on %s for *.%s", addr, baseDomain)
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
