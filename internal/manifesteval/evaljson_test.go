package manifesteval

import (
	"testing"
	"time"
)

// A manifest is untrusted package JS. Without an interrupt, `while(true){}`
// spins the calling goroutine forever and nothing can recover it — the "pure
// object literal" the evaluator expects is an assumption about input, not an
// enforced property. The builder's trusted-side spec resolution runs this
// evaluator on every fetched manifest.
func TestEvalJSON_InterruptsRunawayScript(t *testing.T) {
	orig := Timeout
	Timeout = 200 * time.Millisecond
	t.Cleanup(func() { Timeout = orig })

	errCh := make(chan error, 1)
	go func() {
		_, err := EvalJSON("const manifest = {}\nwhile (true) {}\nexport default manifest", "manifest.ts")
		errCh <- err
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected a timeout error for a runaway manifest")
		}
	case <-time.After(Timeout + 5*time.Second):
		t.Fatal("EvalJSON never returned for a runaway script")
	}
}
