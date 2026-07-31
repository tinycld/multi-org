// Package recipeparity pins this router checkout to a tinycld.org/core whose
// recipe-hash canonicalization matches the one the router's build cache will
// key on (DESIGN-org-package-agency D4/D5: RecipeHash has exactly one
// definition, in core's pkgbuild, and both hosts must compute identical keys
// for identical inputs).
//
// PAIRING RULE (same contract as the transpile goldens shared with the
// PocketBase fork): the fixture + golden constant below are duplicated
// byte-for-byte from tinycld/core/server/pkgbuild/recipehash_test.go. If the
// canonical form changes (recipeFormatVersion bump), core's golden goes red
// there and this stale twin goes red here — fix by changing BOTH sides and
// regenerating BOTH goldens together, never one alone.
package recipeparity

import (
	"testing"

	"tinycld.org/core/pkgbuild"
)

// goldenRecipeFixture is the frozen cross-repo fixture: three members
// including a third-party one (undistinguished by design), two overrides plus
// the "//"-style doc-key-free map shape, and a fixed toolchain.
func goldenRecipeFixture() ([]pkgbuild.ResolvedMember, map[string]string, pkgbuild.Toolchain) {
	members := []pkgbuild.ResolvedMember{
		{Slug: "tinycld", Name: "@tinycld/core", Version: "0.0.9", Integrity: "sha256:ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34", FromCurrent: true},
		{Slug: "mail", Name: "@tinycld/mail", Version: "0.3.1", Integrity: "sha256:cd34ef56cd34ef56cd34ef56cd34ef56cd34ef56cd34ef56cd34ef56cd34ef56"},
		{Slug: "todo", Name: "some-third-party-todo", Version: "2.1.0", Integrity: "sha256:ef56ab78ef56ab78ef56ab78ef56ab78ef56ab78ef56ab78ef56ab78ef56ab78"},
	}
	overrides := map[string]string{
		"uniwind":              "1.8.0",
		"@sentry/react-native": "7.11.0",
	}
	tc := pkgbuild.Toolchain{Go: "go1.26.3", Node: "v22.12.0", Pnpm: "pnpm@11.3.0+sha512.f00dfeedf00dfeed"}
	return members, overrides, tc
}

const goldenRecipeHash = "sha256:1e01f41d23eb4ffc9bd6c590bc7497b0d3ece1ad1a08df21e1f3d4628f361329"

func TestRecipeHash_MatchesCoreGolden(t *testing.T) {
	members, overrides, tc := goldenRecipeFixture()
	got, err := pkgbuild.RecipeHash(members, overrides, tc)
	if err != nil {
		t.Fatal(err)
	}
	if got != goldenRecipeHash {
		t.Fatalf("recipe hash disagrees with core's golden:\n  got  %s\n  want %s\nThe linked tinycld.org/core computes a different canonical form than this checkout was built against — regenerate BOTH goldens (core pkgbuild/recipehash_test.go and this file) together.", got, goldenRecipeHash)
	}
}
