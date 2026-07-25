package controlplane

import (
	"encoding/json"
	"fmt"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/grafana/sobek"
)

// manifestJSONFile is the store filename holding a package's parsed manifest.
// Emitted at publish time from manifest.ts so the host (orgmanager) can read a
// package's host-side capability config (carddav/fts/audit blocks) without a TS
// loader.
const manifestJSONFile = "manifest.json"

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

	obj, err := evalManifest(string(src), name)
	if err != nil {
		return nil, fmt.Errorf("evaluate %s: %w", name, err)
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshal manifest json: %w", err)
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
