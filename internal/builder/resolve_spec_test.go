package builder

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// packWithMigrations extends the standard fixture writer: a feature package
// declaring a migrations directory with files in it, and a base package with
// core/server/pb_migrations.
func packWithMigrations(t *testing.T) packFn {
	t.Helper()
	return func(spec, workDir string) (string, string, error) {
		dir := filepath.Join(workDir, "package")
		switch spec {
		case "@tinycld/todo@1.0.0":
			manifest := `const manifest = {
    name: 'Todo',
    slug: 'todo',
    version: '1.0.0',
    migrations: { directory: 'pb-migrations' },
    peerVersions: { '@tinycld/core': '>=0.0.4 <0.1.0' },
}
export default manifest
`
			if err := os.MkdirAll(filepath.Join(dir, "pb-migrations"), 0o755); err != nil {
				return "", "", err
			}
			if err := os.WriteFile(filepath.Join(dir, "manifest.ts"), []byte(manifest), 0o644); err != nil {
				return "", "", err
			}
			if err := os.WriteFile(filepath.Join(dir, "package.json"),
				[]byte(`{"name": "@tinycld/todo", "version": "1.0.0"}`), 0o644); err != nil {
				return "", "", err
			}
			for _, name := range []string{"1751000001_todo_more.js", "1751000000_todo_init.js"} {
				if err := os.WriteFile(filepath.Join(dir, "pb-migrations", name), []byte("// m"), 0o644); err != nil {
					return "", "", err
				}
			}
		case "@tinycld/bare@1.0.0":
			manifest := `const manifest = { name: 'Bare', slug: 'bare', version: '1.0.0' }
export default manifest
`
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", "", err
			}
			if err := os.WriteFile(filepath.Join(dir, "manifest.ts"), []byte(manifest), 0o644); err != nil {
				return "", "", err
			}
			if err := os.WriteFile(filepath.Join(dir, "package.json"),
				[]byte(`{"name": "@tinycld/bare", "version": "1.0.0"}`), 0o644); err != nil {
				return "", "", err
			}
		case "@tinycld/escape@1.0.0":
			manifest := `const manifest = { name: 'Esc', slug: 'esc', version: '1.0.0', migrations: { directory: '../../outside' } }
export default manifest
`
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", "", err
			}
			if err := os.WriteFile(filepath.Join(dir, "manifest.ts"), []byte(manifest), 0o644); err != nil {
				return "", "", err
			}
			if err := os.WriteFile(filepath.Join(dir, "package.json"),
				[]byte(`{"name": "@tinycld/escape", "version": "1.0.0"}`), 0o644); err != nil {
				return "", "", err
			}
		case "tinycld@1.0.0":
			migDir := filepath.Join(dir, "core", "server", "pb_migrations")
			if err := os.MkdirAll(migDir, 0o755); err != nil {
				return "", "", err
			}
			if err := os.WriteFile(filepath.Join(dir, "core", "package.json"),
				[]byte(`{"name": "@tinycld/core", "version": "0.0.4"}`), 0o644); err != nil {
				return "", "", err
			}
			if err := os.WriteFile(filepath.Join(migDir, "1700000000_core.js"), []byte("// m"), 0o644); err != nil {
				return "", "", err
			}
		default:
			t.Fatalf("unknown spec %q", spec)
		}
		tgz := filepath.Join(workDir, "fake.tgz")
		if err := os.WriteFile(tgz, []byte("tarball:"+spec), 0o644); err != nil {
			return "", "", err
		}
		return dir, tgz, nil
	}
}

func resolveSpecBuilder(t *testing.T) *Builder {
	t.Helper()
	b, err := New(Config{
		Root:         t.TempDir(),
		ScaffoldRoot: writeScaffoldRoot(t),
		Toolchain:    testToolchain,
		Runner:       &fakeRunner{},
		pack:         packWithMigrations(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestResolveSpec_FeatureMember(t *testing.T) {
	b := resolveSpecBuilder(t)

	res, err := b.ResolveSpec("@tinycld/todo@1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if res.Slug != "todo" || res.Name != "@tinycld/todo" || res.Version != "1.0.0" {
		t.Fatalf("identity = %s/%s/%s", res.Slug, res.Name, res.Version)
	}
	if res.Integrity == "" {
		t.Fatal("integrity missing")
	}
	if res.PeerVersions["@tinycld/core"] != ">=0.0.4 <0.1.0" {
		t.Fatalf("peerVersions = %v", res.PeerVersions)
	}
	if res.Manifest == nil {
		t.Fatal("evaluated manifest missing")
	}
	// Sorted basenames — the contribution list D6's delta computation needs.
	want := []string{"1751000000_todo_init.js", "1751000001_todo_more.js"}
	if !reflect.DeepEqual(res.Migrations, want) {
		t.Fatalf("migrations = %v, want %v", res.Migrations, want)
	}
}

func TestResolveSpec_BaseMember(t *testing.T) {
	b := resolveSpecBuilder(t)

	res, err := b.ResolveSpec("tinycld@1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if res.Slug != "core" {
		t.Fatalf("base resolves to registry slug %q, want core", res.Slug)
	}
	if res.Version != "0.0.4" {
		t.Fatalf("base version = %q, want CORE's version", res.Version)
	}
	if res.Manifest != nil {
		t.Fatal("base must carry no manifest")
	}
	if !reflect.DeepEqual(res.Migrations, []string{"1700000000_core.js"}) {
		t.Fatalf("migrations = %v", res.Migrations)
	}
}

func TestResolveSpec_NoMigrationsDeclared(t *testing.T) {
	b := resolveSpecBuilder(t)

	res, err := b.ResolveSpec("@tinycld/bare@1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if res.Migrations == nil || len(res.Migrations) != 0 {
		t.Fatalf("undeclared migrations must be an empty (non-nil) list, got %v", res.Migrations)
	}
}

func TestResolveSpec_MigrationsDirEscapeRefused(t *testing.T) {
	b := resolveSpecBuilder(t)

	if _, err := b.ResolveSpec("@tinycld/escape@1.0.0"); err == nil {
		t.Fatal("a migrations directory escaping the package must be refused")
	}
}

func TestResolveSpec_InvalidSpecRefused(t *testing.T) {
	b := resolveSpecBuilder(t)

	if _, err := b.ResolveSpec("; rm -rf /"); err == nil {
		t.Fatal("invalid spec must be refused before any pack")
	}
}

func TestArtifactMigrationBasenames(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pb_migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"b.js", "a.js"} {
		if err := os.WriteFile(filepath.Join(dir, "pb_migrations", name), []byte("//"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ArtifactMigrationBasenames(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"a.js", "b.js"}) {
		t.Fatalf("got %v", got)
	}

	empty, err := ArtifactMigrationBasenames(t.TempDir())
	if err != nil || len(empty) != 0 {
		t.Fatalf("missing dir should be empty list, got %v / %v", empty, err)
	}
}
