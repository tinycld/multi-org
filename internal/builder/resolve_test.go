package builder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

func TestResolve_RefusesSetWithoutBase(t *testing.T) {
	fixtures := map[string]fixture{
		"@tinycld/mail@0.3.1": defaultFixtures["@tinycld/mail@0.3.1"],
	}
	b := testBuilder(t, fixtures, &fakeRunner{})
	_, err := b.Build(context.Background(), []PackageRef{{Spec: "@tinycld/mail@0.3.1"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "no base") {
		t.Fatalf("err = %v, want no-base refusal", err)
	}
}

func TestResolve_RefusesEmptySet(t *testing.T) {
	b := testBuilder(t, nil, &fakeRunner{})
	if _, err := b.Build(context.Background(), nil, nil); err == nil {
		t.Fatal("empty set resolved")
	}
}

func TestResolve_RefusesDuplicateSlugs(t *testing.T) {
	fixtures := map[string]fixture{
		"tinycld@0.4.0":       defaultFixtures["tinycld@0.4.0"],
		"@tinycld/mail@0.3.1": defaultFixtures["@tinycld/mail@0.3.1"],
		"@evil/mail@1.0.0":    {slug: "mail", name: "@evil/mail", version: "1.0.0"},
	}
	b := testBuilder(t, fixtures, &fakeRunner{})
	_, err := b.Build(context.Background(), []PackageRef{
		{Spec: "tinycld@0.4.0"}, {Spec: "@tinycld/mail@0.3.1"}, {Spec: "@evil/mail@1.0.0"},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "mail") {
		t.Fatalf("err = %v, want duplicate-slug refusal", err)
	}
}

func TestResolve_RefusesFeatureClaimingBaseSlug(t *testing.T) {
	fixtures := map[string]fixture{
		"tinycld@0.4.0":    defaultFixtures["tinycld@0.4.0"],
		"@evil/base@1.0.0": {slug: "tinycld", name: "@evil/base", version: "1.0.0"},
	}
	b := testBuilder(t, fixtures, &fakeRunner{})
	_, err := b.Build(context.Background(), []PackageRef{
		{Spec: "tinycld@0.4.0"}, {Spec: "@evil/base@1.0.0"},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "reserved base slug") {
		t.Fatalf("err = %v, want reserved-base-slug refusal", err)
	}
}

func TestResolve_RefusesNonTinycldPackage(t *testing.T) {
	b := testBuilder(t, nil, &fakeRunner{})
	// A package with neither a manifest nor a nested core: plain npm package.
	b.cfg.pack = func(spec, workDir string) (string, string, error) {
		dir := workDir + "/package"
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", "", err
		}
		if err := os.WriteFile(dir+"/index.js", []byte("module.exports = {}"), 0o644); err != nil {
			return "", "", err
		}
		tgz := workDir + "/fake.tgz"
		if err := os.WriteFile(tgz, []byte("bytes"), 0o644); err != nil {
			return "", "", err
		}
		return dir, tgz, nil
	}
	_, err := b.Build(context.Background(), []PackageRef{{Spec: "leftpad@1.0.0"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "not a tinycld workspace member") {
		t.Fatalf("err = %v, want not-a-member refusal", err)
	}
}

func TestResolve_RefusesMalformedSpec(t *testing.T) {
	b := testBuilder(t, nil, &fakeRunner{})
	for _, spec := range []string{"", "-rf", "pkg; rm -rf /"} {
		if _, err := b.Build(context.Background(), []PackageRef{{Spec: spec}}, nil); err == nil {
			t.Errorf("spec %q resolved", spec)
		}
	}
}

func TestResolve_IntegrityIsParentComputedFromTarballBytes(t *testing.T) {
	runner := &fakeRunner{}
	b := testBuilder(t, defaultFixtures, runner)
	res, err := b.Build(context.Background(), defaultRefs, nil)
	if err != nil {
		t.Fatal(err)
	}
	// fakePack writes "tarball:<spec>" as the tarball bytes; the recorded
	// integrity must be OUR sha256 of exactly those bytes — the seam a pack
	// implementation cannot influence.
	sum := sha256.Sum256([]byte("tarball:@tinycld/mail@0.3.1"))
	want := "sha256:" + hex.EncodeToString(sum[:])
	for _, m := range res.Members {
		if m.Slug == "mail" && m.Integrity != want {
			t.Fatalf("mail integrity = %q, want %q", m.Integrity, want)
		}
	}
}

func TestResolve_CleansUpFetchDirAfterBuild(t *testing.T) {
	b := testBuilder(t, defaultFixtures, &fakeRunner{})
	if _, err := b.Build(context.Background(), defaultRefs, nil); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(b.workDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "fetch-") {
			t.Fatalf("fetch dir %s survived the build", e.Name())
		}
	}
}
