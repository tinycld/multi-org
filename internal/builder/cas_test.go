package builder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tinycld.org/core/pkgbuild"
)

const testHash = "sha256:" + testHex

const testHex = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func stageTestArtifact(t *testing.T, root string, r Recipe) string {
	t.Helper()
	dir, err := os.MkdirTemp(root, "stage-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeRecipeFile(dir, r); err != nil {
		t.Fatal(err)
	}
	return dir
}

func testRecipe(hash string) Recipe {
	return Recipe{
		RecipeHash: hash,
		BuildID:    "recipe-aaaaaaaaaaaa",
		BuiltAt:    time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC),
		Toolchain:  pkgbuild.Toolchain{Go: "go1.26.3", Node: "v22.12.0", Pnpm: "pnpm@11.3.0"},
		Members: []pkgbuild.ResolvedMember{
			{Slug: "tinycld", Name: "@tinycld/core", Version: "0.0.9", Integrity: "sha256:beef"},
		},
		Overrides: map[string]string{"uniwind": "1.8.0"},
	}
}

func TestStore_RejectsMalformedHashes(t *testing.T) {
	s := NewStore(t.TempDir())
	for _, hash := range []string{
		"",
		testHex,                    // missing prefix
		"sha256:short",             // wrong length
		"sha256:../../etc/passwd",  // traversal
		"sha512:" + testHex,        // wrong algorithm
		"sha256:" + strings.ToUpper(testHex), // not lowercase hex
	} {
		if _, err := s.Dir(hash); err == nil {
			t.Errorf("Dir(%q): want error, got none", hash)
		}
	}
}

func TestStore_CommitHasReadRemove(t *testing.T) {
	root := t.TempDir()
	s := NewStore(filepath.Join(root, "builds"))

	if ok, err := s.Has(testHash); err != nil || ok {
		t.Fatalf("Has before commit = %v, %v; want false, nil", ok, err)
	}

	stage := stageTestArtifact(t, root, testRecipe(testHash))
	dir, err := s.Commit(testHash, stage)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "builds", testHex); dir != want {
		t.Fatalf("Commit dir = %q, want %q", dir, want)
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatalf("stage dir still exists after commit")
	}

	if ok, err := s.Has(testHash); err != nil || !ok {
		t.Fatalf("Has after commit = %v, %v; want true, nil", ok, err)
	}
	recipe, err := s.ReadRecipe(testHash)
	if err != nil {
		t.Fatal(err)
	}
	if recipe.RecipeHash != testHash || recipe.Members[0].Slug != "tinycld" {
		t.Fatalf("ReadRecipe = %+v", recipe)
	}

	entries, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].RecipeHash != testHash {
		t.Fatalf("List = %+v", entries)
	}

	if err := s.Remove(testHash); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Has(testHash); ok {
		t.Fatal("Has after Remove = true")
	}
	if err := s.Remove(testHash); err != nil {
		t.Fatalf("Remove of absent entry: %v", err)
	}
}

func TestStore_CommitRequiresRecipeFile(t *testing.T) {
	root := t.TempDir()
	s := NewStore(filepath.Join(root, "builds"))
	stage, err := os.MkdirTemp(root, "stage-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(testHash, stage); err == nil {
		t.Fatal("Commit without recipe.json succeeded")
	}
}

func TestStore_CommitLosingRaceAdoptsWinner(t *testing.T) {
	root := t.TempDir()
	s := NewStore(filepath.Join(root, "builds"))

	winner := stageTestArtifact(t, root, testRecipe(testHash))
	if _, err := s.Commit(testHash, winner); err != nil {
		t.Fatal(err)
	}

	loser := stageTestArtifact(t, root, testRecipe(testHash))
	if err := os.WriteFile(filepath.Join(loser, "marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir, err := s.Commit(testHash, loser)
	if err != nil {
		t.Fatalf("losing commit: %v", err)
	}
	// The winner's entry survives; the loser's tree is discarded.
	if _, err := os.Stat(filepath.Join(dir, "marker")); !os.IsNotExist(err) {
		t.Fatal("losing commit replaced the winner's entry")
	}
	if _, err := os.Stat(loser); !os.IsNotExist(err) {
		t.Fatal("losing stage dir was not cleaned up")
	}
}

func TestStore_CommitOpensTreeAndPreservesExec(t *testing.T) {
	root := t.TempDir()
	s := NewStore(filepath.Join(root, "builds"))
	stage := stageTestArtifact(t, root, testRecipe(testHash))

	if err := os.WriteFile(filepath.Join(stage, "tinycld"), []byte("#!bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stage, "pb_hooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "pb_hooks", "main.pb.js"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	dir, err := s.Commit(testHash, stage)
	if err != nil {
		t.Fatal(err)
	}
	assertMode := func(rel string, want os.FileMode) {
		t.Helper()
		info, err := os.Stat(filepath.Join(dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode = %v, want %v", rel, info.Mode().Perm(), want)
		}
	}
	assertMode("tinycld", 0o755)
	assertMode("pb_hooks", 0o755)
	assertMode(filepath.Join("pb_hooks", "main.pb.js"), 0o644)
}
