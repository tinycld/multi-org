package orgmanager

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"tinycld.org/multi-org/internal/lockfile"
)

// The resolved slugs must reach the tenant through .runtime/, host-resolved —
// the child gates feature Go on this list and must not walk the store itself.
func TestWritePackagesConfigWritesResolvedSlugs(t *testing.T) {
	orgDir := t.TempDir()
	m := &OrgManager{cfg: Config{
		PackageSlugs: func(_ []lockfile.ResolvedPackage) ([]string, error) {
			return []string{"contacts", "mail"}, nil
		},
	}}

	path, err := m.writePackagesConfig(orgDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(orgDir, ".runtime", "packages.json")
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg packagesConfigFile
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Slugs, []string{"contacts", "mail"}) {
		t.Fatalf("slugs = %v", cfg.Slugs)
	}
}

// An empty package set must still write the file: "no packages → no feature
// Go" and "config missing" must not be distinguishable to the tenant.
func TestWritePackagesConfigWritesEmptySet(t *testing.T) {
	orgDir := t.TempDir()
	m := &OrgManager{cfg: Config{
		PackageSlugs: func(_ []lockfile.ResolvedPackage) ([]string, error) { return nil, nil },
	}}

	path, err := m.writePackagesConfig(orgDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("an org with no packages must still get a config file: %v", err)
	}
}

// No hook wired → no file, empty flag: the child then registers no feature Go.
func TestWritePackagesConfigSkippedWithoutHook(t *testing.T) {
	m := &OrgManager{cfg: Config{}}
	path, err := m.writePackagesConfig(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if path != "" {
		t.Fatalf("path = %q, want empty when no PackageSlugs hook is wired", path)
	}
}
