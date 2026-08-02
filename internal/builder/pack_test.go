package builder

import "testing"

// isRegistrySpec decides whether `npm pack` may run the package's own
// lifecycle scripts. Registry specs download a prebuilt tarball and execute
// nothing; git and directory specs build from source and would otherwise run
// prepare/prepack as root (resolve() runs in the router process). Getting this
// classification wrong is a root-RCE, so it is pinned here rather than left to
// the regexes it delegates to.
func TestIsRegistrySpec(t *testing.T) {
	for _, tc := range []struct {
		spec string
		want bool
	}{
		// Registry forms — safe to let npm run scripts (it runs none).
		{"tinycld", true},
		{"tinycld@0.4.0", true},
		{"@tinycld/mail", true},
		{"@tinycld/mail@1.2.3", true},

		// Git forms — npm clones and builds, so scripts must be suppressed.
		{"github:tinycld/tinycld", false},
		{"github:tinycld/tinycld#main", false},
		{"gitlab:owner/repo", false},
		{"bitbucket:owner/repo", false},
		{"tinycld/tinycld", false},
		{"tinycld/tinycld#v0.4.0", false},
		{"git+ssh://git@github.com/tinycld/tinycld.git", false},
		{"git+https://github.com/tinycld/tinycld.git#main", false},
		{"git+file:///srv/tinycld-mt/tinycld", false},

		// Local directory (AllowDirSpecs) — also builds from source.
		{"/srv/tinycld-mt/tinycld", false},
	} {
		if got := isRegistrySpec(tc.spec); got != tc.want {
			t.Errorf("isRegistrySpec(%q) = %v, want %v", tc.spec, got, tc.want)
		}
	}
}

// The production packFn must pass --ignore-scripts for every non-registry
// spec, and must not for registry specs (where it would be pointless noise).
// npmPackWith builds the args, so assert on them directly rather than shelling
// out to npm.
func TestNpmPackWith_IgnoreScriptsForNonRegistrySpecs(t *testing.T) {
	for _, tc := range []struct {
		spec           string
		registry       string
		wantIgnore     bool
		wantRegistryIn bool
	}{
		{spec: "tinycld@0.4.0", wantIgnore: false},
		{spec: "tinycld@0.4.0", registry: "http://localhost:4873", wantIgnore: false, wantRegistryIn: true},
		{spec: "github:tinycld/tinycld#main", wantIgnore: true},
		{spec: "git+ssh://git@github.com/tinycld/tinycld.git", wantIgnore: true},
		{spec: "/srv/tinycld-mt/tinycld", wantIgnore: true},
	} {
		args := packArgs(tc.spec, tc.registry)
		if got := containsArg(args, "--ignore-scripts"); got != tc.wantIgnore {
			t.Errorf("packArgs(%q) --ignore-scripts = %v, want %v (args=%v)", tc.spec, got, tc.wantIgnore, args)
		}
		if got := containsArg(args, "--registry"); got != tc.wantRegistryIn {
			t.Errorf("packArgs(%q, registry=%q) --registry = %v, want %v", tc.spec, tc.registry, got, tc.wantRegistryIn)
		}
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
