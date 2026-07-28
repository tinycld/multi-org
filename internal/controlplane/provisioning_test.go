package controlplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"tinycld.org/multi-org/internal/store"
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
	if err := cpInitForTest(cp); err != nil {
		t.Fatal(err)
	}

	s := store.New(root)
	if err := s.Publish("@tinycld/core", "1.0.0", map[string][]byte{"server/a.pb.js": []byte("1")}); err != nil {
		t.Fatal(err)
	}

	p := NewProvisioner(cp.App, root, s, func(slug string) {}, nil) // evict no-op
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

// TestCreateOrg_VerifiesTenantBootBeforeReturning proves CreateOrg boots the
// org through the verify hook (which production wires to the org manager, so
// the org's migrations run inside the confined tenant process) — and that
// verify observes the org already active, since the manager refuses to load a
// non-active org.
func TestCreateOrg_VerifiesTenantBootBeforeReturning(t *testing.T) {
	root := t.TempDir()
	cp, _ := New(filepath.Join(root, "pb_control", "pb_data"))
	if err := cp.App.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer cp.App.ResetBootstrapState()
	if err := cpInitForTest(cp); err != nil {
		t.Fatal(err)
	}
	s := store.New(root)
	_ = s.Publish("@tinycld/core", "1.0.0", map[string][]byte{"server/a.pb.js": []byte("1")})

	var calls []string
	statusAtVerify := ""
	verify := func(ctx context.Context, slug string) error {
		calls = append(calls, slug)
		if rec, err := cp.App.FindFirstRecordByData("orgs", "slug", slug); err == nil {
			statusAtVerify = rec.GetString("status")
		}
		return nil
	}
	p := NewProvisioner(cp.App, root, s, func(string) {}, verify)
	rec, err := p.CreateOrg("acme", "Acme", map[string]string{"@tinycld/core": "1.0.0"})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if len(calls) != 1 || calls[0] != "acme" {
		t.Fatalf("expected exactly one verify call for acme, got %v", calls)
	}
	// The manager's load path refuses status != "active", so verify must see
	// the record already activated.
	if statusAtVerify != "active" {
		t.Fatalf("verify observed status %q, want active", statusAtVerify)
	}
	if rec.GetString("status") != "active" {
		t.Fatalf("expected final status active, got %s", rec.GetString("status"))
	}
}

// TestCreateOrg_VerifyFailureRollsBackActivation proves a failed tenant boot
// (e.g. a broken migration, reported through the readiness handshake) fails
// provisioning with the child's reason and leaves the org resumable — never
// active.
func TestCreateOrg_VerifyFailureRollsBackActivation(t *testing.T) {
	root := t.TempDir()
	cp, _ := New(filepath.Join(root, "pb_control", "pb_data"))
	if err := cp.App.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer cp.App.ResetBootstrapState()
	if err := cpInitForTest(cp); err != nil {
		t.Fatal(err)
	}
	s := store.New(root)
	_ = s.Publish("@tinycld/core", "1.0.0", map[string][]byte{"server/a.pb.js": []byte("1")})

	bootErr := errors.New("acme failed to start: migration 1700_bad.js: boom")
	failing := func(ctx context.Context, slug string) error { return bootErr }
	p := NewProvisioner(cp.App, root, s, func(string) {}, failing)

	if _, err := p.CreateOrg("acme", "Acme", map[string]string{"@tinycld/core": "1.0.0"}); err == nil {
		t.Fatal("expected CreateOrg to fail when the tenant never became ready")
	} else if !errors.Is(err, bootErr) {
		t.Fatalf("expected the child's boot failure as the cause, got: %v", err)
	}

	rec, err := cp.App.FindFirstRecordByData("orgs", "slug", "acme")
	if err != nil {
		t.Fatalf("org row should survive for resume: %v", err)
	}
	if got := rec.GetString("status"); got != "provisioning" {
		t.Fatalf("expected failed org rolled back to provisioning, got %q", got)
	}

	// Once the boot succeeds (fixed package deployed), the same CreateOrg
	// resumes the stranded row to active.
	p2 := NewProvisioner(cp.App, root, s, func(string) {}, func(context.Context, string) error { return nil })
	rec2, err := p2.CreateOrg("acme", "Acme", map[string]string{"@tinycld/core": "1.0.0"})
	if err != nil {
		t.Fatalf("expected resume after fixed boot, got: %v", err)
	}
	if rec2.GetString("status") != "active" {
		t.Fatalf("expected resumed org active, got %s", rec2.GetString("status"))
	}
}

func TestProvision_DeployEvicts(t *testing.T) {
	root := t.TempDir()
	cp, _ := New(filepath.Join(root, "pb_control", "pb_data"))
	_ = cp.App.Bootstrap()
	defer cp.App.ResetBootstrapState()
	_ = cpInitForTest(cp)

	s := store.New(root)
	_ = s.Publish("@tinycld/core", "1.0.0", map[string][]byte{"server/a.pb.js": []byte("1")})
	_ = s.Publish("@tinycld/core", "1.1.0", map[string][]byte{"server/a.pb.js": []byte("2")})

	evicted := ""
	p := NewProvisioner(cp.App, root, s, func(slug string) { evicted = slug }, nil)
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
	_ = cpInitForTest(cp)
	s := store.New(root)
	_ = s.Publish("@tinycld/core", "1.0.0", map[string][]byte{"server/a.pb.js": []byte("1")})

	p := NewProvisioner(cp.App, root, s, func(string) {}, nil)
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
	_ = cpInitForTest(cp)
	s := store.New(root)

	p := NewProvisioner(cp.App, root, s, func(string) {}, nil)
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
	_ = cpInitForTest(cp)
	s := store.New(root)
	_ = s.Publish("@tinycld/core", "1.0.0", map[string][]byte{"server/a.pb.js": []byte("1")})
	p := NewProvisioner(cp.App, root, s, func(string) {}, nil)
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
	_ = cpInitForTest(cp)
	s := store.New(root)
	p := NewProvisioner(cp.App, root, s, func(string) {}, nil)
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
	if err := cpInitForTest(cp); err != nil {
		t.Fatal(err)
	}
	s := store.New(root)
	if err := s.Publish("@tinycld/core", "1.0.0", map[string][]byte{"server/a.pb.js": []byte("1")}); err != nil {
		t.Fatal(err)
	}
	p := NewProvisioner(cp.App, root, s, func(string) {}, nil)
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
	if err := cpInitForTest(cp); err != nil {
		t.Fatal(err)
	}
	s := store.New(root)
	_ = s.Publish("@tinycld/core", "1.0.0", map[string][]byte{"server/a.pb.js": []byte("1")})
	_ = s.Publish("@tinycld/core", "1.1.0", map[string][]byte{"server/a.pb.js": []byte("2")})
	p := NewProvisioner(cp.App, root, s, func(string) {}, nil)
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
	if err := cpInitForTest(cp); err != nil {
		t.Fatal(err)
	}
	s := store.New(root)
	_ = s.Publish("@tinycld/core", "1.0.0", map[string][]byte{"server/a.pb.js": []byte("1")})
	p := NewProvisioner(cp.App, root, s, func(string) {}, nil)
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
