package controlplane

import (
	"path/filepath"
	"testing"

	"github.com/pocketbase/pocketbase/core"
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
	defer func() { WaitForAppDeploys(cp.App); _ = cp.App.ResetBootstrapState() }()

	if err := cpInitForTest(cp); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	col, err := cp.App.FindCollectionByNameOrId("orgs")
	if err != nil {
		t.Fatalf("expected orgs collection: %v", err)
	}
	if _, err := cp.App.FindCollectionByNameOrId("deployments"); err != nil {
		t.Fatalf("expected deployments collection: %v", err)
	}
	// The store-era `packages` registry is dropped by 1900000003 — only the
	// publish endpoint ever wrote it, and that endpoint is gone with the store.
	if _, err := cp.App.FindCollectionByNameOrId("packages"); err == nil {
		t.Fatal("packages collection must be dropped after migrations")
	}

	// assert the schema shape landed, not just the collection.
	if f := col.Fields.GetByName("status"); f == nil {
		t.Fatal("orgs.status field missing")
	}
	if len(col.Indexes) == 0 {
		t.Fatal("expected orgs to have the unique slug index")
	}

	// deployments.org should be a relation to orgs.
	dep, err := cp.App.FindCollectionByNameOrId("deployments")
	if err != nil {
		t.Fatal(err)
	}
	orgRel := dep.Fields.GetByName("org")
	if orgRel == nil {
		t.Fatal("deployments.org field missing")
	}
	if _, ok := orgRel.(*core.RelationField); !ok {
		t.Fatalf("deployments.org should be a RelationField, got %T", orgRel)
	}
}
