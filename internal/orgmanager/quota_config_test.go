package orgmanager

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The org ceiling must arrive from the control-plane record, not the tenant's
// own settings — each tenant has its own superusers, so a limit stored inside
// the org could be raised by the org.
func TestWriteQuotaConfigCarriesTheOperatorsLimit(t *testing.T) {
	orgDir := t.TempDir()
	m := &OrgManager{cfg: Config{}}

	path, err := m.writeQuotaConfig(orgDir, OrgRecord{
		Slug:              "foo",
		StorageLimitBytes: 50 << 30, // 50 GB
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// It lands in .runtime/, which the router owns — not anywhere the tenant's
	// own DB or settings can reach.
	want := filepath.Join(orgDir, ".runtime", "quota.json")
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg quotaConfigFile
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.StorageLimitBytes != 50<<30 {
		t.Fatalf("limit = %d, want %d", cfg.StorageLimitBytes, int64(50<<30))
	}
}

// Zero must still write the file: "explicitly unlimited" and "no config at all"
// should not be distinguishable to the tenant.
func TestWriteQuotaConfigWritesZeroLimit(t *testing.T) {
	orgDir := t.TempDir()
	m := &OrgManager{cfg: Config{}}

	path, err := m.writeQuotaConfig(orgDir, OrgRecord{Slug: "foo"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("an unlimited org must still get a config file: %v", err)
	}
}
