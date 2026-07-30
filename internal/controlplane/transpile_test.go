package controlplane

import (
	"strings"
	"testing"
)

func TestTranspileForStore_RewritesTSKeysAndContent(t *testing.T) {
	in := map[string][]byte{
		"server/main.pb.ts":         []byte("const x: number = 1\nrouterAdd('GET','/x',()=>{})"),
		"pb-migrations/001_init.ts": []byte("migrate((app)=>{},(app)=>{})"),
		"server/util.pb.js":         []byte("const y = 2"),
		"client/dist/index.html":    []byte("<html></html>"),
	}
	out, err := transpileForStore(in)
	if err != nil {
		t.Fatal(err)
	}
	// .pb.ts key rewritten to .pb.js, content transpiled (no type annotation).
	js, ok := out["server/main.pb.js"]
	if !ok {
		t.Fatalf("expected server/main.pb.js key; got keys %v", keys(out))
	}
	if strings.Contains(string(js), ": number") {
		t.Fatalf("type annotation not stripped: %s", js)
	}
	if _, stillTS := out["server/main.pb.ts"]; stillTS {
		t.Fatal("original .pb.ts key must be gone")
	}
	// .ts migration key rewritten to .js.
	if _, ok := out["pb-migrations/001_init.js"]; !ok {
		t.Fatalf("expected pb-migrations/001_init.js; got %v", keys(out))
	}
	// .js and client assets untouched, byte-identical.
	if string(out["server/util.pb.js"]) != "const y = 2" {
		t.Fatal(".js must pass through byte-identical")
	}
	if string(out["client/dist/index.html"]) != "<html></html>" {
		t.Fatal("client asset must pass through byte-identical")
	}
}

func TestTranspileForStore_DTSPassesThrough(t *testing.T) {
	in := map[string][]byte{
		"server/types.d.ts": []byte("export interface X { a: number }"),
	}
	out, err := transpileForStore(in)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out["server/types.d.ts"]; !ok {
		t.Fatalf(".d.ts must pass through unchanged (key kept); got keys %v", keys(out))
	}
	if _, bad := out["server/types.d.js"]; bad {
		t.Fatal(".d.ts must NOT be rewritten to .d.js")
	}
}

func TestTranspileForStore_OutputIsStableES2020(t *testing.T) {
	// Golden-ish: pins that TS is stripped and ES2020 features are preserved
	// (not down-leveled). Guards against esbuild-option drift vs the fork's
	// transformSource, keeping publish-time and load-time transpile interchangeable.
	in := map[string][]byte{
		"m.pb.ts": []byte("const f = (x?: number): number => x ?? 0\nrouterAdd('GET','/x',()=>{})"),
	}
	out, err := transpileForStore(in)
	if err != nil {
		t.Fatal(err)
	}
	js := string(out["m.pb.js"])
	if strings.Contains(js, ": number") {
		t.Fatalf("type annotations must be stripped: %s", js)
	}
	if !strings.Contains(js, "??") {
		t.Fatalf("ES2020 nullish-coalescing must be preserved (target ES2020, not down-leveled): %s", js)
	}
	if !strings.Contains(js, "routerAdd") {
		t.Fatalf("call must be preserved: %s", js)
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A hook file using import/export must be refused AT PUBLISH with an error
// naming the file and the fix. Hook files run as plain scripts in the tenant
// (the fork's jsvm rejects module syntax at load with the same message);
// letting the store accept one would defer the failure to every tenant's
// boot instead of the publisher's terminal.
func TestTranspileForStore_RejectsModuleSyntax(t *testing.T) {
	_, err := transpileForStore(map[string][]byte{
		"pb-hooks/exports.pb.ts": []byte("export const answer = 42\n"),
	})
	if err == nil {
		t.Fatal("a module-syntax hook file was accepted into the store")
	}
	if !strings.Contains(err.Error(), "exports.pb.ts") {
		t.Fatalf("error %q does not name the author's file", err)
	}
	if !strings.Contains(err.Error(), "import/export") {
		t.Fatalf("error %q does not say what to change", err)
	}
}

// A string that merely contains export-looking text is not module syntax.
func TestTranspileForStore_ExportInsideStringAccepted(t *testing.T) {
	_, err := transpileForStore(map[string][]byte{
		"pb-hooks/strings.pb.ts": []byte("const doc = `\nexport const x = 1\n`\nconsole.log(doc)\n"),
	})
	if err != nil {
		t.Fatalf("a script whose string mentions export was refused: %v", err)
	}
}
