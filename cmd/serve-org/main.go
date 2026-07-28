// Command serve-org runs ONE organization's PocketBase app in its own OS
// process, serving on a unix socket handed down by the multi-org router.
//
// This is the tenant side of per-process isolation. The process is confined by
// its parent (per-tenant uid, mount and PID namespaces, cgroups on Linux) so
// that a hostile tenant's JS cannot reach another org's data even though it
// holds a full $app/DB surface — a boundary in-process allowlisting provably
// could not provide.
//
// The tenant is composed by coreserver.RegisterTenant — the same composition
// root as the single-org app, minus the host-only pieces (CLI flags, static
// serving, package install, demo, setup bootstrap). This file owns only what
// is genuinely tenant-transport: config loading, the unix socket, the
// readiness handshake, confinement, and shutdown. Do NOT register core
// behavior here; add it to coreserver's shared composition so both the app
// and every tenant get it (docs/FINDING-tenant-composition-gap.md records
// what the previous hand-rolled subset cost).
//
// FEATURE Go links here through the pinned menu (internal/tenantpkgs,
// docs/SCOPE-tenant-feature-go.md closed as option b): each feature's
// RegisterTenant runs when the org's resolved package set — materialized to
// .runtime/packages.json and passed via --packages-config — includes its
// slug. A feature's request-hook enforcement therefore runs in a tenant.
// Packages outside the menu are TS-hooks/rules-only.
//
// The architectural rule about ports still holds: a service that must BIND A
// PORT moves into core so the router can open it. A tenant serves on a unix
// socket the router hands down and the router owns every listening socket, so
// a feature cannot bring its own listener — which is why the features'
// RegisterTenant entries exclude mail's IMAP/SMTP listeners, and why CardDAV,
// CalDAV and WebDAV stay core libraries driven by the declarative source
// lists the host materialized (the features' own DAV mounts are host-only).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"

	"tinycld.org/core/caldav"
	"tinycld.org/core/carddav"
	"tinycld.org/core/coreserver"
	"tinycld.org/core/quota"
	"tinycld.org/core/webdav"
	"tinycld.org/multi-org/internal/davconfig"
	"tinycld.org/multi-org/internal/tenantpkgs"
)

func main() {
	var (
		orgDir       = flag.String("org-dir", "", "org directory containing pb_data, pb_hooks, pb_migrations")
		socketPath   = flag.String("socket", "", "unix socket to create and serve on")
		readyFD      = flag.Int("ready-fd", 0, "file descriptor to report readiness on")
		slug         = flag.String("slug", "", "org slug (identification and logging only)")
		hooksPool    = flag.Int("hooks-pool", 15, "jsvm hook runtime pool size")
		webdavConfig = flag.String("webdav-config", "", "path to the org's materialized webdav.json")
		caldavConfig = flag.String("caldav-config", "", "path to the org's materialized caldav.json")
		quotaConfig  = flag.String("quota-config", "", "path to the org's materialized quota.json")
		pkgsConfig   = flag.String("packages-config", "", "path to the org's materialized packages.json (resolved slugs)")
		appConfig    = flag.String("app-config", "", "path to the org's materialized app.json (public URL)")
		davConfig    = flag.String("carddav-config", "", "path to the CardDAV source JSON")
		drain        = flag.Duration("drain", 10*time.Second, "graceful shutdown budget")
		confinePkg   = flag.String("confine-packages", "", "remount this dir read-only in our mount namespace")
	)
	flag.Parse()

	if *orgDir == "" || *socketPath == "" {
		log.Fatal("serve-org: --org-dir and --socket are required")
	}

	// Readiness is reported on a pipe the parent holds the read end of. Report
	// failures through it too, so the parent gets a reason instead of inferring
	// one from an exit code.
	var ready *os.File
	if *readyFD > 0 {
		ready = os.NewFile(uintptr(*readyFD), "ready")
	}

	if err := run(*orgDir, *socketPath, *slug, *hooksPool, *davConfig, *caldavConfig, *webdavConfig, *quotaConfig, *pkgsConfig, *appConfig, *confinePkg, *drain, ready); err != nil {
		reportNotReady(ready, err)
		log.Fatalf("serve-org: %v", err)
	}
}

