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

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
