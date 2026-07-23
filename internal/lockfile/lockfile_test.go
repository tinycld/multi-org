package lockfile

import (
	"path/filepath"
	"testing"

	"tinycld.org/multi-org/internal/store"
)

func TestLockfile_ParseAndResolve(t *testing.T) {
	root := t.TempDir()
	s := store.New(root)
	if err := s.Publish("@tinycld/core", "2.4.0", map[string][]byte{"server/a.pb.js": []byte("1")}); err != nil {
		t.Fatal(err)
	}

	lf, err := Parse([]byte(`{"@tinycld/core":"2.4.0"}`))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := lf.Resolve(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 {
		t.Fatalf("expected 1 resolved package, got %d", len(resolved))
	}
	if resolved[0].Name != "@tinycld/core" || resolved[0].Version != "2.4.0" {
		t.Fatalf("unexpected resolution: %+v", resolved[0])
	}
	if filepath.Base(resolved[0].Dir) != "2.4.0" {
		t.Fatalf("unexpected dir: %s", resolved[0].Dir)
	}
}

func TestLockfile_ResolveMissingVersionErrors(t *testing.T) {
	s := store.New(t.TempDir())
	lf, _ := Parse([]byte(`{"@tinycld/core":"9.9.9"}`))
	if _, err := lf.Resolve(s); err == nil {
		t.Fatal("expected resolve to fail for missing store version")
	}
}
