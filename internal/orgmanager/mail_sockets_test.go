package orgmanager

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The per-org mail socket contract these tests pin: an org whose resolved
// package set includes mail gets imap/smtp/mx unix sockets NEXT TO its HTTP
// socket — one directory per org, because the Linux spawner chowns exactly
// that directory to the tenant's uid (P0-1) and a socket in any other
// directory would be either unreachable or another org's to unlink. Orgs
// without mail get none, and the spawn request carries the paths so serve-org
// binds them itself (the same child-binds precedent as the HTTP socket).

// shortRoot returns a root shallow enough that the primary socket layout fits
// sun_path — t.TempDir() on darwin is deep enough to force the fallback,
// which is not the layout these tests assert.
func shortRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mtsock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestSocketPaths_MailSocketsShareOneOrgDir(t *testing.T) {
	m := &OrgManager{cfg: Config{Root: shortRoot(t)}}

	socks, err := m.socketPaths("acme", true)
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(m.cfg.Root, "run", "acme")
	for name, p := range map[string]string{
		"http": socks.http, "imap": socks.imap, "smtp": socks.smtp, "mx": socks.mx,
	} {
		if filepath.Dir(p) != wantDir {
			t.Errorf("%s socket %q not in the org dir %q", name, p, wantDir)
		}
	}
	if socks.http != filepath.Join(wantDir, "acme.sock") {
		t.Errorf("http socket = %q, want the legacy <slug>.sock name", socks.http)
	}
}

func TestSocketPaths_WithoutMailMatchesLegacyLayout(t *testing.T) {
	m := &OrgManager{cfg: Config{Root: shortRoot(t)}}

	socks, err := m.socketPaths("acme", false)
	if err != nil {
		t.Fatal(err)
	}
	if socks.http != filepath.Join(m.cfg.Root, "run", "acme", "acme.sock") {
		t.Fatalf("http socket = %q", socks.http)
	}
	if socks.imap != "" || socks.smtp != "" || socks.mx != "" {
		t.Fatalf("no-mail org must get no mail sockets, got %+v", socks)
	}
}

// A root deep enough to overrun sun_path must relocate EVERY socket to the
// hashed fallback dir together — a split (http primary, mail fallback) would
// leave the mail sockets in a directory the spawner never chowned.
func TestSocketPaths_DeepRootFallbackKeepsAllSocketsTogether(t *testing.T) {
	deep := filepath.Join(t.TempDir(), strings.Repeat("d", maxSocketPath))
	m := &OrgManager{cfg: Config{Root: deep}}

	socks, err := m.socketPaths("acme", true)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(socks.http)
	if strings.HasPrefix(dir, deep) {
		t.Fatalf("expected fallback dir, got %q under the deep root", socks.http)
	}
	for name, p := range map[string]string{"imap": socks.imap, "smtp": socks.smtp, "mx": socks.mx} {
		if filepath.Dir(p) != dir {
			t.Errorf("%s socket %q split from the org dir %q", name, p, dir)
		}
		if len(p) > maxSocketPath {
			t.Errorf("%s socket %q exceeds sun_path budget", name, p)
		}
	}
}

func TestBuildCmd_MailSocketFlags(t *testing.T) {
	req := SpawnRequest{
		Slug: "acme", OrgDir: t.TempDir(), SocketPath: "/run/acme/acme.sock",
		BinaryPath:     "/bin/true",
		IMAPSocketPath: "/run/acme/imap.sock",
		SMTPSocketPath: "/run/acme/smtp.sock",
		MXSocketPath:   "/run/acme/mx.sock",
	}
	args := strings.Join(buildCmd(req, quietLogger()).Args, " ")
	for _, want := range []string{
		"--imap-socket /run/acme/imap.sock",
		"--smtp-socket /run/acme/smtp.sock",
		"--mx-socket /run/acme/mx.sock",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("argv missing %q: %s", want, args)
		}
	}

	bare := strings.Join(buildCmd(SpawnRequest{
		Slug: "acme", OrgDir: t.TempDir(), SocketPath: "/run/acme/acme.sock",
		BinaryPath: "/bin/true",
	}, quietLogger()).Args, " ")
	if strings.Contains(bare, "-socket-imap") || strings.Contains(bare, "--imap-socket") {
		t.Errorf("no-mail spawn must not pass mail socket flags: %s", bare)
	}
}

