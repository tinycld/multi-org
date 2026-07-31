package orgmanager

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
)

// controlServer serves the router-side control API on one org's ctl.sock —
// the deploy-proposal channel (design D1). Unlike every other per-org socket
// it is bound by the ROUTER and dialed by the tenant: package-change
// execution (restart, revert) must stay with the router, and a socket the
// router owns is what lets a tenant propose without ever being able to
// self-restart. Authentication is the filesystem: the socket lives in the
// org's 0700 tenant-uid socket directory, so the dialer can only be that
// org's uid (or root), and a compromised tenant can only ever propose
// changes to itself.
type controlServer struct {
	path string
	ino  uint64
	srv  *http.Server
}

// startControl binds path and serves h on it. The socket file is opened to
// 0666 — the 0700 directory is the authorization boundary, and the tenant
// uid needs write on the inode to connect().
func startControl(path string, h http.Handler, log *slog.Logger) (*controlServer, error) {
	addr, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", addr)
	if err != nil {
		return nil, err
	}
	// The manager owns socket-file lifecycle with an inode-ownership check
	// (see close): Go's default unlink-on-close would let THIS listener's
	// Close delete a replacement's socket that re-bound the same path.
	ln.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o666); err != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		return nil, err
	}
	c := &controlServer{path: path, ino: socketInode(path), srv: &http.Server{Handler: h}}
	go func() {
		if err := c.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Debug("control socket server stopped", "path", path, "error", err)
		}
	}()
	return c, nil
}

// close stops the server and removes the socket file if this instance still
// owns it (an Evict-then-Get replacement may have re-bound the same path).
// Close rather than Shutdown: control requests are short, and a deploy
// proposal already in a handler ends with the org being killed anyway.
// Nil-safe, so callers need not guard on the manager having no Control hook.
func (c *controlServer) close(log *slog.Logger, slug string) {
	if c == nil {
		return
	}
	_ = c.srv.Close()
	if !socketOwned(c.path, c.ino) {
		return
	}
	if err := os.Remove(c.path); err != nil && !os.IsNotExist(err) {
		log.Warn("could not remove control socket", "slug", slug, "error", err)
	}
}
