package mailrouter

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"tinycld.org/multi-org/internal/orgmanager"
)

const testBaseDomain = "example.test"

// testTLSConfig builds a self-signed cert; clients dial with
// InsecureSkipVerify plus an explicit ServerName, which is all SNI routing
// needs.
func testTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "*." + testBaseDomain},
		DNSNames:     []string{"*." + testBaseDomain, testBaseDomain},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{
		Certificate: [][]byte{der},
		PrivateKey:  key,
	}}}
}

// shortSockDir returns a dir shallow enough for unix socket paths —
// t.TempDir() on darwin overruns sun_path.
func shortSockDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mrt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// fakeTenant satisfies Tenant with fixed socket paths and a live conn count.
type fakeTenant struct {
	socks   orgmanager.MailSockets
	tracked atomic.Int64
}

func (f *fakeTenant) MailSockets() orgmanager.MailSockets { return f.socks }
func (f *fakeTenant) TrackConn() func() {
	f.tracked.Add(1)
	var once sync.Once
	return func() { once.Do(func() { f.tracked.Add(-1) }) }
}

// serveGreetingEcho serves a unix socket: greet, then echo everything back.
func serveGreetingEcho(t *testing.T, path, greeting string) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.WriteString(c, greeting)
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
}

func startRouter(t *testing.T, cfg Config) *Router {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.BaseDomain == "" {
		cfg.BaseDomain = testBaseDomain
	}
	if cfg.TLS == nil {
		cfg.TLS = testTLSConfig(t)
	}
	r := New(cfg)
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Shutdown)
	return r
}

