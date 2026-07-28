package orgmanager

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tinycld.org/multi-org/internal/lockfile"
	"tinycld.org/multi-org/internal/store"
)

// End-to-end through the REAL serve-org binary: an org whose resolved package
// set includes mail must come up serving mail's actual IMAP session on the
// router-managed imap.sock (in external-TLS mode: plaintext greeting, auth
// allowed), and an org without mail must bind no mail sockets at all. This is
// the tenant half of the demux chain the mailrouter tests fake — together
// they cover public port → SNI/RCPT demux → per-org socket → real session.
func TestTenant_ServesIMAPOnRouterManagedSocket(t *testing.T) {
	bin := buildTenantBinary(t)
	root := t.TempDir()

	s := store.New(root)
	if err := s.Publish("@tinycld/core", "1.0.0", map[string][]byte{
		"client/dist/index.html": []byte("<html></html>"),
		"server/main.pb.js":      []byte("routerAdd('GET','/ping',(e)=>e.json(200,{ok:true}))"),
	}); err != nil {
		t.Fatal(err)
	}
	// Only manifest.json — mail's Go is linked into the binary; the store
	// copy is what puts "mail" in the org's resolved set.
	if err := s.Publish("@tinycld/mail", "1.0.0", map[string][]byte{
		"manifest.json": []byte(`{"slug":"mail"}`),
	}); err != nil {
		t.Fatal(err)
	}

	mgr := New(Config{
		Root:         root,
		Store:        s,
		Spawner:      execSpawner{},
		TenantBinary: bin,
		Logger:       quietLogger(),
		HooksPool:    2,
		PackageSlugs: func(resolved []lockfile.ResolvedPackage) ([]string, error) {
			var slugs []string
			for _, pkg := range resolved {
				slug := pkg.Name
				if body, err := os.ReadFile(filepath.Join(pkg.Dir, "manifest.json")); err == nil {
					var m struct {
						Slug string `json:"slug"`
					}
					if json.Unmarshal(body, &m) == nil && m.Slug != "" {
						slug = m.Slug
					}
				}
				slugs = append(slugs, slug)
			}
			return slugs, nil
		},
		LookupOrg: stubLookup(map[string]OrgRecord{
			"withmail": {Slug: "withmail", Status: "active",
				Lockfile: []byte(`{"@tinycld/core":"1.0.0","@tinycld/mail":"1.0.0"}`)},
			"nomail": {Slug: "nomail", Status: "active",
				Lockfile: []byte(`{"@tinycld/core":"1.0.0"}`)},
		}),
	})
	t.Cleanup(mgr.Shutdown)

	ctx, cancel := context.WithTimeout(context.Background(), spawnTimeout+15*time.Second)
	defer cancel()

	inst, err := mgr.Get(ctx, "withmail")
	if err != nil {
		t.Fatalf("Get(withmail): %v", err)
	}
	socks := inst.MailSockets()
	if socks.IMAP == "" || socks.SMTP == "" || socks.MX == "" {
		t.Fatalf("mail org must expose all three mail sockets, got %+v", socks)
	}

	// The real mail IMAP session, over the plaintext unix socket the tenant
	// bound — greeting plus a capability probe proving auth is allowed
	// without TLS (the router terminates it) and STARTTLS is not offered.
	conn, err := net.Dial("unix", socks.IMAP)
	if err != nil {
		t.Fatalf("dial tenant imap socket: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	rd := bufio.NewReader(conn)
	greeting, err := rd.ReadString('\n')
	if err != nil {
		t.Fatalf("read IMAP greeting: %v", err)
	}
	if !strings.HasPrefix(greeting, "* OK") {
		t.Fatalf("IMAP greeting = %q", greeting)
	}
	if _, err := conn.Write([]byte("a1 CAPABILITY\r\n")); err != nil {
		t.Fatal(err)
	}
	var caps strings.Builder
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("read capabilities: %v (so far %q)", err, caps.String())
		}
		caps.WriteString(line)
		if strings.HasPrefix(line, "a1 ") {
			break
		}
	}
	if strings.Contains(caps.String(), "LOGINDISABLED") || strings.Contains(caps.String(), "STARTTLS") {
		t.Fatalf("tenant IMAP must run in external-TLS mode (auth allowed, no STARTTLS), got %q", caps.String())
	}

	// The submission socket serves SMTP.
	sconn, err := net.Dial("unix", socks.SMTP)
	if err != nil {
		t.Fatalf("dial tenant smtp socket: %v", err)
	}
	defer sconn.Close()
	_ = sconn.SetDeadline(time.Now().Add(10 * time.Second))
	if line, err := bufio.NewReader(sconn).ReadString('\n'); err != nil || !strings.HasPrefix(line, "220 ") {
		t.Fatalf("SMTP greeting = %q, err=%v", line, err)
	}

	// An org without the mail package binds no mail sockets.
	noMail, err := mgr.Get(ctx, "nomail")
	if err != nil {
		t.Fatalf("Get(nomail): %v", err)
	}
	if s := noMail.MailSockets(); s.IMAP != "" || s.SMTP != "" || s.MX != "" {
		t.Fatalf("no-mail org must expose no mail sockets, got %+v", s)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(noMail.sockPath), imapSockName)); !os.IsNotExist(err) {
		t.Fatalf("no-mail org's socket dir contains an imap socket (stat err=%v)", err)
	}
}
