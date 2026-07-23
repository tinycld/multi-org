package controlplane

import (
	"os"
	"path/filepath"
	"testing"

	"tinycld.org/multitenant/internal/store"
)

func TestProvision_CreatesOrgRowAndDirs(t *testing.T) {
	root := t.TempDir()
	cp, err := New(filepath.Join(root, "pb_control", "pb_data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.App.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer cp.App.ResetBootstrapState()
	if err := cp.App.RunAllMigrations(); err != nil {
		t.Fatal(err)
	}

	s := store.New(root)
	if err := s.Publish("@tinycld/core", "1.0.0", map[string][]byte{"server/a.pb.js": []byte("1")}); err != nil {
		t.Fatal(err)
	}

	p := NewProvisioner(cp.App, root, s, func(slug string) {}) // evict no-op
	rec, err := p.CreateOrg("acme", "Acme Inc", map[string]string{"@tinycld/core": "1.0.0"})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if rec.GetString("slug") != "acme" || rec.GetString("status") != "active" {
		t.Fatalf("unexpected org record: slug=%s status=%s", rec.GetString("slug"), rec.GetString("status"))
	}
	for _, sub := range []string{"pb_data", "pb_hooks", "pb_public"} {
		fi, err := os.Stat(filepath.Join(root, "pb_orgs", "acme", sub))
		if err != nil || !fi.IsDir() {
			t.Fatalf("expected %s dir: %v", sub, err)
		}
	}
}

func TestProvision_DeployEvicts(t *testing.T) {
	root := t.TempDir()
	cp, _ := New(filepath.Join(root, "pb_control", "pb_data"))
	_ = cp.App.Bootstrap()
	defer cp.App.ResetBootstrapState()
	_ = cp.App.RunAllMigrations()

	s := store.New(root)
	_ = s.Publish("@tinycld/core", "1.0.0", map[string][]byte{"server/a.pb.js": []byte("1")})
	_ = s.Publish("@tinycld/core", "1.1.0", map[string][]byte{"server/a.pb.js": []byte("2")})

	evicted := ""
	p := NewProvisioner(cp.App, root, s, func(slug string) { evicted = slug })
	if _, err := p.CreateOrg("acme", "Acme", map[string]string{"@tinycld/core": "1.0.0"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Deploy("acme", map[string]string{"@tinycld/core": "1.1.0"}); err != nil {
		t.Fatal(err)
	}
	if evicted != "acme" {
		t.Fatalf("expected deploy to evict acme, got %q", evicted)
	}
}

func TestProvision_SuspendResumeArchive(t *testing.T) {
	root := t.TempDir()
	cp, _ := New(filepath.Join(root, "pb_control", "pb_data"))
	_ = cp.App.Bootstrap()
	defer cp.App.ResetBootstrapState()
	_ = cp.App.RunAllMigrations()
	s := store.New(root)
	_ = s.Publish("@tinycld/core", "1.0.0", map[string][]byte{"server/a.pb.js": []byte("1")})

	p := NewProvisioner(cp.App, root, s, func(string) {})
	if _, err := p.CreateOrg("acme", "Acme", map[string]string{"@tinycld/core": "1.0.0"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		fn   func(string) error
		want string
	}{
		{p.Suspend, "suspended"},
		{p.Resume, "active"},
		{p.Archive, "archived"},
	} {
		if err := tc.fn("acme"); err != nil {
			t.Fatal(err)
		}
		rec, err := cp.App.FindFirstRecordByData("orgs", "slug", "acme")
		if err != nil {
			t.Fatal(err)
		}
		if rec.GetString("status") != tc.want {
			t.Fatalf("expected status %s, got %s", tc.want, rec.GetString("status"))
		}
	}
}

func TestProvision_PublishPackageRegisters(t *testing.T) {
	root := t.TempDir()
	cp, _ := New(filepath.Join(root, "pb_control", "pb_data"))
	_ = cp.App.Bootstrap()
	defer cp.App.ResetBootstrapState()
	_ = cp.App.RunAllMigrations()
	s := store.New(root)

	p := NewProvisioner(cp.App, root, s, func(string) {})
	if err := p.PublishPackage("@tinycld/mail", "1.2.0", map[string][]byte{"server/m.pb.js": []byte("x")}, "official"); err != nil {
		t.Fatal(err)
	}
	rec, err := cp.App.FindFirstRecordByData("packages", "name", "@tinycld/mail")
	if err != nil {
		t.Fatalf("expected package row: %v", err)
	}
	if rec.GetString("version") != "1.2.0" || rec.GetString("kind") != "official" {
		t.Fatalf("unexpected package: version=%s kind=%s", rec.GetString("version"), rec.GetString("kind"))
	}
	// store dir exists
	if _, err := s.VersionDir("@tinycld/mail", "1.2.0"); err != nil {
		t.Fatalf("expected published store version: %v", err)
	}
}

func TestProvision_DuplicateSlugErrors(t *testing.T) {
	root := t.TempDir()
	cp, _ := New(filepath.Join(root, "pb_control", "pb_data"))
	_ = cp.App.Bootstrap()
	defer cp.App.ResetBootstrapState()
	_ = cp.App.RunAllMigrations()
	s := store.New(root)
	_ = s.Publish("@tinycld/core", "1.0.0", map[string][]byte{"server/a.pb.js": []byte("1")})
	p := NewProvisioner(cp.App, root, s, func(string) {})
	if _, err := p.CreateOrg("acme", "Acme", map[string]string{"@tinycld/core": "1.0.0"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateOrg("acme", "Acme2", map[string]string{"@tinycld/core": "1.0.0"}); err == nil {
		t.Fatal("expected duplicate slug to error")
	}
}

func TestProvision_InvalidSlugErrors(t *testing.T) {
	root := t.TempDir()
	cp, _ := New(filepath.Join(root, "pb_control", "pb_data"))
	_ = cp.App.Bootstrap()
	defer cp.App.ResetBootstrapState()
	_ = cp.App.RunAllMigrations()
	s := store.New(root)
	p := NewProvisioner(cp.App, root, s, func(string) {})
	if _, err := p.CreateOrg("Acme_Bad!", "x", map[string]string{}); err == nil {
		t.Fatal("expected invalid slug to error")
	}
}

func TestProvision_CreateOrgResumesStrandedRow(t *testing.T) {
	root := t.TempDir()
	cp, _ := New(filepath.Join(root, "pb_control", "pb_data"))
	if err := cp.App.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer cp.App.ResetBootstrapState()
	if err := cp.App.RunAllMigrations(); err != nil {
		t.Fatal(err)
	}
	s := store.New(root)
	if err := s.Publish("@tinycld/core", "1.0.0", map[string][]byte{"server/a.pb.js": []byte("1")}); err != nil {
		t.Fatal(err)
	}
	p := NewProvisioner(cp.App, root, s, func(string) {})
	rec, err := p.CreateOrg("acme", "Acme", map[string]string{"@tinycld/core": "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	// simulate a strand: force status back to provisioning
	rec.Set("status", "provisioning")
	if err := cp.App.Save(rec); err != nil {
		t.Fatal(err)
	}
	// re-run must RESUME (not error) and end active
	rec2, err := p.CreateOrg("acme", "Acme", map[string]string{"@tinycld/core": "1.0.0"})
	if err != nil {
		t.Fatalf("expected resume, got error: %v", err)
	}
	if rec2.GetString("status") != "active" {
		t.Fatalf("expected resumed org active, got %s", rec2.GetString("status"))
	}
	// and an ACTIVE org still rejects duplicate create
	if _, err := p.CreateOrg("acme", "Acme", map[string]string{"@tinycld/core": "1.0.0"}); err == nil {
		t.Fatal("expected duplicate active org to error")
	}
}

func TestProvision_DeployWritesAuditRecord(t *testing.T) {
	root := t.TempDir()
	cp, _ := New(filepath.Join(root, "pb_control", "pb_data"))
	if err := cp.App.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer cp.App.ResetBootstrapState()
	if err := cp.App.RunAllMigrations(); err != nil {
		t.Fatal(err)
	}
	s := store.New(root)
	_ = s.Publish("@tinycld/core", "1.0.0", map[string][]byte{"server/a.pb.js": []byte("1")})
	_ = s.Publish("@tinycld/core", "1.1.0", map[string][]byte{"server/a.pb.js": []byte("2")})
	p := NewProvisioner(cp.App, root, s, func(string) {})
	if _, err := p.CreateOrg("acme", "Acme", map[string]string{"@tinycld/core": "1.0.0"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Deploy("acme", map[string]string{"@tinycld/core": "1.1.0"}); err != nil {
		t.Fatal(err)
	}
	deps, err := cp.App.FindAllRecords("deployments")
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected 1 deployment audit record, got %d", len(deps))
	}
}

func TestProvision_DeployRejectsArchivedOrg(t *testing.T) {
	root := t.TempDir()
	cp, _ := New(filepath.Join(root, "pb_control", "pb_data"))
	if err := cp.App.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer cp.App.ResetBootstrapState()
	if err := cp.App.RunAllMigrations(); err != nil {
		t.Fatal(err)
	}
	s := store.New(root)
	_ = s.Publish("@tinycld/core", "1.0.0", map[string][]byte{"server/a.pb.js": []byte("1")})
	p := NewProvisioner(cp.App, root, s, func(string) {})
	if _, err := p.CreateOrg("acme", "Acme", map[string]string{"@tinycld/core": "1.0.0"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Archive("acme"); err != nil {
		t.Fatal(err)
	}
	if err := p.Deploy("acme", map[string]string{"@tinycld/core": "1.0.0"}); err == nil {
		t.Fatal("expected deploy to archived org to error")
	}
}
