package controlplane

import "testing"

// The esbuild call here and the fork's load-time transformSource
// (tinycld/third_party/pocketbase/plugins/jsvm/transform.go) are duplicated
// across repos and MUST stay behaviorally identical — a package transpiled at
// publish must run exactly as it would if the tenant transpiled it at load.
// This is the golden test HANDOFF §5.6 promises (R6): the fixture source and
// expected output below are byte-identical to the fork's
// plugins/jsvm/transform_golden_test.go. If either side changes its esbuild
// options or bumps esbuild alone, its own copy of this test goes red; fix by
// changing BOTH sides and regenerating BOTH goldens together.

const goldenTSFixture = `interface Hook {
    name: string
}
const h: Hook = { name: 'golden' }
export const picked = h?.name ?? 'fallback'
`

const goldenJSOutput = "const h = { name: \"golden\" };\nexport const picked = h?.name ?? \"fallback\";\n//# sourceMappingURL=data:application/json;base64,ewogICJ2ZXJzaW9uIjogMywKICAic291cmNlcyI6IFsiPHN0ZGluPiJdLAogICJzb3VyY2VzQ29udGVudCI6IFsiaW50ZXJmYWNlIEhvb2sge1xuICAgIG5hbWU6IHN0cmluZ1xufVxuY29uc3QgaDogSG9vayA9IHsgbmFtZTogJ2dvbGRlbicgfVxuZXhwb3J0IGNvbnN0IHBpY2tlZCA9IGg/Lm5hbWUgPz8gJ2ZhbGxiYWNrJ1xuIl0sCiAgIm1hcHBpbmdzIjogIkFBR0EsTUFBTSxJQUFVLEVBQUUsTUFBTSxTQUFTO0FBQzFCLGFBQU0sU0FBUyxHQUFHLFFBQVE7IiwKICAibmFtZXMiOiBbXQp9Cg==\n"

func TestTranspileForStore_MatchesForkGolden(t *testing.T) {
	out, err := transpileForStore(map[string][]byte{
		"fixture.pb.ts": []byte(goldenTSFixture),
	})
	if err != nil {
		t.Fatalf("transpileForStore: %v", err)
	}
	got, ok := out["fixture.pb.js"]
	if !ok {
		t.Fatalf("expected fixture.pb.ts to be rewritten to fixture.pb.js; got keys %v", keysOf(out))
	}
	if string(got) != goldenJSOutput {
		t.Fatalf("store-side transpile diverged from the shared golden (did esbuild options or version change on one side only?)\n got: %q\nwant: %q", got, goldenJSOutput)
	}
}

func keysOf(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
