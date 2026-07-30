package controlplane

import (
	"fmt"

	"tinycld.org/multi-org/internal/manifesteval"
)

// manifestEvaluator turns manifest source into marshalled JSON. The default
// evaluates in-process (manifesteval.EvalJSON — interrupt-bounded, size caps
// applied by the caller); serve-multi swaps in the SUBPROCESS evaluator
// (SetManifestEvaluator) so an allocation bomb dies in a rlimited child
// instead of OOMing the router and every tenant it fronts.
var manifestEvaluator = manifesteval.EvalJSON

// SetManifestEvaluator replaces how manifest.ts is evaluated to JSON —
// serve-multi passes its subprocess evaluator here at boot.
func SetManifestEvaluator(fn func(src, name string) ([]byte, error)) {
	manifestEvaluator = fn
}

// manifestJSONFile is the store filename holding a package's parsed manifest.
// Emitted at publish time from manifest.ts so the host (orgmanager) can read a
// package's host-side capability config (carddav/fts/audit blocks) without a TS
// loader.
const manifestJSONFile = "manifest.json"

// Size caps on the eval, applied regardless of which evaluator runs: an
// oversized source is refused before eval and an oversized export before it
// is stored. Together with the evaluator's interrupt (time) and serve-multi's
// subprocess rlimit (memory), publish-time manifest JS is fully bounded.
const (
	// manifestSourceMaxBytes is generous: the largest real manifest is a few
	// KB of object literal.
	manifestSourceMaxBytes = 1 << 20 // 1 MiB

	// manifestJSONMaxBytes caps the marshalled manifest.json — the value every
	// org load re-reads from the store.
	manifestJSONMaxBytes = 4 << 20 // 4 MiB
)

// emitManifestJSON adds a parsed manifest.json to the file map when the package
// ships a manifest.ts (or .js), via the configured evaluator (see
// manifestEvaluator). A package without a manifest source is left unchanged
// (no manifest.json emitted).
func emitManifestJSON(files map[string][]byte) (map[string][]byte, error) {
	src, name := manifestSource(files)
	if src == nil {
		return files, nil
	}
	if len(src) > manifestSourceMaxBytes {
		return nil, fmt.Errorf("%s is %d bytes (max %d): a manifest is a small object literal", name, len(src), manifestSourceMaxBytes)
	}

	data, err := manifestEvaluator(string(src), name)
	if err != nil {
		return nil, err
	}
	if len(data) > manifestJSONMaxBytes {
		return nil, fmt.Errorf("%s evaluates to %d bytes of JSON (max %d)", name, len(data), manifestJSONMaxBytes)
	}

	out := make(map[string][]byte, len(files)+1)
	for k, v := range files {
		out[k] = v
	}
	out[manifestJSONFile] = data
	return out, nil
}

// manifestSource returns the manifest source bytes and its filename, preferring
// manifest.ts then manifest.js. Returns nil when neither is present.
func manifestSource(files map[string][]byte) ([]byte, string) {
	for _, name := range []string{"manifest.ts", "manifest.js"} {
		if b, ok := files[name]; ok {
			return b, name
		}
	}
	return nil, ""
}