func run(orgDir, socketPath, slug string, hooksPool int, davConfigPath, caldavConfigPath, webdavConfigPath, quotaConfigPath, pkgsConfigPath, appConfigPath, confinePkg string,
	drain time.Duration, ready *os.File) error {

	// Confinement steps that must happen inside our own mount namespace. Go
	// cannot run code between fork and exec, so the parent creates the
	// namespace and we finish the job here.
	if confinePkg != "" {
		if err := confinePackages(confinePkg); err != nil {
			return fmt.Errorf("confine packages: %w", err)
		}
	}

	sources, err := loadCardDAVSources(davConfigPath)
	if err != nil {
		return fmt.Errorf("load carddav config: %w", err)
	}

	davSources, err := loadWebDAVSources(webdavConfigPath)
	if err != nil {
		return fmt.Errorf("load webdav config: %w", err)
	}

	calSources, err := loadCalDAVSources(caldavConfigPath)
	if err != nil {
		return fmt.Errorf("load caldav config: %w", err)
	}

	quotaCfg, err := loadQuotaConfig(quotaConfigPath)
	if err != nil {
		return fmt.Errorf("load quota config: %w", err)
	}

	pkgSlugs, err := loadPackageSlugs(pkgsConfigPath)
	if err != nil {
		return fmt.Errorf("load packages config: %w", err)
	}

	appURL, err := loadAppURL(appConfigPath)
	if err != nil {
		return fmt.Errorf("load app config: %w", err)
	}

	app := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir: filepath.Join(orgDir, "pb_data"),
		// The start banner is derived from Server.Addr, which is meaningless
		// for a unix socket.
		HideStartBanner: true,
	})

	// The full shared core composition: guards, invites, account lifecycle,
	// notify, realtime, audit, quota, sandboxed jsvm with the `$`-binding and
	// hook-point seams, and the DAV protocol servers from the materialized
	// source lists. The org storage ceiling comes from the router's runtime
	// config (FixedLimits), NOT this org's settings — its superusers must not
	// be able to raise the plan they were sold.
	if err := coreserver.RegisterTenant(app, coreserver.TenantOptions{
		HooksDir:      filepath.Join(orgDir, "pb_hooks"),
		MigrationsDir: filepath.Join(orgDir, "pb_migrations"),
		HooksPoolSize: hooksPool,
		// Feature Go for the org's resolved packages, from the pinned menu
		// this binary links (internal/tenantpkgs — SCOPE-tenant-feature-go.md
		// option b). A resolved slug with no Go on the menu is normal
		// (TS-only or third-party package) but logged, so a menu omission is
		// visible at boot instead of silently serving rules-only.
		RegisterExtras: func(app *pocketbase.PocketBase) {
			registered, unknown := tenantpkgs.Register(app, pkgSlugs)
			log.Printf("serve-org[%s]: feature Go registered=%v ts-only=%v", slug, registered, unknown)
		},
		QuotaSources:   davconfig.DecodeQuota(quotaCfg.Sources),
		QuotaLimits:    quota.FixedLimits(quotaCfg.StorageLimitBytes),
		WebDAVSources:  davSources,
		CalDAVSources:  calSources,
		CardDAVSources: sources,
	}); err != nil {
		return fmt.Errorf("register tenant: %w", err)
	}
	// Errors in tenant request handling report to Sentry once the org's
	// system_settings carry a DSN; until then the SDK is a no-op. Flush is
	// also called on the signal path below, since os.Exit skips defers.
	defer sentry.Flush(2 * time.Second)

	if err := app.Bootstrap(); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	defer app.ResetBootstrapState()

	// Adopt the org's public URL as Meta.AppURL — the value PB interpolates
	// into {APP_URL} for verification / password-reset / email-change links.
	// Persisted rather than patched in memory so a settings reload (e.g. a
	// tenant superuser saving unrelated settings) cannot revert it to PB's
	// default http://localhost:8090.
	if appURL != "" && app.Settings().Meta.AppURL != appURL {
		app.Settings().Meta.AppURL = appURL
		if err := app.Save(app.Settings()); err != nil {
			return fmt.Errorf("set app url: %w", err)
		}
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", socketPath, err)
	}
	// The ROUTER owns the socket file's lifecycle, not us. On Evict-then-Get a
	// replacement binds this same deterministic path while we are still
	// draining, and Go's default unlink-on-close would delete the
	// REPLACEMENT's socket when our listener finally closes — permanently
	// 502ing the org. The router clears stale files before each bind and
	// guards its own teardown removal by inode.
	if ul, ok := listener.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	// Only the router connects to this socket; the tenant uid owns it.
	if err := os.Chmod(socketPath, 0o600); err != nil {
		return fmt.Errorf("chmod socket: %w", err)
	}

	// Captured from the ServeEvent so the signal handler can drain the real
	// server rather than just closing the listener under it.
	var srv *http.Server

	// DAV routes are mounted by RegisterTenant (webdav/caldav/carddav
	// Register bind their own OnServe handlers, same as the single-org app);
	// this handler owns only the tenant transport concerns.
	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		// apis.Serve uses this listener verbatim instead of dialing TCP.
		e.Listener = listener
		srv = e.Server
		// The installer builds a URL from Server.Addr and would try to open a
		// browser; meaningless for a headless tenant on a socket.
		e.InstallerFunc = nil

		if err := e.Next(); err != nil {
			return err
		}

		// Routes are bound and the server is about to accept: tell the parent.
		reportReady(ready)
		return nil
	})

	// Our own signal handling. apis.Serve's graceful shutdown is a hard-coded
	// one second and is wired through pb.Execute(), which we deliberately do
	// not call — Execute would also re-bootstrap and install its own signal
	// handling on top of the parent's supervision.
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		<-sigs
		// Stop accepting and let in-flight requests finish, bounded by the
		// parent's drain budget. Exits as soon as the last one completes; the
		// parent SIGKILLs us if we overrun.
		ctx, cancel := context.WithTimeout(context.Background(), drain)
		defer cancel()
		if srv != nil {
			_ = srv.Shutdown(ctx)
		} else {
			_ = listener.Close()
		}
		if err := app.ResetBootstrapState(); err != nil {
			log.Printf("serve-org: reset bootstrap state: %v", err)
		}
		// os.Exit skips deferred functions, so flush Sentry here too.
		sentry.Flush(2 * time.Second)
		os.Exit(0)
	}()

	err = apis.Serve(app, apis.ServeConfig{ShowStartBanner: false})
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// quotaConfig is the shape of .runtime/quota.json, written by the router.
type quotaConfig struct {
	StorageLimitBytes int64                   `json:"storageLimitBytes"`
	Sources           []davconfig.QuotaSource `json:"sources"`
}

