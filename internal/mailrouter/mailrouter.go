// Package mailrouter owns the public mail ports for a multi-org deployment,
// the way internal/server owns :443. The tinycld app's mail listeners are
// host-only in the single-org composition because a tenant must never bind a
// port — this is the router-side counterpart that makes per-tenant mail work:
//
//   - IMAPS (:993) and SMTPS submission (:465) are demultiplexed by TLS SNI.
//     Clients configure <slug>.<baseDomain> as their mail server, the router
//     terminates TLS with the wildcard cert (a tenant must never hold that
//     private key), reads the SNI from the handshake, brings the org up via
//     the same manager Get the HTTP front router uses, and splices the
//     plaintext byte stream onto the org's imap.sock / smtp.sock — where the
//     tenant serves the real sessions in mailproto's external-TLS mode.
//
//   - The inbound MX (:25) has no reliable SNI (MTAs connect to the MX host,
//     often without TLS), so it routes on RCPT TO instead: a store-and-forward
//     SMTP frontend accepts each transaction, resolves every recipient's
//     domain through the control plane's org_mail_domains registry, and
//     replays the message over each target org's mx.sock (see mx.go).
//
// Failure responses are deliberately uniform: an unknown slug, a suspended
// org, an org without the mail package and a crash-looping org all get the
// same goodbye, so an unauthenticated prober cannot map which slugs exist —
// the same policy as the HTTP front router's 404.
package mailrouter

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"tinycld.org/multi-org/internal/frontrouter"
	"tinycld.org/multi-org/internal/orgmanager"
)

const (
	// handshakeTimeout bounds the TLS handshake: until it completes the peer
	// has proven nothing and holds a goroutine.
	handshakeTimeout = 30 * time.Second

	// orgBringupTimeout bounds bringing the target org up. It must exceed the
	// manager's spawnTimeout (45s): mail clients happily wait that long for a
	// greeting, and cutting the wait short would strand the spawn other
	// callers are collapsed onto.
	orgBringupTimeout = 60 * time.Second

	svcIMAP = "imap"
	svcSMTP = "smtp"
)

// Tenant is the slice of an org instance the mail router needs: where the
// org's mail sockets are, and a way to mark a long-lived connection open so
// the idle sweeper doesn't evict the org mid-session.
type Tenant interface {
	MailSockets() orgmanager.MailSockets
	TrackConn() func()
}

type Config struct {
	BaseDomain string

	// GetOrg brings the org up (or returns the resident instance) — the same
	// manager Get the HTTP front router uses, so a cold org spawns on its
	// first mail connection exactly like on its first HTTP request.
	GetOrg func(ctx context.Context, slug string) (Tenant, error)

	// LookupDomain resolves a mail domain to the owning org's slug, backed by
	// the control plane's org_mail_domains registry. Used only by the MX
	// frontend.
	LookupDomain func(domain string) (slug string, ok bool)

	// TLS is the wildcard-cert config. Required: it terminates IMAPS/SMTPS
	// (the SNI read is part of the handshake) and serves STARTTLS on the MX.
	TLS *tls.Config

	Logger *slog.Logger

	// Listener addresses. Empty string disables that listener.
	IMAPSAddr string
	SMTPSAddr string
	MXAddr    string

	// MXHostname is the identity the :25 greeting announces and the HELO name
	// used toward tenants. Empty defaults to BaseDomain.
	MXHostname string
}

type Router struct {
	cfg    Config
	closed chan struct{}

	imapsLn net.Listener
	smtpsLn net.Listener
	mx      *mxServer
}

func New(cfg Config) *Router {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MXHostname == "" {
		cfg.MXHostname = cfg.BaseDomain
	}
	return &Router{cfg: cfg, closed: make(chan struct{})}
}

// Start binds every configured listener and begins serving. A bind failure is
// returned (and already-bound listeners closed) — a router configured for
// mail ports it cannot open must fail loudly, not come up HTTP-only.
func (r *Router) Start() error {
	if r.cfg.TLS == nil && (r.cfg.IMAPSAddr != "" || r.cfg.SMTPSAddr != "" || r.cfg.MXAddr != "") {
		return fmt.Errorf("mailrouter: TLS config required (the router terminates mail TLS; tenants never hold the key)")
	}

	if r.cfg.IMAPSAddr != "" {
		ln, err := net.Listen("tcp", r.cfg.IMAPSAddr)
		if err != nil {
			return fmt.Errorf("mailrouter: listen IMAPS %s: %w", r.cfg.IMAPSAddr, err)
		}
		r.imapsLn = ln
		go r.acceptLoop(ln, svcIMAP)
	}

	if r.cfg.SMTPSAddr != "" {
		ln, err := net.Listen("tcp", r.cfg.SMTPSAddr)
		if err != nil {
			r.Shutdown()
			return fmt.Errorf("mailrouter: listen SMTPS %s: %w", r.cfg.SMTPSAddr, err)
		}
		r.smtpsLn = ln
		go r.acceptLoop(ln, svcSMTP)
	}

	if r.cfg.MXAddr != "" {
		mx, err := startMX(r)
		if err != nil {
			r.Shutdown()
			return err
		}
		r.mx = mx
	}

	return nil
}

