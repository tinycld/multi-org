package controlplane

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/grafana/sobek"
)

// manifestEvalTimeout bounds evalManifest's VM run. A manifest is expected to
// be a pure object literal, but that is an assumption about untrusted package
// JS, not an enforced property — without an interrupt, `while(true){}` hangs
// the publish handler's goroutine unrecoverably. Generous: a genuine manifest
// evaluates in microseconds. A var only so the runaway-script test doesn't
// spin a core for the full production window.
var manifestEvalTimeout = 5 * time.Second

// manifestJSONFile is the store filename holding a package's parsed manifest.
// Emitted at publish time from manifest.ts so the host (orgmanager) can read a
// package's host-side capability config (carddav/fts/audit blocks) without a TS
// loader.
const manifestJSONFile = "manifest.json"

// Size caps on the eval. The interrupt above bounds time, not memory: a
// doubling-string bomb allocates gigabytes in microseconds, and the router
// hosts every tenant, so an OOM here takes them all down. The caps refuse an
// oversized source before eval and an oversized export before it is stored.
// A hostile script can still allocate transiently inside the interrupt window
// (full isolation would need a subprocess with rlimits); publish is
// superuser-only, so these bound the accident rather than a live attack.
const (
	// manifestSourceMaxBytes is generous: the largest real manifest is a few
	// KB of object literal.
	manifestSourceMaxBytes = 1 << 20 // 1 MiB

	// manifestJSONMaxBytes caps the marshalled manifest.json — the value every
	// org load re-reads from the store.
	manifestJSONMaxBytes = 4 << 20 // 4 MiB
)

// emitManifestJSON adds a parsed manifest.json to the file map when the package
// ships a manifest.ts (or .js). It transpiles the manifest to CommonJS via
// esbuild, evaluates it in a throwaway sobek VM to capture the default export,
// and marshals that object to JSON. A package without a manifest source is left
// unchanged (no manifest.json emitted). Evaluation runs a pure object literal —
// the manifest has no runtime imports — so the VM needs no host bindings.
func emitManifestJSON(files map[string][]byte) (map[string][]byte, error) {
	src, name := manifestSource(files)
	if src == nil {
		return files, nil
	}
	if len(src) > manifestSourceMaxBytes {
		return nil, fmt.Errorf("%s is %d bytes (max %d): a manifest is a small object literal", name, len(src), manifestSourceMaxBytes)
	}

	obj, err := evalManifest(string(src), name)
	if err != nil {
		return nil, fmt.Errorf("evaluate %s: %w", name, err)
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshal manifest json: %w", err)
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

// evalManifest transpiles a manifest module to CommonJS and evaluates it in a
// fresh sobek VM, returning the default export as a plain Go value (maps/slices/
// scalars) suitable for json.Marshal.
func evalManifest(src, name string) (any, error) {
	res := api.Transform(src, api.TransformOptions{
		Loader: api.LoaderTS,
		Target: api.ES2020,
		Format: api.FormatCommonJS,
	})
	if len(res.Errors) > 0 {
		return nil, fmt.Errorf("transpile %s: %s", name, res.Errors[0].Text)
	}

	vm := sobek.New()
	module := vm.NewObject()
	exports := vm.NewObject()
	if err := module.Set("exports", exports); err != nil {
		return nil, err
	}
	if err := vm.Set("module", module); err != nil {
		return nil, err
	}
	if err := vm.Set("exports", exports); err != nil {
		return nil, err
	}

	interrupt := time.AfterFunc(manifestEvalTimeout, func() {
		vm.Interrupt(fmt.Sprintf("manifest evaluation exceeded %s", manifestEvalTimeout))
	})
	defer interrupt.Stop()

	if _, err := vm.RunString(string(res.Code)); err != nil {
		return nil, err
	}

	// CommonJS default export lands on module.exports.default (esbuild) or, for a
	// bare `module.exports =`, on module.exports itself.
	me := module.Get("exports")
	if me == nil {
		return nil, fmt.Errorf("manifest produced no exports")
	}
	meObj := me.ToObject(vm)
	if def := meObj.Get("default"); def != nil && !sobek.IsUndefined(def) {
		return def.Export(), nil
	}
	return me.Export(), nil
}