func dialSNI(t *testing.T, addr net.Addr, serverName string) (*tls.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := tls.Dial("tcp", addr.String(), &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         serverName,
	})
	if err != nil {
		t.Fatalf("tls dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	return conn, bufio.NewReader(conn)
}

func TestTLSDemux_RoutesBySNIToTheOrgsServiceSocket(t *testing.T) {
	dir := shortSockDir(t)
	imapSock := filepath.Join(dir, "imap.sock")
	smtpSock := filepath.Join(dir, "smtp.sock")
	serveGreetingEcho(t, imapSock, "* OK org-acme imap\r\n")
	serveGreetingEcho(t, smtpSock, "220 org-acme smtp\r\n")

	acme := &fakeTenant{socks: orgmanager.MailSockets{IMAP: imapSock, SMTP: smtpSock}}
	var gotSlugs []string
	var mu sync.Mutex
	r := startRouter(t, Config{
		IMAPSAddr: "127.0.0.1:0",
		SMTPSAddr: "127.0.0.1:0",
		GetOrg: func(_ context.Context, slug string) (Tenant, error) {
			mu.Lock()
			gotSlugs = append(gotSlugs, slug)
			mu.Unlock()
			if slug != "acme" {
				return nil, fmt.Errorf("unknown org")
			}
			return acme, nil
		},
	})

	// IMAPS reaches the org's IMAP socket…
	conn, rd := dialSNI(t, r.IMAPSListenerAddr(), "acme."+testBaseDomain)
	if line, err := rd.ReadString('\n'); err != nil || line != "* OK org-acme imap\r\n" {
		t.Fatalf("IMAPS greeting = %q, err=%v", line, err)
	}
	// …and the splice is bidirectional (the fake echoes).
	if _, err := conn.Write([]byte("a1 NOOP\r\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := rd.ReadString('\n'); err != nil || line != "a1 NOOP\r\n" {
		t.Fatalf("echo through splice = %q, err=%v", line, err)
	}

	// SMTPS reaches the SMTP socket of the same org.
	_, srd := dialSNI(t, r.SMTPSListenerAddr(), "acme."+testBaseDomain)
	if line, err := srd.ReadString('\n'); err != nil || line != "220 org-acme smtp\r\n" {
		t.Fatalf("SMTPS greeting = %q, err=%v", line, err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, s := range gotSlugs {
		if s != "acme" {
			t.Fatalf("routed to slug %q, want acme (all: %v)", s, gotSlugs)
		}
	}
	if len(gotSlugs) != 2 {
		t.Fatalf("GetOrg called %d times, want 2", len(gotSlugs))
	}
}

// Unknown org, reserved label, and an org without mail must all be refused
// with IDENTICAL bytes — a distinct response per cause would let a prober map
// which slugs exist (the HTTP front router's shared-404 policy).
func TestTLSDemux_RefusalsAreUniform(t *testing.T) {
	mailless := &fakeTenant{} // org exists, no mail sockets
	r := startRouter(t, Config{
		IMAPSAddr: "127.0.0.1:0",
		GetOrg: func(_ context.Context, slug string) (Tenant, error) {
			if slug == "mailless" {
				return mailless, nil
			}
			return nil, fmt.Errorf("no such org")
		},
	})

	responses := map[string]string{}
	for _, sni := range []string{
		"ghost." + testBaseDomain,    // unknown org
		"mailless." + testBaseDomain, // org without the mail package
		"admin." + testBaseDomain,    // reserved label
		testBaseDomain,               // apex — no org at all
	} {
		_, rd := dialSNI(t, r.IMAPSListenerAddr(), sni)
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("read refusal for %q: %v", sni, err)
		}
		responses[sni] = line
	}
	var first string
	for sni, line := range responses {
		if first == "" {
			first = line
		}
		if line != first {
			t.Fatalf("refusals differ (%q): %#v", sni, responses)
		}
	}
	if first != "* BYE service unavailable\r\n" {
		t.Fatalf("IMAP refusal = %q", first)
	}
}

// A relayed connection must keep the org resident (TrackConn held) exactly as
// long as it is open.
func TestTLSDemux_TracksConnLifetime(t *testing.T) {
	dir := shortSockDir(t)
	imapSock := filepath.Join(dir, "imap.sock")
	serveGreetingEcho(t, imapSock, "* OK hello\r\n")
	acme := &fakeTenant{socks: orgmanager.MailSockets{IMAP: imapSock}}
	r := startRouter(t, Config{
		IMAPSAddr: "127.0.0.1:0",
		GetOrg:    func(_ context.Context, _ string) (Tenant, error) { return acme, nil },
	})

	conn, rd := dialSNI(t, r.IMAPSListenerAddr(), "acme."+testBaseDomain)
	if _, err := rd.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if got := acme.tracked.Load(); got != 1 {
		t.Fatalf("open connection tracked = %d, want 1", got)
	}

	_ = conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for acme.tracked.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("connection still tracked after close: %d", acme.tracked.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// flakyListener fails its first N Accepts with a transient error, then
// reports closed so the loop under test terminates.
type flakyListener struct {
	mu       sync.Mutex
	calls    int
	failures int
}

func (l *flakyListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	l.calls++
	n := l.calls
	l.mu.Unlock()
	if n <= l.failures {
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: syscall.EMFILE}
	}
	return nil, net.ErrClosed
}

func (l *flakyListener) Close() error   { return nil }
func (l *flakyListener) Addr() net.Addr { return &net.TCPAddr{} }

func (l *flakyListener) acceptCalls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// A transient Accept error — an fd-exhaustion burst is the canonical case —
// must not end the accept loop. The listener stays bound either way, so
// giving up turns :993/:465 into a black hole that accepts TCP and then
// hangs forever: mail dies with no crash, no restart, and no alert.
func TestAcceptLoop_SurvivesTransientAcceptErrors(t *testing.T) {
	ln := &flakyListener{failures: 3}
	r := New(Config{
		BaseDomain: testBaseDomain,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	done := make(chan struct{})
	go func() {
		r.acceptLoop(ln, svcIMAP)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("accept loop did not terminate on net.ErrClosed")
	}
	if got := ln.acceptCalls(); got != ln.failures+1 {
		t.Fatalf("Accept called %d times, want %d — the loop must retry transient errors until the listener closes",
			got, ln.failures+1)
	}
}

// Shutdown during the retry backoff must end the loop promptly rather than
// sleeping out the remaining delay.
func TestAcceptLoop_ShutdownInterruptsRetryBackoff(t *testing.T) {
	// Enough consecutive failures to drive the backoff to its cap, so a loop
	// that ignores shutdown measurably overstays.
	ln := &flakyListener{failures: 1 << 20}
	r := New(Config{
		BaseDomain: testBaseDomain,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	done := make(chan struct{})
	go func() {
		r.acceptLoop(ln, svcIMAP)
		close(done)
	}()

	// Let it fail at least once so it is inside the retry path.
	deadline := time.Now().Add(5 * time.Second)
	for ln.acceptCalls() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	r.Shutdown()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("accept loop kept retrying after Shutdown")
	}
}
