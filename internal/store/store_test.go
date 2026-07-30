package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStore_PublishAndResolve(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	files := map[string][]byte{
		"server/main.pb.js":      []byte("routerAdd('GET','/x',()=>{})"),
		"client/dist/index.html": []byte("<html></html>"),
	}
	if err := s.Publish("@tinycld/core", "2.4.0", files); err != nil {
		t.Fatal(err)
	}

	dir, err := s.VersionDir("@tinycld/core", "2.4.0")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "server", "main.pb.js"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "routerAdd('GET','/x',()=>{})" {
		t.Fatalf("unexpected content: %s", got)
	}
}

func TestStore_PublishIsImmutable(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	files := map[string][]byte{"server/a.pb.js": []byte("1")}
	if err := s.Publish("pkg", "1.0.0", files); err != nil {
		t.Fatal(err)
	}
	err := s.Publish("pkg", "1.0.0", map[string][]byte{"server/a.pb.js": []byte("2")})
	if err == nil {
		t.Fatal("expected re-publishing an existing version to error (immutable)")
	}
}

func TestStore_VersionDirMissing(t *testing.T) {
	s := New(t.TempDir())
	if _, err := s.VersionDir("nope", "9.9.9"); err == nil {
		t.Fatal("expected error for missing version")
	}
}

// A package name and version reach filepath.Join unvalidated, from a lockfile
// (lockfile.go) or a publish request body (provisioning.go). Both are
// superuser-supplied today, which is why this is low severity — but "..%2F"
// segments escape the store root entirely, so a publish could write anywhere
// the router process can reach and a resolve could hand a tenant a directory
// outside the store.
func TestStore_RejectsPathTraversal(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	hostile := []struct{ name, version string }{
		{"../../../etc", "1.0.0"},
		{"@tinycld/core", "../../../../tmp"},
		{"..", "1.0.0"},
		{"a/../../b", "1.0.0"},
		{"@tinycld/core", ".."},
		{"", "1.0.0"},
		{"@tinycld/core", ""},
	}

	for _, tc := range hostile {
		t.Run(tc.name+"@"+tc.version, func(t *testing.T) {
			err := s.Publish(tc.name, tc.version, map[string][]byte{"a.js": []byte("x")})
			if err == nil {
				t.Errorf("Publish(%q, %q) was accepted; it must be rejected", tc.name, tc.version)
			}
			if _, err := s.VersionDir(tc.name, tc.version); err == nil {
				t.Errorf("VersionDir(%q, %q) resolved; it must be rejected", tc.name, tc.version)
			}
		})
	}
}

// The file paths inside a publish are the same problem one level down: a
// relative path with .. segments escapes the version directory.
func TestStore_RejectsTraversalInFileNames(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	err := s.Publish("pkg", "1.0.0", map[string][]byte{
		"../../../escaped.js": []byte("x"),
	})
	if err == nil {
		t.Fatal("a file path escaping the version dir was accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "escaped.js")); err == nil {
		t.Fatal("the file was written outside the store root")
	}
}

// The positive control: ordinary scoped names and semver versions still work.
// Without it, rejecting everything would pass the tests above.
func TestStore_AcceptsOrdinaryNames(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	for _, tc := range []struct{ name, version string }{
		{"@tinycld/core", "2.4.0"},
		{"@acme/custom-pkg", "0.1.0-beta.1"},
		{"mail", "1.0.0"},
		{"google-takeout-import", "1.2.3+build.4"},
	} {
		if err := s.Publish(tc.name, tc.version, map[string][]byte{"a.js": []byte("x")}); err != nil {
			t.Errorf("Publish(%q, %q): %v", tc.name, tc.version, err)
		}
		if _, err := s.VersionDir(tc.name, tc.version); err != nil {
			t.Errorf("VersionDir(%q, %q): %v", tc.name, tc.version, err)
		}
	}
}

// M6: a failed publish must leave NOTHING behind. Before staging, a partial
// write left a half-populated version directory that every retry refused as
// "already published (immutable)" — the only recourse was rm -rf inside the
// store by hand.
func TestStore_FailedPublishIsRepairable(t *testing.T) {
	s := New(t.TempDir())

	// One good file, one that escapes the version dir: the write fails midway.
	err := s.Publish("@tinycld/mail", "1.0.0", map[string][]byte{
		"manifest.json": []byte(`{}`),
		"../escape":     []byte("nope"),
	})
	if err == nil {
		t.Fatal("expected the escaping path to fail the publish")
	}

	// The retry with a fixed payload must succeed — no manual cleanup.
	if err := s.Publish("@tinycld/mail", "1.0.0", map[string][]byte{
		"manifest.json": []byte(`{"name":"@tinycld/mail"}`),
	}); err != nil {
		t.Fatalf("retry after failed publish = %v — the store is not repairable", err)
	}

	dir, err := s.VersionDir("@tinycld/mail", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatalf("published file missing: %v", err)
	}
}

// A version directory must appear atomically: never in a state where a
// concurrent resolve sees some files but not others.
func TestStore_PublishStagesThenRenames(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	if err := s.Publish("pkg", "2.0.0", map[string][]byte{
		"a.txt": []byte("a"),
		"b.txt": []byte("b"),
	}); err != nil {
		t.Fatal(err)
	}
	// No staging leftovers next to the published version.
	entries, err := os.ReadDir(filepath.Join(root, "packages", "pkg"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "2.0.0" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("package dir holds %v, want exactly [2.0.0]", names)
	}
}
