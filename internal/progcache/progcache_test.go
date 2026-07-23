package progcache

import (
	"sync"
	"testing"

	"github.com/grafana/sobek"
)

func TestSharedProgramCache_CompilesOncePerSource(t *testing.T) {
	c := New()
	p1, err := c.Compile("pb.js", "1 + 1", true)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := c.Compile("pb.js", "1 + 1", true)
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 {
		t.Fatal("expected identical *Program pointer for identical source")
	}
	if c.Len() != 1 {
		t.Fatalf("expected 1 cached program, got %d", c.Len())
	}
}

func TestSharedProgramCache_DistinctSourcesDistinctPrograms(t *testing.T) {
	c := New()
	p1, _ := c.Compile("pb.js", "1 + 1", true)
	p2, _ := c.Compile("pb.js", "2 + 2", true)
	if p1 == p2 {
		t.Fatal("expected different programs for different source")
	}
	if c.Len() != 2 {
		t.Fatalf("expected 2 cached programs, got %d", c.Len())
	}
}

func TestSharedProgramCache_ConcurrentCompileIsSafe(t *testing.T) {
	c := New()
	// Under concurrent first-compile of the same source, multiple goroutines may
	// each call sobek.Compile; the double-checked lock ensures only ONE program is
	// stored and all callers observe it (losers discard their own). This asserts
	// the retained-program count is 1, not that compile ran exactly once.
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Compile("pb.js", "40 + 2", true); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if c.Len() != 1 {
		t.Fatalf("expected 1 program after concurrent identical compiles, got %d", c.Len())
	}
}

func TestSharedProgramCache_ProgramRunsOnFreshRuntime(t *testing.T) {
	c := New()
	prog, _ := c.Compile("pb.js", "40 + 2", true)
	vm := sobek.New()
	v, err := vm.RunProgram(prog)
	if err != nil {
		t.Fatal(err)
	}
	if v.ToInteger() != 42 {
		t.Fatalf("expected 42, got %v", v.Export())
	}
}

func TestSharedProgramCache_StrictModeKeyedSeparately(t *testing.T) {
	c := New()
	// Same source, different strictness -> distinct cache entries. An octal
	// literal is legal in sloppy mode but a SyntaxError in strict mode, proving
	// the two modes are compiled (and keyed) separately and don't collide.
	sloppy, err := c.Compile("pb.js", "var x = 0777", false)
	if err != nil {
		t.Fatalf("sloppy compile should succeed: %v", err)
	}
	if sloppy == nil {
		t.Fatal("expected a sloppy program")
	}
	if _, err := c.Compile("pb.js", "var x = 0777", true); err == nil {
		t.Fatal("strict compile of an octal literal should fail")
	}
	// The strict failure must not have poisoned or replaced the sloppy entry.
	if c.Len() != 1 {
		t.Fatalf("expected only the sloppy program cached, got %d", c.Len())
	}
}
