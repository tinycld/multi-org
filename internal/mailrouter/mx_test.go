package mailrouter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-smtp"

	"tinycld.org/multi-org/internal/orgmanager"
)

// tenantRecorder is a fake tenant MX endpoint: a real go-smtp server on a
// unix socket that records what it was asked to deliver — standing in for the
// tenant's inbound session.
type tenantRecorder struct {
	mu     sync.Mutex
	from   string
	rcpts  []string
	bodies []string
}

func (rec *tenantRecorder) snapshot() (string, []string, []string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.from, append([]string(nil), rec.rcpts...), append([]string(nil), rec.bodies...)
}

type recSession struct{ rec *tenantRecorder }

func (s *recSession) Reset()        {}
func (s *recSession) Logout() error { return nil }
func (s *recSession) Mail(from string, _ *smtp.MailOptions) error {
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	s.rec.from = from
	return nil
}
func (s *recSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	s.rec.rcpts = append(s.rec.rcpts, to)
	return nil
}
func (s *recSession) Data(r io.Reader) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	s.rec.bodies = append(s.rec.bodies, string(body))
	return nil
}

type recBackend struct{ rec *tenantRecorder }

func (b *recBackend) NewSession(_ *smtp.Conn) (smtp.Session, error) {
	return &recSession{rec: b.rec}, nil
}

func serveTenantMX(t *testing.T, path string) *tenantRecorder {
	t.Helper()
	rec := &tenantRecorder{}
	srv := smtp.NewServer(&recBackend{rec: rec})
	srv.Domain = "tenant.internal"
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = ln.Close(); _ = srv.Close() })
	return rec
}

func mxClient(t *testing.T, addr net.Addr) *smtp.Client {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr.String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c := smtp.NewClient(conn)
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Hello("mta.sender.test"); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestMX_FansOutOneTransactionToEveryTargetOrg(t *testing.T) {
	dir := shortSockDir(t)
	acmeSock := filepath.Join(dir, "a.sock")
	rivalSock := filepath.Join(dir, "b.sock")
	acmeRec := serveTenantMX(t, acmeSock)
	rivalRec := serveTenantMX(t, rivalSock)

	tenants := map[string]*fakeTenant{
		"acme":  {socks: orgmanager.MailSockets{MX: acmeSock}},
		"rival": {socks: orgmanager.MailSockets{MX: rivalSock}},
	}
	registry := map[string]string{"acme-corp.com": "acme", "rival.io": "rival"}

	r := startRouter(t, Config{
		MXAddr: "127.0.0.1:0",
		GetOrg: func(_ context.Context, slug string) (Tenant, error) {
			ten, ok := tenants[slug]
			if !ok {
				return nil, fmt.Errorf("unknown org")
			}
			return ten, nil
		},
		LookupDomain: func(domain string) (string, bool) {
			slug, ok := registry[domain]
			return slug, ok
		},
	})

	c := mxClient(t, r.MXListenerAddr())
	if err := c.Mail("sender@elsewhere.example", nil); err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"alice@acme-corp.com", "ops@acme-corp.com", "bob@rival.io"} {
		if err := c.Rcpt(to, nil); err != nil {
			t.Fatalf("RCPT %s: %v", to, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		t.Fatal(err)
	}
	const body = "Subject: hi\r\nMessage-ID: <m1@elsewhere.example>\r\n\r\nhello both orgs\r\n"
	if _, err := io.WriteString(w, body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("DATA must succeed once every org accepted: %v", err)
	}

	from, rcpts, bodies := acmeRec.snapshot()
	if from != "sender@elsewhere.example" {
		t.Fatalf("acme saw MAIL FROM %q", from)
	}
	if len(rcpts) != 2 || rcpts[0] != "alice@acme-corp.com" || rcpts[1] != "ops@acme-corp.com" {
		t.Fatalf("acme rcpts = %v — must receive only its own recipients, in order", rcpts)
	}
	if len(bodies) != 1 || !strings.Contains(bodies[0], "hello both orgs") {
		t.Fatalf("acme bodies = %q", bodies)
	}

	_, rcpts, bodies = rivalRec.snapshot()
	if len(rcpts) != 1 || rcpts[0] != "bob@rival.io" {
		t.Fatalf("rival rcpts = %v", rcpts)
	}
	if len(bodies) != 1 {
		t.Fatalf("rival got %d bodies, want 1", len(bodies))
	}
}

func TestMX_UnknownDomainIsRefusedAtRcptTime(t *testing.T) {
	r := startRouter(t, Config{
		MXAddr:       "127.0.0.1:0",
		GetOrg:       func(_ context.Context, _ string) (Tenant, error) { return nil, fmt.Errorf("no org") },
		LookupDomain: func(string) (string, bool) { return "", false },
	})

	c := mxClient(t, r.MXListenerAddr())
	if err := c.Mail("sender@elsewhere.example", nil); err != nil {
		t.Fatal(err)
	}
	err := c.Rcpt("who@nowhere.example", nil)
	if err == nil {
		t.Fatal("RCPT for an unhosted domain must be refused — this deployment is not its MX")
	}
	var sErr *smtp.SMTPError
	if !smtpErrAs(err, &sErr) || sErr.Code != 550 {
		t.Fatalf("want permanent 550 at RCPT time, got %v", err)
	}
}

// One org failing must fail the whole message TRANSIENTLY (451): the sender
// retries, and orgs that already accepted dedup the retry by Message-ID —
// a permanent refusal would drop the failed org's copy forever.
func TestMX_TenantFailureYieldsTransient451(t *testing.T) {
	dir := shortSockDir(t)
	okSock := filepath.Join(dir, "ok.sock")
	serveTenantMX(t, okSock)

	tenants := map[string]*fakeTenant{
		"good": {socks: orgmanager.MailSockets{MX: okSock}},
		"dead": {socks: orgmanager.MailSockets{MX: filepath.Join(dir, "gone.sock")}},
	}
	registry := map[string]string{"good.test": "good", "dead.test": "dead"}

	r := startRouter(t, Config{
		MXAddr: "127.0.0.1:0",
		GetOrg: func(_ context.Context, slug string) (Tenant, error) {
			return tenants[slug], nil
		},
		LookupDomain: func(domain string) (string, bool) {
			slug, ok := registry[domain]
			return slug, ok
		},
	})

	c := mxClient(t, r.MXListenerAddr())
	if err := c.Mail("sender@elsewhere.example", nil); err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"a@good.test", "b@dead.test"} {
		if err := c.Rcpt(to, nil); err != nil {
			t.Fatalf("RCPT %s: %v", to, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(w, "Subject: x\r\n\r\nbody\r\n")
	err = w.Close()
	if err == nil {
		t.Fatal("DATA must fail when a target org cannot take delivery")
	}
	var sErr *smtp.SMTPError
	if !smtpErrAs(err, &sErr) || sErr.Code != 451 {
		t.Fatalf("want transient 451, got %v", err)
	}
}

func smtpErrAs(err error, target **smtp.SMTPError) bool {
	return errors.As(err, target)
}
