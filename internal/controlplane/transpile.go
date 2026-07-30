package controlplane

import (
	"fmt"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/grafana/sobek"
)

// transpileForStore converts TypeScript entries in a package's file map to JS
// before it is written to the immutable store, so production tenants materialize
// only .js (the fork's load-time transpile seam then no-ops). It rewrites the
// map KEY (foo.pb.ts -> foo.pb.js, foo.ts -> foo.js) and the content. Non-.ts
// files (.js, client assets) pass through byte-for-byte. Uses the same esbuild
// loader/target as the fork so behavior matches whether transpile happens here
// or at load.
func transpileForStore(files map[string][]byte) (map[string][]byte, error) {
	out := make(map[string][]byte, len(files))
	for name, content := range files {
		// .d.ts files are type declarations with no runtime — pass them through
		// unchanged (like .js), rather than letting esbuild erase them to empty JS
		// and rewriting the key to .d.js.
		if !strings.HasSuffix(name, ".ts") || strings.HasSuffix(name, ".d.ts") {
			out[name] = content
			continue
		}
		res := api.Transform(string(content), api.TransformOptions{
			Loader:    api.LoaderTS,
			Target:    api.ES2020,
			Sourcemap: api.SourceMapInline,
		})
		if len(res.Errors) > 0 {
			msgs := make([]string, 0, len(res.Errors))
			for _, e := range res.Errors {
				msgs = append(msgs, e.Text)
			}
			return nil, fmt.Errorf("transpile %s: %s", name, strings.Join(msgs, "; "))
		}
		if isScriptExecuted(name) {
			if err := rejectModuleSyntax(name, res.Code); err != nil {
				return nil, err
			}
		}
		out[strings.TrimSuffix(name, ".ts")+".js"] = res.Code
	}
	return out, nil
}

// isScriptExecuted reports whether the tenant runs this file as a plain
// script (RunScript / script-mode compile) — the only files module syntax
// breaks. manifest.ts is deliberately NOT one: it is evaluated as a
// CommonJS module (evalManifest) and legitimately uses `export default`.
func isScriptExecuted(name string) bool {
	return strings.HasPrefix(name, "pb-hooks/") || strings.HasPrefix(name, "pb-migrations/")
}

// rejectModuleSyntax refuses transpiled output that still carries ES-module
// syntax, with an error naming the author's file and the fix. Mirrors the
// fork's jsvm transformSource check (the two transpiles must stay
// behaviorally identical — see the shared golden test): hook files run as
// plain scripts in the tenant, so accepting a module-syntax file into the
// store would defer the failure to every tenant's boot instead of the
// publisher's terminal. A script-mode parse is the authoritative test — a
// string merely containing export-looking text parses fine.
func rejectModuleSyntax(name string, code []byte) error {
	_, err := sobek.Compile(name, string(code), false)
	if err != nil && strings.Contains(err.Error(), "not supported in script") {
		return fmt.Errorf("%s uses import/export: hook and migration files run as plain "+
			"scripts, not ES modules — remove the top-level import/export statements "+
			"(the PocketBase APIs are provided as globals)", name)
	}
	// Any other parse failure is left for the tenant's real compile, whose
	// error already names the file.
	return nil
}
