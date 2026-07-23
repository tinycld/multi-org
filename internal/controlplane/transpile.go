package controlplane

import (
	"fmt"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
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
		if !strings.HasSuffix(name, ".ts") {
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
		out[strings.TrimSuffix(name, ".ts")+".js"] = res.Code
	}
	return out, nil
}
