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
	if len(cfg.TrustedProxyHeaders) != 1 || cfg.TrustedProxyHeaders[0] != "X-Forwarded-For" {
		t.Fatalf("trustedProxyHeaders = %v, want [X-Forwarded-For]", cfg.TrustedProxyHeaders)
	}
}

// A host with no OrgURL hook (tests, exotic wiring) still materializes the
// proxy-trust config: every router-managed tenant is reached exclusively
// through the router's socket, where RemoteAddr is empty — without trusting
// X-Forwarded-For, per-IP rate limiting collapses into a single bucket.
func TestWriteAppConfigWithoutOrgURLStillMaterializesProxyTrust(t *testing.T) {
	orgDir := t.TempDir()
	m := &OrgManager{cfg: Config{}}

	path, err := m.writeAppConfig(orgDir, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if path == "" {
		t.Fatal("want app.json written even with no OrgURL hook (it carries the proxy trust config)")
	}

	body, err := os.ReadFile(filepath.Join(orgDir, ".runtime", "app.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg appConfigFile
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.AppURL != "" {
		t.Fatalf("appURL = %q, want empty when OrgURL is unwired", cfg.AppURL)
	}
	if len(cfg.TrustedProxyHeaders) != 1 || cfg.TrustedProxyHeaders[0] != "X-Forwarded-For" {
		t.Fatalf("trustedProxyHeaders = %v, want [X-Forwarded-For]", cfg.TrustedProxyHeaders)
	}
}
