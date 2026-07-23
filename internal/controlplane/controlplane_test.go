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
	defer cp.App.ResetBootstrapState()

	if err := cp.App.RunAllMigrations(); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	col, err := cp.App.FindCollectionByNameOrId("orgs")
	if err != nil {
		t.Fatalf("expected orgs collection: %v", err)
	}
	for _, name := range []string{"packages", "deployments"} {
		if _, err := cp.App.FindCollectionByNameOrId(name); err != nil {
			t.Fatalf("expected %s collection: %v", name, err)
		}
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
