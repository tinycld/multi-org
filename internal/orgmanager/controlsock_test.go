package orgmanager

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// H4: the control-socket server must carry the slow-loris timeout profile and a
// per-socket connection cap, so an attacker-controlled tenant cannot exhaust the
// router's fds by holding connections open or dribbling headers.
func TestStartControl_HardenedServer(t *testing.T) {
	path := filepath.Join(shortRoot(t), "ctl.sock")
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	c, err := startControl(path, h, quietLogger())
	if err != nil {
		t.Fatalf("startControl: %v", err)
	}
	defer c.close(quietLogger(), "acme")

	srv := c.srv
	if srv.ReadHeaderTimeout != ctlReadHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want %v", srv.ReadHeaderTimeout, ctlReadHeaderTimeout)
	}
	if srv.ReadTimeout != ctlReadTimeout {
		t.Errorf("ReadTimeout = %v, want %v", srv.ReadTimeout, ctlReadTimeout)
	}
	if srv.WriteTimeout != ctlWriteTimeout {
		t.Errorf("WriteTimeout = %v, want %v", srv.WriteTimeout, ctlWriteTimeout)
	}
	if srv.IdleTimeout != ctlIdleTimeout {
		t.Errorf("IdleTimeout = %v, want %v", srv.IdleTimeout, ctlIdleTimeout)
	}
	if srv.MaxHeaderBytes != ctlMaxHeaderBytes {
		t.Errorf("MaxHeaderBytes = %d, want %d", srv.MaxHeaderBytes, ctlMaxHeaderBytes)
	}
}

// A client that opens a connection and dribbles a partial request line without
// ever completing the headers is disconnected by the server's ReadHeaderTimeout
// rather than pinning the fd forever. Bounded well under the 5s timeout budget
// on the read side by giving the read its own deadline.
func TestStartControl_SlowHeaderClientTimesOut(t *testing.T) {
	path := filepath.Join(shortRoot(t), "ctl.sock")
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	c, err := startControl(path, h, quietLogger())
	if err != nil {
		t.Fatalf("startControl: %v", err)
	}
	defer c.close(quietLogger(), "acme")

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Send a partial request line and never finish the headers.
	if _, err := fmt.Fprint(conn, "GET /v1/state HTTP/1.1\r\n"); err != nil {
		t.Fatalf("write partial: %v", err)
	}

	// The server should close the connection once ReadHeaderTimeout elapses. Give
	// the read a deadline generous enough to observe that close but not to hang
	// the suite if the timeout regressed.
	_ = conn.SetReadDeadline(time.Now().Add(ctlReadHeaderTimeout + 3*time.Second))
	r := bufio.NewReader(conn)
	// A closed connection surfaces as EOF (or a reset); either way Read returns a
	// non-nil error. A regressed server would instead block until our own
	// deadline, which we treat as a failure.
	if _, err := r.ReadByte(); err == nil {
		t.Fatal("server accepted a byte from a stalled slow-header client; ReadHeaderTimeout not enforced")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("client-side read deadline fired before the server closed the connection; slow-loris timeout not enforced")
	}
}
