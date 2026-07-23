package controlplane

import (
	"path/filepath"
	"testing"
)

func TestControlPlane_BootstrapsWithOrgsCollection(t *testing.T) {
	root := t.TempDir()
	cp, err := New(filepath.Join(root, "pb_control", "pb_data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.App.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	defer cp.App.ResetBootstrapState()

	if err := cp.App.RunAllMigrations(); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	col, err := cp.App.FindCollectionByNameOrId("orgs")
	if err != nil {
		t.Fatalf("expected orgs collection: %v", err)
	}
	if col == nil {
		t.Fatal("orgs collection is nil")
	}
	for _, name := range []string{"packages", "deployments"} {
		if _, err := cp.App.FindCollectionByNameOrId(name); err != nil {
			t.Fatalf("expected %s collection: %v", name, err)
		}
	}
}
