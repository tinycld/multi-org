package mailrouter

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/emersion/go-smtp"
)

// The :25 frontend is store-and-forward rather than a byte splice, because a
// single SMTP transaction can name recipients in SEVERAL orgs (one MTA, one
// message, RCPTs on two customers' domains) and a spliced connection can only
// reach one tenant. The frontend accepts the transaction itself — offering
// STARTTLS with the router's cert — resolves each RCPT's domain through the
// registry at RCPT time (so an unknown domain is refused per-recipient with a
// permanent 550, before any DATA is transferred), buffers the message
// (bounded by MaxMessageBytes), and replays it over each target org's
// mx.sock, where the tenant's real inbound session runs. The client's 250
// arrives only after EVERY target org accepted; any failure is a transient
// 451, and the retry's duplicates toward orgs that already accepted are
// deduplicated by Message-ID in mail's inbound path
// (TestHandleInbound_IdempotentRetry pins that).

// mxMaxMessageBytes mirrors the tenant inbound server's cap: accepting more
// here than a tenant will take just converts the refusal into a relay error.
const mxMaxMessageBytes = 25 << 20

// mxMaxRecipients bounds one transaction's RCPT list. 100 is RFC 5321's
// required minimum, and a legitimate MTA splits beyond it; unbounded, every
// extra recipient is another buffered replay this frontend owes.
const mxMaxRecipients = 100

// mxMaxTargetOrgs bounds how many DISTINCT orgs one transaction may deliver
// to. Each new org can be a cold start (relayTimeout each), so without a cap
// a single crafted message with RCPTs across every enumerable hosted domain
// cold-starts the whole fleet from one DATA. Real cross-org mail names a
// couple of customers, not dozens; excess is refused 452 so a legitimate
// sender retries the rest in a new transaction.
const mxMaxTargetOrgs = 8

// relayTimeout bounds one org's relay, including a cold spawn (the manager's
// 45s spawnTimeout) plus the SMTP round-trips.
const relayTimeout = 90 * time.Second

type mxServer struct {
	srv *smtp.Server
	ln  net.Listener
}

func (m *mxServer) close() {
	_ = m.ln.Close()
	_ = m.srv.Close()
}

func startMX(r *Router) (*mxServer, error) {
	srv := smtp.NewServer(&mxBackend{r: r})
	srv.Domain = r.cfg.MXHostname
	srv.MaxMessageBytes = mxMaxMessageBytes
	srv.MaxRecipients = mxMaxRecipients
	srv.EnableSMTPUTF8 = true
	srv.ReadTimeout = 60 * time.Second
	srv.WriteTimeout = 60 * time.Second
	// STARTTLS with the wildcard cert. AUTH is never offered — this is
	// server-to-server traffic; submission is :465.
	srv.TLSConfig = r.cfg.TLS

	ln, err := net.Listen("tcp", r.cfg.MXAddr)
	if err != nil {
		return nil, fmt.Errorf("mailrouter: listen MX %s: %w", r.cfg.MXAddr, err)
	}
	go func() {
		if err := srv.Serve(ln); err != nil {
			select {
			case <-r.closed:
			default:
				r.cfg.Logger.Error("mailrouter MX server error", "error", err)
			}
		}
	}()
	return &mxServer{srv: srv, ln: ln}, nil
}

type mxBackend struct{ r *Router }

func (b *mxBackend) NewSession(_ *smtp.Conn) (smtp.Session, error) {
	return &mxSession{r: b.r, rcpts: map[string][]string{}}, nil
}

type mxSession struct {
	r    *Router
	from string
	opts *smtp.MailOptions

	// rcpts groups accepted recipients by target org slug, preserving order
	// within each org.
	rcpts map[string][]string
}

func (s *mxSession) Reset() {
	s.from = ""
	s.opts = nil
	s.rcpts = map[string][]string{}
}

func (s *mxSession) Logout() error { return nil }

func (s *mxSession) Mail(from string, opts *smtp.MailOptions) error {
	s.from = from
	s.opts = opts
	return nil
}

func (s *mxSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	at := strings.LastIndexByte(to, '@')
	if at <= 0 || at == len(to)-1 {
		return &smtp.SMTPError{Code: 501, EnhancedCode: smtp.EnhancedCode{5, 1, 3}, Message: "malformed recipient address"}
	}
	slug, ok := s.r.cfg.LookupDomain(to[at+1:])
	if !ok {
		// Permanent: this deployment is not the MX for that domain. Refusing
		// at RCPT time (not after DATA) is what lets the sender bounce
		// per-recipient instead of per-message.
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "recipient domain not hosted here"}
	}
	if _, admitted := s.rcpts[slug]; !admitted && len(s.rcpts) >= mxMaxTargetOrgs {
		// Transient and per-recipient: a legitimate sender retries the excess
		// in a fresh transaction; a fleet-wide cold-start probe does not get
		// its fan-out. The cap is on DISTINCT orgs (each a potential cold
		// start) — more recipients in an admitted org cost nothing extra.
		return &smtp.SMTPError{Code: 452, EnhancedCode: smtp.EnhancedCode{4, 5, 3}, Message: "too many recipient organizations in one transaction"}
	}
	s.rcpts[slug] = append(s.rcpts[slug], to)
	return nil
}

func (s *mxSession) Data(rd io.Reader) error {
	body, err := io.ReadAll(rd)
	if err != nil {
		return err
	}
	for slug, rcpts := range s.rcpts {
		if err := s.r.relayToOrg(slug, s.from, s.opts, rcpts, body); err != nil {
			s.r.cfg.Logger.Error("mailrouter MX relay failed",
				"slug", slug, "rcpts", len(rcpts), "error", err)
			// Transient for the WHOLE message: the sender retries, and orgs
			// that already accepted dedup the retry by Message-ID.
			return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 4, 1}, Message: "temporary delivery failure, try again later"}
		}
	}
	return nil
}

// relayToOrg replays one accepted message into an org's tenant over its
// mx.sock, as a plain SMTP client transaction — the tenant side is the same
// inbound session a single-org deployment runs on :25.
func (r *Router) relayToOrg(slug, from string, opts *smtp.MailOptions, rcpts []string, body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), relayTimeout)
	defer cancel()

	tenant, err := r.cfg.GetOrg(ctx, slug)
	if err != nil {
		return fmt.Errorf("bring up org: %w", err)
	}
	path := tenant.MailSockets().MX
	if path == "" {
		// A domain registered to an org whose package set has no mail is
		// registry/lockfile drift. Transient rather than permanent: the
		// operator may be mid-deploy adding the package, and a 550 would make
		// the sender bounce the mail irrecoverably.
		return fmt.Errorf("org %s has a registered mail domain but no mail listener", slug)
	}

	release := tenant.TrackConn()
	defer release()

	conn, err := net.Dial("unix", path)
	if err != nil {
		return fmt.Errorf("dial mx socket: %w", err)
	}
	c := smtp.NewClient(conn)
	defer func() { _ = c.Close() }()

	if err := c.Hello(r.cfg.MXHostname); err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	if err := c.Mail(from, opts); err != nil {
		return fmt.Errorf("mail from: %w", err)
	}
	for _, to := range rcpts {
		if err := c.Rcpt(to, nil); err != nil {
			return fmt.Errorf("rcpt %s: %w", to, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("finish data: %w", err)
	}
	return c.Quit()
}
