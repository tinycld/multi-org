package orgmanager

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// OrgInstance is one org's running tenant process plus the reverse proxy that
// fronts it. Instances are spawned lazily by the manager and torn down when
// evicted, when idle, or when the child dies on its own.
//
// The host holds no PocketBase app object for a tenant — that is the whole
// point of the isolation model. Everything reaches the tenant over its socket.
type OrgInstance struct {
	slug     string
	sockPath string
	// sockIno is the inode of the socket file the child bound, recorded once
	// the child reports ready. socketPath(slug) is deterministic, so an
	// evicted instance's teardown can race a replacement that has re-bound
	// the same path; the inode is how teardown tells its own socket file from
	// the replacement's. 0 = never recorded (fail safe: don't remove).
	sockIno uint64

	proc    Process
	proxy   *httputil.ReverseProxy
	handler http.Handler // proxy wrapped in the in-flight tracking middleware

	lastUsed atomic.Int64 // unix nanos; seeded at load, updated per proxied request

	// inflight counts requests currently being proxied so shutdown can drain
	// them before signalling the child.
	inflight sync.WaitGroup

	// closeOnce guards the drain→TERM→KILL→reap sequence. Four callers can race
	// into it for one instance: Evict from the provisioner, Evict from the idle
	// sweeper, manager Shutdown, and the supervisor noticing an unexpected
	// exit. Without the Once that is a double Wait (which errors) and a double
	// channel close (which panics).
	closeOnce sync.Once

	closed chan struct{} // closed when shutdown starts; new requests bail with 503
	dead   chan struct{} // closed once Wait() has returned
	// torndown closes when shutdown has fully completed — including the
	// socket cleanup that runs AFTER dead. Since the child no longer unlinks
	// its own socket on listener close (see serve-org's SetUnlinkOnClose),
	// "process reaped" and "socket removed" are distinct moments; anything
	// asserting post-eviction disk state must wait on this, not dead.
	torndown chan struct{}
	exitErr  error // the child's exit status; set before dead is closed

	log *slog.Logger
}

func (i *OrgInstance) Mux() http.Handler { return i.handler }
func (i *OrgInstance) Slug() string      { return i.slug }

func (i *OrgInstance) touch(now int64) { i.lastUsed.Store(now) }

// newProxy builds the reverse proxy for a tenant's unix socket. Three of these
// settings are load-bearing and fail quietly if omitted; see the comments.
func newProxy(sockPath string, log *slog.Logger) *httputil.ReverseProxy {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sockPath)
		},
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     90 * time.Second,
		// Deliberately no ResponseHeaderTimeout: /api/realtime is a long-lived
		// SSE stream that sends its first event only when something happens.
	}

	return &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			// Ignored by the unix dialer, but net/http requires a non-empty host.
			pr.Out.URL.Host = "tenant"
			pr.SetXForwarded()
			// Rewrite mode does not carry the inbound Host over (unlike the
			// legacy Director). PocketBase builds absolute URLs — verification
			// links, OAuth redirects — from the request host, so losing it
			// produces links pointing at "tenant".
			pr.Out.Host = pr.In.Host
		},
		// Immediate flush. PocketBase serves realtime subscriptions over SSE;
		// with default buffering those streams stall rather than error, which
		// is the kind of failure that reaches production unnoticed.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// A client hanging up on an SSE stream is routine, not an error,
			// and there is no live ResponseWriter left to write to.
			if errors.Is(err, context.Canceled) {
				return
			}
			log.Error("proxy to tenant failed", "path", r.URL.Path, "error", err)
			http.Error(w, "organization backend unavailable", http.StatusBadGateway)
		},
	}
}

// serveProxied tracks the request as in-flight so shutdown can drain it.
func (i *OrgInstance) serveProxied(w http.ResponseWriter, r *http.Request) {
	select {
	case <-i.closed:
		http.Error(w, "organization is restarting", http.StatusServiceUnavailable)
		return
	default:
	}

	// Benign race: closed can be closed between the select and the Add. That
	// request then proceeds against a child that is still alive (shutdown is in
	// its drain wait, TERM has not been sent), and at worst is cut off
	// mid-response — the same outcome as any drain-timeout expiry. Closing it
	// would need a mutex on the per-request hot path.
	i.inflight.Add(1)
	defer i.inflight.Done()

	// Touch here rather than only in Get, so a long-lived SSE stream keeps its
	// instance alive instead of being swept out from under itself.
	i.touch(time.Now().UnixNano())

	i.proxy.ServeHTTP(w, r)
}

// shutdown drains in-flight requests, then stops the child politely before
// resorting to force. Safe to call concurrently; only the first call runs.
func (i *OrgInstance) shutdown(drain, kill time.Duration) {
	i.closeOnce.Do(func() {
		defer close(i.torndown)
		close(i.closed)

		drained := make(chan struct{})
		go func() {
			i.inflight.Wait()
			close(drained)
		}()

		// An org with any open browser tab holds an SSE stream, so inflight
		// never reaches zero and this always waits the full drain window. That
		// is acceptable only because Evict hands shutdown to a goroutine and
		// returns immediately — nothing user-facing blocks on it. If Evict is
		// ever made synchronous again, every Deploy pays this latency.
		select {
		case <-drained:
		case <-time.After(drain):
			i.log.Warn("drain timed out with requests still in flight", "slug", i.slug)
		}

		if err := i.proc.Signal(syscall.SIGTERM); err != nil {
			i.log.Debug("SIGTERM failed (child likely already gone)", "slug", i.slug, "error", err)
		}

		select {
		case <-i.dead:
		case <-time.After(kill):
			i.log.Warn("child ignored SIGTERM; killing", "slug", i.slug)
			_ = i.proc.Kill()
			<-i.dead // the supervisor's Wait always returns once the child is killed
		}

		// Remove only a socket this instance still owns. An Evict-then-Get
		// respawns the org on the same deterministic path while this teardown
		// is still draining; removing unconditionally here would delete the
		// REPLACEMENT's socket — every dial then ENOENTs into 502 while the
		// supervisor still believes the new instance healthy, so nothing
		// respawns it until the idle sweep.
		if !i.ownsSocket() {
			i.log.Debug("skipping socket removal; path no longer this instance's",
				"slug", i.slug)
			return
		}
		if err := os.Remove(i.sockPath); err != nil && !os.IsNotExist(err) {
			i.log.Warn("could not remove tenant socket", "slug", i.slug, "error", err)
		}
	})
}

// ownsSocket reports whether the file currently at sockPath is the one this
// instance's child bound. A replacement child re-binding the path creates a
// new file with a new inode, which is what distinguishes "our stale socket"
// from "the replacement's live one". The stat-then-remove window is not
// atomic, but it shrinks the race from the whole drain+kill budget (15s) to
// microseconds — and a replacement cannot finish a spawn in that window.
func (i *OrgInstance) ownsSocket() bool {
	if i.sockIno == 0 {
		return false
	}
	st, err := os.Stat(i.sockPath)
	if err != nil {
		return false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	return ok && sys.Ino == i.sockIno
}
