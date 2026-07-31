package materialize

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"tinycld.org/multi-org/internal/lockfile"
	"tinycld.org/multi-org/internal/store"
)

// Deploy re-materializes a RUNNING tenant's directories before evicting it, and
// manager.load can re-materialize while an evicted predecessor is still
// draining. The live tenant serves static assets out of pb_public the whole
// time, so a rebuild that clears the live directory in place makes the app
// 404 for the duration of every deploy. The rebuild must stage the new tree
// and swap it in atomically: an observer must never catch the live tree
// missing a file that both the old and new trees contain.
func TestMaterialize_LiveTreeNeverGoesDark(t *testing.T) {
	root := t.TempDir()
	s := store.New(root)
	if err := s.Publish("@tinycld/core", "2.4.0", map[string][]byte{
		"server/main.pb.js":      []byte("routerAdd('GET','/x',()=>{})"),
		"client/dist/index.html": []byte("<html>core</html>"),
	}); err != nil {
		t.Fatal(err)
	}
	lf, _ := lockfile.Parse([]byte(`{"@tinycld/core":"2.4.0"}`))
	resolved, err := lf.Resolve(s)
	if err != nil {
		t.Fatal(err)
	}

	orgDir := filepath.Join(t.TempDir(), "acme")
	if err := Materialize(orgDir, resolved); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(orgDir, "pb_public", "index.html")

	// The observer plays the running tenant: it reads the asset continuously
	// while deploys rebuild the tree underneath it. The failure under test is
	// SUSTAINED darkness — the in-place clear leaves the path gone for the
	// whole rebuild (and live, for the whole drain), so a tight stat loop
	// fails many times in a row. A single isolated stat failure is tolerated:
	// on darwin, rename-over-symlink can surface a one-syscall EINVAL blip to
	// a concurrently resolving lookup even though the path never lacks a
	// complete tree.
	var stop, worstStreak atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		streak := int64(0)
		for stop.Load() == 0 {
			if _, err := os.Stat(index); err != nil {
				streak++
				if streak > worstStreak.Load() {
					worstStreak.Store(streak)
				}
			} else {
				streak = 0
			}
		}
	}()

	for i := 0; i < 50; i++ {
		if err := Materialize(orgDir, resolved); err != nil {
			stop.Store(1)
			<-done
			t.Fatal(err)
		}
	}
	stop.Store(1)
	<-done

	if n := worstStreak.Load(); n >= 3 {
		t.Fatalf("live asset was missing for %d consecutive lookups during re-materialize — the rebuild clears the live tree instead of staging and swapping", n)
	}
}

func TestMaterialize_LinksHooksAndPublic(t *testing.T) {
	root := t.TempDir()
	s := store.New(root)
	if err := s.Publish("@tinycld/core", "2.4.0", map[string][]byte{
		"server/main.pb.js":      []byte("routerAdd('GET','/x',()=>{})"),
		"client/dist/index.html": []byte("<html>core</html>"),
		"client/dist/app/app.js": []byte("console.log(1)"),
	}); err != nil {
		t.Fatal(err)
	}
	lf, _ := lockfile.Parse([]byte(`{"@tinycld/core":"2.4.0"}`))
	resolved, err := lf.Resolve(s)
	if err != nil {
		t.Fatal(err)
	}

	orgDir := filepath.Join(t.TempDir(), "acme")
	if err := Materialize(orgDir, resolved); err != nil {
		t.Fatal(err)
	}

	// pb_hooks/main.pb.js resolves (through the symlink) to the store content.
	hook, err := os.ReadFile(filepath.Join(orgDir, "pb_hooks", "main.pb.js"))
	if err != nil {
		t.Fatalf("reading materialized hook: %v", err)
	}
	if string(hook) != "routerAdd('GET','/x',()=>{})" {
		t.Fatalf("unexpected hook content: %s", hook)
	}

	// pb_public preserves nested client/dist structure.
	idx, err := os.ReadFile(filepath.Join(orgDir, "pb_public", "index.html"))
	if err != nil {
		t.Fatalf("reading materialized index: %v", err)
	}
	if string(idx) != "<html>core</html>" {
		t.Fatalf("unexpected index content: %s", idx)
	}
	if _, err := os.Stat(filepath.Join(orgDir, "pb_public", "app", "app.js")); err != nil {
		t.Fatalf("expected nested client asset materialized: %v", err)
	}
}