// Shutdown closes the listeners. In-flight relayed connections are owned by
// the tenants they reach; the manager's teardown drains those.
func (r *Router) Shutdown() {
	select {
	case <-r.closed:
		return
	default:
		close(r.closed)
	}
	if r.imapsLn != nil {
		_ = r.imapsLn.Close()
	}
	if r.smtpsLn != nil {
		_ = r.smtpsLn.Close()
	}
	if r.mx != nil {
		r.mx.close()
	}
}

// Test/introspection accessors: the bound address of each listener, nil when
// disabled.
func (r *Router) IMAPSListenerAddr() net.Addr { return lnAddr(r.imapsLn) }
func (r *Router) SMTPSListenerAddr() net.Addr { return lnAddr(r.smtpsLn) }
func (r *Router) MXListenerAddr() net.Addr {
	if r.mx == nil {
		return nil
	}
	return r.mx.ln.Addr()
}

func lnAddr(ln net.Listener) net.Addr {
	if ln == nil {
		return nil
	}
	return ln.Addr()
}

func (r *Router) acceptLoop(ln net.Listener, svc string) {
	var delay time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-r.closed:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// A transient failure — an fd-exhaustion burst is the canonical
			// case — must not end this loop: the listener stays bound either
			// way, so giving up turns the port into a black hole that accepts
			// TCP and then hangs forever, killing mail with no crash and no
			// alert. Retry with capped backoff, staying responsive to
			// shutdown.
			delay *= 2
			switch {
			case delay == 0:
				delay = 5 * time.Millisecond
			case delay > time.Second:
				delay = time.Second
			}
			r.cfg.Logger.Warn("mailrouter accept failed; retrying",
				"service", svc, "delay", delay, "error", err)
			select {
			case <-r.closed:
				return
			case <-time.After(delay):
			}
			continue
		}
		delay = 0
		go r.handleTLSConn(conn, svc)
	}
}

// handleTLSConn terminates TLS, reads the org from the SNI, brings the org
// up, and splices the plaintext stream onto its mail socket.
func (r *Router) handleTLSConn(raw net.Conn, svc string) {
	tconn := tls.Server(raw, r.cfg.TLS)
	_ = tconn.SetDeadline(time.Now().Add(handshakeTimeout))
	if err := tconn.Handshake(); err != nil {
		_ = tconn.Close()
		return
	}
	_ = tconn.SetDeadline(time.Time{})

	sni := tconn.ConnectionState().ServerName
	slug := frontrouter.Subdomain(sni, r.cfg.BaseDomain)
	// Apex, www and admin are the front router's reserved labels; none is an
	// org. (An empty slug also covers a client that sent no SNI at all.)
	if slug == "" || slug == "www" || slug == "admin" {
		r.refuse(tconn, svc)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), orgBringupTimeout)
	defer cancel()
	tenant, err := r.cfg.GetOrg(ctx, slug)
	if err != nil {
		r.refuse(tconn, svc)
		return
	}

	socks := tenant.MailSockets()
	var path string
	switch svc {
	case svcIMAP:
		path = socks.IMAP
	case svcSMTP:
		path = socks.SMTP
	}
	if path == "" {
		// Org exists but runs no mail listeners (no mail package) — same
		// goodbye as an unknown org, deliberately.
		r.refuse(tconn, svc)
		return
	}

	back, err := net.Dial("unix", path)
	if err != nil {
		r.cfg.Logger.Error("mailrouter dial tenant socket failed",
			"service", svc, "slug", slug, "path", path, "error", err)
		r.refuse(tconn, svc)
		return
	}

	// TrackConn is granted by the splice itself, once it observes the tenant
	// accept the client's credentials — never at accept time (H5): a tracked
	// connection pins the org resident, so tracking unauthenticated clients
	// let a prober hold every org it could name and starve admission under
	// MaxResident. Until auth, the connection rides on lastUsed alone and the
	// org stays evictable.
	splice(tconn, back, newAuthDetector(svc), tenant.TrackConn)
}

