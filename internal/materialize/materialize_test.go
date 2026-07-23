package materialize

import (
	"os"
	"path/filepath"
	"testing"

	"tinycld.org/multitenant/internal/lockfile"
	"tinycld.org/multitenant/internal/store"
)

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
