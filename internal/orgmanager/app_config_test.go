package orgmanager

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The org's public URL must reach the tenant through .runtime/, host-resolved —
// the child knows neither MT_BASE_DOMAIN nor the TLS mode, and without it every
// auth email interpolates PocketBase's default http://localhost:8090 into its
// verification / password-reset / email-change links.
func TestWriteAppConfigWritesOrgURL(t *testing.T) {
	orgDir := t.TempDir()
	m := &OrgManager{cfg: Config{
		OrgURL: func(slug string) string { return "https://" + slug + ".tenants.example.test" },
	}}

	path, err := m.writeAppConfig(orgDir, "acme")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(orgDir, ".runtime", "app.json")
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg appConfigFile
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.AppURL != "https://acme.tenants.example.test" {
		t.Fatalf("appURL = %q", cfg.AppURL)
	}
}

// A host with no OrgURL hook (tests, exotic wiring) materializes nothing and
// the tenant keeps its stored settings untouched.
func TestWriteAppConfigSkipsWhenUnwired(t *testing.T) {
	orgDir := t.TempDir()
	m := &OrgManager{cfg: Config{}}

	path, err := m.writeAppConfig(orgDir, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if path != "" {
		t.Fatalf("path = %q, want empty for an unwired host", path)
	}
	if _, err := os.Stat(filepath.Join(orgDir, ".runtime", "app.json")); !os.IsNotExist(err) {
		t.Fatalf("no app.json should be written when OrgURL is unwired, stat err = %v", err)
	}
}
