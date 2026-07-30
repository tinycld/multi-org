package manifesteval

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"testing"
)

// The child is this test binary re-exec'd (the standard helper-process
// pattern), so the tests exercise the real spawn ⇄ stdio ⇄ rlimit path
// without building serve-multi.
func TestMain(m *testing.M) {
	if os.Getenv("MANIFEST_EVAL_HELPER") == "1" {
		os.Exit(ServeStdio())
	}
	os.Exit(m.Run())
}

func helperArgv() []string {
	return []string{os.Args[0], "-test.run=TestMain"}
}

func evalViaSubprocess(t *testing.T, src, name string) ([]byte, error) {
	t.Helper()
	t.Setenv("MANIFEST_EVAL_HELPER", "1")
	return EvalSubprocess(context.Background(), helperArgv(), src, name)
}

func TestEvalSubprocess_RoundTripsAManifest(t *testing.T) {
	data, err := evalViaSubprocess(t, `
const manifest = { name: 'Demo', slug: 'demo', version: '1.0.0' }
export default manifest
`, "manifest.ts")
	if err != nil {
		t.Fatalf("EvalSubprocess: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("child produced non-JSON %q: %v", data, err)
	}
	if got["slug"] != "demo" || got["version"] != "1.0.0" {
		t.Fatalf("manifest = %v", got)
	}
}

func TestEvalSubprocess_EvalErrorNamesTheManifest(t *testing.T) {
	_, err := evalViaSubprocess(t, `throw new Error('boom')`, "manifest.ts")
	if err == nil {
		t.Fatal("a throwing manifest evaluated without error")
	}
	if !strings.Contains(err.Error(), "manifest.ts") {
		t.Fatalf("err = %v, want the manifest named", err)
	}
}

// M7's residual, closed: an allocation bomb must kill the CHILD and come
// back as an error — before subprocess isolation it OOMed the router and
// every tenant it fronts. (The in-process red equivalent of this test would
// OOM the test runner, which is exactly the point.) Linux-only: RLIMIT_AS is
// not reliably enforced elsewhere, and production is Linux.
func TestEvalSubprocess_AllocationBombKillsOnlyTheChild(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("RLIMIT_AS enforcement is linux-only; see rlimit_other.go")
	}

	// Doubles a 16-byte string 28 times ≈ 4 GiB — past the child's 2 GiB
	// ceiling, far under what the eval interrupt alone would stop in time.
	_, err := evalViaSubprocess(t, `
let s = 'xxxxxxxxxxxxxxxx'
for (let i = 0; i < 28; i++) s += s
export default { blob: s }
`, "manifest.ts")
	if err == nil {
		t.Fatal("the allocation bomb returned a result — nothing limited the child")
	}
}