// refuse writes one uniform protocol goodbye and closes. The SAME bytes for
// unknown, suspended, mail-less and unavailable orgs — a distinct message
// would tell an unauthenticated prober which slugs exist (the HTTP front
// router's shared-404 policy, in mail dialect).
func (r *Router) refuse(conn net.Conn, svc string) {
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	switch svc {
	case svcIMAP:
		_, _ = io.WriteString(conn, "* BYE service unavailable\r\n")
	case svcSMTP:
		_, _ = io.WriteString(conn, "421 service unavailable\r\n")
	}
	_ = conn.Close()
}

// spliceIdleTimeout is how long a spliced connection may go with no bytes in
// EITHER direction before the router tears it down. Without it, one abandoned
// TCP connection (a client that vanished without FIN, a wedged MTA) holds its
// TrackConn forever — and a tracked connection pins the org resident: the
// idle sweeper skips it and the LRU evictor cannot reclaim it. 30 minutes is
// RFC 3501's minimum autologout timer, so a compliant IMAP client in IDLE
// (which re-issues within 29 minutes) is never cut. Var for tests.
var spliceIdleTimeout = 30 * time.Minute

// preAuthTimeout is how long a spliced connection may exist WITHOUT observed
// authentication before the router closes it. Real clients authenticate
// within seconds of the greeting; a session still unauthenticated minutes in
// is a prober or a stalled scanner, and pre-auth sessions are exactly the
// ones H5 says must stay cheap. Var for tests.
var preAuthTimeout = 3 * time.Minute

// splice copies both directions until each side's read half is done,
// half-closing the opposite write half so in-flight bytes still flush —
// an IMAP LOGOUT's tagged response must reach the client even though the
// client's sending side finished first. A connection idle in both directions
// past spliceIdleTimeout — or unauthenticated past preAuthTimeout — is closed
// outright (see the two vars).
//
// The splice owns TrackConn (H5): det watches the byte stream, and only when
// it reports the tenant accepted the client's credentials is track() taken,
// released when the splice ends.
func splice(a, b net.Conn, det authDetector, track func() func()) {
	// Captured once so this connection's watchdog sees consistent deadlines
	// (the vars exist to be swapped in tests).
	idleTimeout, preAuth := spliceIdleTimeout, preAuthTimeout

	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	start := time.Now()

	var authed atomic.Bool
	var trackMu sync.Mutex
	var release func()
	markAuthed := func() {
		trackMu.Lock()
		defer trackMu.Unlock()
		if release == nil {
			release = track()
		}
		authed.Store(true)
	}

	type closeWriter interface{ CloseWrite() error }
	done := make(chan struct{}, 1)
	stop := make(chan struct{})
	cp := func(dst net.Conn, src *activityReader) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(closeWriter); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	// a is the client side, b the tenant side: the tenant's responses carry
	// the auth verdict, the client's bytes give the IMAP detector its tags.
	clientRd := &activityReader{conn: a, last: &lastActivity, observe: det.clientData}
	serverRd := &activityReader{conn: b, last: &lastActivity, observe: func(p []byte) {
		if !authed.Load() && det.serverData(p) {
			markAuthed()
		}
	}}

	// The watchdog closes both conns when the splice has been silent past the
	// deadline (or unauthenticated past its budget), which unblocks both
	// copies. Polling beats per-read SetReadDeadline juggling: two goroutines
	// share the two conns, and a deadline set by one direction would clip the
	// other's in-flight read.
	go func() {
		ticker := time.NewTicker(min(idleTimeout, preAuth) / 4)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				idle := time.Since(time.Unix(0, lastActivity.Load())) > idleTimeout
				overdue := !authed.Load() && time.Since(start) > preAuth
				if idle || overdue {
					_ = a.Close()
					_ = b.Close()
					return
				}
			}
		}
	}()

	go func() {
		cp(a, serverRd)
		done <- struct{}{}
	}()
	cp(b, clientRd)
	<-done
	close(stop)
	_ = a.Close()
	_ = b.Close()
	trackMu.Lock()
	if release != nil {
		release()
	}
	trackMu.Unlock()
}

// activityReader stamps the shared last-activity clock on every successful
// read — so traffic in either direction pushes the idle deadline out — and
// taps the bytes for the auth detector.
type activityReader struct {
	conn    net.Conn
	last    *atomic.Int64
	observe func([]byte)
}

func (r *activityReader) Read(p []byte) (int, error) {
	n, err := r.conn.Read(p)
	if n > 0 {
		r.last.Store(time.Now().UnixNano())
		if r.observe != nil {
			r.observe(p[:n])
		}
	}
	return n, err
}
