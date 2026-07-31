package builder

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSweep_RemovesOnlyDeadEntriesPastGrace(t *testing.T) {
	root := t.TempDir()
	s := NewStore(filepath.Join(root, "builds"))
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

	commit := func(hexByte string, builtAt time.Time) string {
		t.Helper()
		hash := "sha256:" + strings.Repeat(hexByte, 64)
		r := testRecipe(hash)
		r.BuiltAt = builtAt
		if _, err := s.Commit(hash, stageTestArtifact(t, root, r)); err != nil {
			t.Fatal(err)
		}
		return hash
	}

	liveOld := commit("a", now.Add(-24*time.Hour))
	deadOld := commit("b", now.Add(-24*time.Hour))
	deadFresh := commit("c", now.Add(-time.Minute)) // inside grace: the deploy window

	removed, err := s.Sweep(func(h string) bool { return h == liveOld }, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != deadOld {
		t.Fatalf("removed = %v, want [%s]", removed, deadOld)
	}
	for hash, want := range map[string]bool{liveOld: true, deadOld: false, deadFresh: true} {
		if ok, _ := s.Has(hash); ok != want {
			t.Errorf("Has(%s) = %v, want %v", hash[:16], ok, want)
		}
	}
}
