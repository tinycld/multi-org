package mailrouter

import (
	"bytes"
	"strings"
	"sync"
)

// Auth detection for the spliced IMAPS/SMTPS sessions (H5): TrackConn — the
// signal that pins an org resident against the idle sweeper and the LRU
// evictor — is granted only once the tenant's own mail server has accepted
// the client's credentials, observed from the plaintext stream the router is
// already splicing. An unauthenticated prober therefore cannot pin an org
// resident or starve admission under MaxResident.
//
// The detectors are line-based heuristics over the protocol, not full
// parsers. Both directions of a mail session are line-oriented until well
// past authentication, so pre-auth traffic is parseable; a false POSITIVE
// (e.g. crafted post-auth literal content that looks like a tagged OK) merely
// degrades to the pre-H5 behavior of tracking an unauthenticated connection,
// and a false negative leaves the session subject to the pre-auth deadline —
// both fail safe. The tenant's responses are trusted as the auth verdict: a
// hostile tenant colluding with a client can mint real credentials for its
// own org anyway, so forged verdicts grant nothing the tenant lacks.
type authDetector interface {
	// clientData observes bytes flowing client→tenant.
	clientData(p []byte)
	// serverData observes bytes flowing tenant→client and reports whether
	// authentication success has been observed so far (sticky once true).
	serverData(p []byte) bool
}

func newAuthDetector(svc string) authDetector {
	if svc == svcSMTP {
		return &smtpAuthDetector{}
	}
	return &imapAuthDetector{pending: map[string]bool{}}
}

// maxAuthLine caps the line-assembly buffers. Pre-auth protocol lines are
// short; anything longer (a literal, a flood) is discarded up to its newline
// rather than buffered.
const maxAuthLine = 1024

// maxPendingTags caps how many in-flight LOGIN/AUTHENTICATE tags an IMAP
// client may register. A real client has one; a flood of garbage commands
// must not grow router memory.
const maxPendingTags = 32

// lineAccum assembles a byte stream into lines, discarding overlong ones.
type lineAccum struct {
	buf  []byte
	skip bool
}

func (l *lineAccum) feed(p []byte, onLine func(line []byte)) {
	for len(p) > 0 {
		nl := bytes.IndexByte(p, '\n')
		if nl < 0 {
			if l.skip {
				return
			}
			if len(l.buf)+len(p) > maxAuthLine {
				l.buf, l.skip = l.buf[:0], true
				return
			}
			l.buf = append(l.buf, p...)
			return
		}
		chunk, rest := p[:nl], p[nl+1:]
		if l.skip {
			l.skip = false
		} else if len(l.buf)+len(chunk) <= maxAuthLine {
			onLine(bytes.TrimRight(append(l.buf, chunk...), "\r"))
		}
		l.buf, p = l.buf[:0], rest
	}
}

// token splits the first space-delimited token off a line.
func token(line []byte) (string, []byte) {
	line = bytes.TrimLeft(line, " ")
	if sp := bytes.IndexByte(line, ' '); sp >= 0 {
		return string(line[:sp]), line[sp+1:]
	}
	return string(line), nil
}

// imapAuthDetector watches the client direction for `<tag> LOGIN` /
// `<tag> AUTHENTICATE` commands and the server direction for that tag's OK
// completion (RFC 3501/9051). A tagged NO/BAD retires the tag — a refused
// login must not count.
type imapAuthDetector struct {
	mu      sync.Mutex
	client  lineAccum
	server  lineAccum
	pending map[string]bool
	authed  bool
}

func (d *imapAuthDetector) clientData(p []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.authed {
		return
	}
	d.client.feed(p, func(line []byte) {
		tag, rest := token(line)
		cmd, _ := token(rest)
		if tag == "" || tag == "*" || tag == "+" {
			return
		}
		if strings.EqualFold(cmd, "LOGIN") || strings.EqualFold(cmd, "AUTHENTICATE") {
			if len(d.pending) < maxPendingTags {
				d.pending[tag] = true
			}
		}
	})
}

func (d *imapAuthDetector) serverData(p []byte) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.authed {
		return true
	}
	d.server.feed(p, func(line []byte) {
		tag, rest := token(line)
		if !d.pending[tag] {
			return
		}
		status, _ := token(rest)
		switch {
		case strings.EqualFold(status, "OK"):
			d.authed = true
		case strings.EqualFold(status, "NO"), strings.EqualFold(status, "BAD"):
			delete(d.pending, tag)
		}
	})
	return d.authed
}

// smtpAuthDetector watches the server direction for a 235 reply — the code
// RFC 4954 reserves for successful authentication. The client direction
// carries no signal it needs.
type smtpAuthDetector struct {
	mu     sync.Mutex
	server lineAccum
	authed bool
}

func (d *smtpAuthDetector) clientData([]byte) {}

func (d *smtpAuthDetector) serverData(p []byte) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.authed {
		return true
	}
	d.server.feed(p, func(line []byte) {
		if len(line) >= 3 && string(line[:3]) == "235" &&
			(len(line) == 3 || line[3] == ' ' || line[3] == '-') {
			d.authed = true
		}
	})
	return d.authed
}