// The manager must hand mail socket paths to the spawn exactly when the org's
// resolved package set — read from the artifact's staged manifests — includes
// mail. A socket for an org that never starts the listener is a dead
// rendezvous point, and a mail org without one is unreachable by the mail
// router.
func TestGet_MailSocketPathsFollowResolvedPackageSet(t *testing.T) {
	newMgr := func(members ...artifactMember) (*OrgManager, *fakeSpawner) {
		sp := &fakeSpawner{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("ok"))
		})}
		root := t.TempDir()
		ref := buildArtifact(t, buildsRootFor(root), artifactSpec{
			Hash:    "core1",
			Members: members,
		})
		mgr := New(Config{
			Root: root, Spawner: sp, Logger: quietLogger(), HooksPool: 2,
			PackageSlugs: slugsFromManifests,
			ResolveBuild: resolveBuilds(map[string]BuildRef{"core1": ref}),
			LookupOrg: stubLookup(map[string]OrgRecord{
				"acme": {Slug: "acme", Status: "active", RecipeHash: "core1"},
			}),
		})
		t.Cleanup(mgr.Shutdown)
		return mgr, sp
	}

	withMail, sp := newMgr(artifactMember{Slug: "contacts"}, artifactMember{Slug: "mail"})
	if _, err := withMail.Get(context.Background(), "acme"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	req := sp.lastReq()
	if req.IMAPSocketPath == "" || req.SMTPSocketPath == "" || req.MXSocketPath == "" {
		t.Fatalf("mail org spawn missing mail socket paths: %+v", req)
	}
	if filepath.Dir(req.IMAPSocketPath) != filepath.Dir(req.SocketPath) {
		t.Fatalf("mail socket %q not beside http socket %q", req.IMAPSocketPath, req.SocketPath)
	}

	without, sp2 := newMgr(artifactMember{Slug: "contacts"})
	if _, err := without.Get(context.Background(), "acme"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if req := sp2.lastReq(); req.IMAPSocketPath != "" || req.SMTPSocketPath != "" || req.MXSocketPath != "" {
		t.Fatalf("no-mail org spawn must carry no mail socket paths: %+v", req)
	}
}

// Long-lived mail connections must keep an org resident: the sweeper evicting
// an org under an open IMAP connection kills the tenant mid-session.
func TestSweep_SkipsInstancesWithTrackedConns(t *testing.T) {
	sp := &fakeSpawner{}
	root := t.TempDir()
	ref := buildArtifact(t, buildsRootFor(root), artifactSpec{Hash: "core1"})
	mgr := New(Config{
		Root: root, Spawner: sp, Logger: quietLogger(), HooksPool: 2,
		ResolveBuild: resolveBuilds(map[string]BuildRef{"core1": ref}),
		LookupOrg: stubLookup(map[string]OrgRecord{
			"acme": {Slug: "acme", Status: "active", RecipeHash: "core1"},
		}),
	})
	t.Cleanup(mgr.Shutdown)

	inst, err := mgr.Get(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	release := inst.TrackConn()

	// Age the instance far past any cutoff, then run one sweep pass by hand.
	inst.lastUsed.Store(1)
	mgr.cfg.MaxIdle = 1 // nanoseconds — everything untracked is stale
	mgr.sweepOnce()
	if _, ok := mgr.orgs["acme"]; !ok {
		t.Fatal("org with an open tracked connection was swept")
	}

	release()
	inst.lastUsed.Store(1) // release() touched; re-age past the cutoff
	mgr.sweepOnce()
	if _, ok := mgr.orgs["acme"]; ok {
		t.Fatal("org stayed resident after its last tracked connection closed")
	}
}