func loadQuotaConfig(path string) (quotaConfig, error) {
	if path == "" {
		return quotaConfig{}, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return quotaConfig{}, nil
		}
		return quotaConfig{}, err
	}
	var cfg quotaConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		return quotaConfig{}, err
	}
	return cfg, nil
}

// loadPackageSlugs reads .runtime/packages.json (written by the router from
// the org's resolved lockfile). An empty path or a missing file means the
// router manages no package set here — register no feature Go, same shape as
// the DAV loaders below.
func loadPackageSlugs(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cfg struct {
		Slugs []string `json:"slugs"`
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, err
	}
	return cfg.Slugs, nil
}

// loadAppURL reads .runtime/app.json (written by the router from
// MT_BASE_DOMAIN + slug). An empty path or a missing file means the host
// manages no public URL here — leave the tenant's stored settings alone.
func loadAppURL(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var cfg struct {
		AppURL string `json:"appURL"`
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		return "", err
	}
	return cfg.AppURL, nil
}

func loadWebDAVSources(path string) ([]webdav.Source, error) {
	if path == "" {
		return nil, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var wire []davconfig.WebDAVSource
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, err
	}
	return davconfig.DecodeWebDAV(wire), nil
}

func loadCalDAVSources(path string) ([]caldav.Source, error) {
	if path == "" {
		return nil, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var wire []davconfig.CalDAVSource
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, err
	}
	return davconfig.DecodeCalDAV(wire), nil
}

func loadCardDAVSources(path string) ([]carddav.Source, error) {
	if path == "" {
		return nil, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var wire []davconfig.Source
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, err
	}
	return davconfig.Decode(wire), nil
}

func reportReady(f *os.File) {
	if f == nil {
		return
	}
	body, _ := json.Marshal(map[string]any{"ok": true, "pid": os.Getpid()})
	_, _ = f.Write(append(body, '\n'))
	_ = f.Close()
}

func reportNotReady(f *os.File, cause error) {
	if f == nil {
		return
	}
	body, _ := json.Marshal(map[string]any{"ok": false, "pid": os.Getpid(), "error": cause.Error()})
	_, _ = f.Write(append(body, '\n'))
	_ = f.Close()
}