func TestMaterialize_LinksMigrations(t *testing.T) {
	root := t.TempDir()
	s := store.New(root)
	if err := s.Publish("@tinycld/core", "2.4.0", map[string][]byte{
		"pb-migrations/1700000000_init.js": []byte("migrate((app)=>{},(app)=>{})"),
	}); err != nil {
		t.Fatal(err)
	}
	lf, _ := lockfile.Parse([]byte(`{"@tinycld/core":"2.4.0"}`))
	resolved, err := lf.Resolve(s)
	if err != nil {
		t.Fatal(err)
	}

	orgDir := filepath.Join(t.TempDir(), "acme")
	if err := Materialize(orgDir, resolved); err != nil {
		t.Fatal(err)
	}

	// The hyphenated source pb-migrations lands in the underscore pb_migrations
	// the tenant app reads, with content resolving through the symlink.
	mig, err := os.ReadFile(filepath.Join(orgDir, "pb_migrations", "1700000000_init.js"))
	if err != nil {
		t.Fatalf("reading materialized migration: %v", err)
	}
	if string(mig) != "migrate((app)=>{},(app)=>{})" {
		t.Fatalf("unexpected migration content: %s", mig)
	}
}

func TestMaterialize_MigrationCollisionErrors(t *testing.T) {
	root := t.TempDir()
	s := store.New(root)
	// Two packages contributing the same migration filename is a real bug.
	if err := s.Publish("@tinycld/core", "2.4.0", map[string][]byte{
		"pb-migrations/1700000000_init.js": []byte("core"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Publish("@tinycld/mail", "1.2.0", map[string][]byte{
		"pb-migrations/1700000000_init.js": []byte("mail"),
	}); err != nil {
		t.Fatal(err)
	}
	lf, _ := lockfile.Parse([]byte(`{"@tinycld/core":"2.4.0","@tinycld/mail":"1.2.0"}`))
	resolved, _ := lf.Resolve(s)

	orgDir := filepath.Join(t.TempDir(), "acme")
	err := Materialize(orgDir, resolved)
	if err == nil {
		t.Fatal("expected a migration filename collision error, got nil")
	}
}

func TestMaterialize_IsIdempotent(t *testing.T) {
	root := t.TempDir()
	s := store.New(root)
	_ = s.Publish("pkg", "1.0.0", map[string][]byte{"server/a.pb.js": []byte("1")})
	lf, _ := lockfile.Parse([]byte(`{"pkg":"1.0.0"}`))
	resolved, _ := lf.Resolve(s)
	orgDir := filepath.Join(t.TempDir(), "org")
	if err := Materialize(orgDir, resolved); err != nil {
		t.Fatal(err)
	}
	// Second run must not error (clears + relinks).
	if err := Materialize(orgDir, resolved); err != nil {
		t.Fatalf("re-materialize failed: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(orgDir, "pb_hooks"))
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 hook after re-materialize, got %d", len(entries))
	}
}

// An artifact-backed org's live names point straight into the committed build
// tree, the transition from a store materialization is atomic, and leftover
// store-era generations are pruned to the newest (a draining predecessor may
// still read it).
func TestMaterializeBuild_PointsLiveNamesAtArtifact(t *testing.T) {
	artifact := t.TempDir()
	for _, name := range []string{"pb_hooks", "pb_public", "pb_migrations"} {
		if err := os.MkdirAll(filepath.Join(artifact, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(artifact, "pb_public", "index.html"), []byte("<html>built</html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Start from a store materialization, as an org migrating onto its first
	// artifact would.
	root := t.TempDir()
	s := store.New(root)
	if err := s.Publish("@tinycld/core", "2.4.0", map[string][]byte{
		"client/dist/index.html": []byte("<html>store</html>"),
	}); err != nil {
		t.Fatal(err)
	}
	lf, _ := lockfile.Parse([]byte(`{"@tinycld/core":"2.4.0"}`))
	resolved, err := lf.Resolve(s)
	if err != nil {
		t.Fatal(err)
	}
	orgDir := filepath.Join(t.TempDir(), "acme")
	if err := Materialize(orgDir, resolved); err != nil {
		t.Fatal(err)
	}
	if err := Materialize(orgDir, resolved); err != nil { // two generations on disk
		t.Fatal(err)
	}

	if err := MaterializeBuild(orgDir, artifact); err != nil {
		t.Fatal(err)
	}

	idx, err := os.ReadFile(filepath.Join(orgDir, "pb_public", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if string(idx) != "<html>built</html>" {
		t.Fatalf("live pb_public reads %q, want the artifact's tree", idx)
	}
	target, err := os.Readlink(filepath.Join(orgDir, "pb_hooks"))
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Join(artifact, "pb_hooks") {
		t.Fatalf("pb_hooks -> %q, want absolute artifact path", target)
	}

	// Store-era generations pruned to the newest only.
	entries, err := os.ReadDir(orgDir)
	if err != nil {
		t.Fatal(err)
	}
	gens := 0
	for _, e := range entries {
		if _, ok := parseGeneration(e.Name(), "pb_public"); ok {
			gens++
		}
	}
	if gens != 1 {
		t.Fatalf("%d pb_public generations survive, want 1", gens)
	}

	// Idempotent, and safe to run again over an artifact-backed org.
	if err := MaterializeBuild(orgDir, artifact); err != nil {
		t.Fatal(err)
	}
}
