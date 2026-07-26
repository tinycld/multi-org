// Command serve-org runs ONE organization's PocketBase app in its own OS
// process, serving on a unix socket handed down by the multi-org router.
//
// This is the tenant side of per-process isolation. The process is confined by
// its parent (per-tenant uid, mount and PID namespaces, cgroups on Linux) so
// that a hostile tenant's JS cannot reach another org's data even though it
// holds a full $app/DB surface — a boundary in-process allowlisting provably
// could not provide.
//
// It links no feature Go. CardDAV and WebDAV come from core and are driven
// entirely by the declarative source lists the host materialized.
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

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/plugins/jsvm"

	"tinycld.org/core/caldav"
	"tinycld.org/core/carddav"
	"tinycld.org/core/quota"
	"tinycld.org/core/webdav"
	"tinycld.org/multi-org/internal/davconfig"
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

	if err := run(*orgDir, *socketPath, *slug, *hooksPool, *davConfig, *caldavConfig, *webdavConfig, *quotaConfig, *confinePkg, *drain, ready); err != nil {
		reportNotReady(ready, err)
		log.Fatalf("serve-org: %v", err)
	}
}

func run(orgDir, socketPath, slug string, hooksPool int, davConfigPath, caldavConfigPath, webdavConfigPath, quotaConfigPath, confinePkg string,
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

	app := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir: filepath.Join(orgDir, "pb_data"),
		// The start banner is derived from Server.Addr, which is meaningless
		// for a unix socket.
		HideStartBanner: true,
	})

	// Register (not MustRegister): a hook that throws at load must fail this
	// one org, reported cleanly to the parent, rather than panicking.
	//
	// NOTE: no OnInit/OnLoaderInit here, so a tenant's VMs carry no `$`
	// bindings and package TS cannot register a Go→TS hook handler. Wiring
	// them means importing tinycld.org/core/coreserver, which drags Sentry,
	// webpush, postmark and go-message into the tenant binary — the one
	// process the isolation model most wants kept small. Closing this needs a
	// narrow bindings-only package that coreserver and serve-org can share;
	// see HANDOFF.
	if err := jsvm.Register(app, jsvm.Config{
		HooksDir:      filepath.Join(orgDir, "pb_hooks"),
		MigrationsDir: filepath.Join(orgDir, "pb_migrations"),
		// A hooks watcher calls app.Restart(), which execve's the process out
		// from under the parent's supervision. The router re-materializes and
		// evicts on deploy instead.
		HooksWatch:    false,
		HooksPoolSize: hooksPool,
		// Defence in depth. OS confinement is the boundary; this still removes
		// exec/env/outbound/file-read bindings for free.
		Sandboxed: true,
	}); err != nil {
		return fmt.Errorf("jsvm register: %w", err)
	}

	// Storage ceilings. The org limit comes from the router's runtime config,
	// NOT this org's settings — its superusers must not be able to raise the
	// plan they were sold. core/quota binds record hooks, so every write path
	// in the tenant is covered even though no feature Go is linked here.
	if err := quota.Register(app, davconfig.DecodeQuota(quotaCfg.Sources),
		quota.FixedLimits(quotaCfg.StorageLimitBytes)); err != nil {
		return fmt.Errorf("quota register: %w", err)
	}

	if err := app.Bootstrap(); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	defer app.ResetBootstrapState()

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", socketPath, err)
	}
	// Only the router connects to this socket; the tenant uid owns it.
	if err := os.Chmod(socketPath, 0o600); err != nil {
		return fmt.Errorf("chmod socket: %w", err)
	}

	// Captured from the ServeEvent so the signal handler can drain the real
	// server rather than just closing the listener under it.
	var srv *http.Server

	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		// apis.Serve uses this listener verbatim instead of dialing TCP.
		e.Listener = listener
		srv = e.Server
		// The installer builds a URL from Server.Addr and would try to open a
		// browser; meaningless for a headless tenant on a socket.
		e.InstallerFunc = nil

		if len(davSources) > 0 {
			// No HostBindings: a tenant's hook points would need coreserver,
			// which drags Sentry/webpush/postmark into the tenant binary. See
			// the jsvm.Register note above and HANDOFF.
			h, _, err := webdav.HandlerFor(e.App, davSources, webdav.HostBindings{})
			if err != nil {
				return fmt.Errorf("webdav handler: %w", err)
			}
			if h != nil {
				serve := func(re *core.RequestEvent) error {
					h.ServeHTTP(re.Response, re.Request)
					return nil
				}
				for _, src := range davSources {
					e.Router.Any(src.Prefix, serve)
					e.Router.Any(src.Prefix+"/{path...}", serve)
				}
				e.Router.Any("/.well-known/webdav", serve)
			}
		}

		if len(sources) > 0 {
			if h := carddav.HandlerFor(e.App, sources); h != nil {
				serve := func(re *core.RequestEvent) error {
					h.ServeHTTP(re.Response, re.Request)
					return nil
				}
				e.Router.Any("/carddav", serve)
				e.Router.Any("/carddav/{path...}", serve)
				e.Router.Any("/.well-known/carddav", serve)
			}
		}

		if len(calSources) > 0 {
			// Same as WebDAV above: no HostBindings, so `caldavHook` handlers a
			// package ships do not run in a tenant. Every access check still
			// does — core evaluates the collections' own PocketBase rules,
			// which travel in the schema rather than as Go closures.
			if h, _ := caldav.HandlerFor(e.App, calSources, caldav.HostBindings{}); h != nil {
				serve := func(re *core.RequestEvent) error {
					h.ServeHTTP(re.Response, re.Request)
					return nil
				}
				// Prefix-driven, not hardcoded: a package chooses where its
				// calendar tree mounts via the manifest's caldav.prefix.
				for _, src := range calSources {
					e.Router.Any(src.Prefix, serve)
					e.Router.Any(src.Prefix+"/{path...}", serve)
				}
				e.Router.Any("/.well-known/caldav", serve)
			}
		}

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
