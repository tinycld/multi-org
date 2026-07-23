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
