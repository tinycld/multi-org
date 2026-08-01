package controlplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// baseLock is the minimal buildable package set: every org boots from a built
// artifact, so a lockfile must name the app shell (pkgbuild.BaseMemberSlug).
var baseLock = map[string]string{"tinycld": "1.0.0"}

// newProvCP boots an initialized control plane under a fresh root.
func newProvCP(t *testing.T) (*ControlPlane, string) {
	t.Helper()
	root := t.TempDir()
	cp, err := New(filepath.Join(root, "pb_control", "pb_data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.App.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { WaitForAppDeploys(cp.App); _ = cp.App.ResetBootstrapState() })
	if err := cpInitForTest(cp); err != nil {
		t.Fatal(err)
	}
	return cp, root
}

// newFakeProvisioner wires a Provisioner whose deployer is backed by a fake
// artifact builder — the test-side analogue of EnableBuilds, which requires a
// real *builder.Builder. Store-era fixtures published packages; artifact-era
// fixtures fake the builder instead, and CreateOrg records the hash it returns.
func newFakeProvisioner(cp *ControlPlane, root string, evict EvictFunc, verify VerifyFunc) *Provisioner {
	p := NewProvisioner(cp.App, root, evict, verify)
	p.deployer = newDeployer(cp.App, root, &fakeArtifactBuilder{hash: hashOld}, evict, verify, quietTestLogger())
	return p
}

func TestProvision_CreatesOrgRowAndDirs(t *testing.T) {
	cp, root := newProvCP(t)
	p := newFakeProvisioner(cp, root, func(string) {}, nil)

	rec, err := p.CreateOrg("acme", "Acme Inc", baseLock)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if rec.GetString("slug") != "acme" || rec.GetString("status") != "active" {
		t.Fatalf("unexpected org record: slug=%s status=%s", rec.GetString("slug"), rec.GetString("status"))
	}
	if rec.GetString("recipe_hash") != hashOld {
		t.Fatalf("recipe_hash = %q, want the built artifact %s", rec.GetString("recipe_hash"), hashOld)
	}
	fi, err := os.Stat(filepath.Join(root, "pb_orgs", "acme", "pb_data"))
	if err != nil || !fi.IsDir() {
		t.Fatalf("expected pb_data dir: %v", err)
	}
	// The live pb_hooks/pb_public/pb_migrations names come from the committed
	// artifact at load (materialize), so CreateOrg pre-creates nothing else.
	for _, sub := range []string{"pb_hooks", "pb_public", "pb_migrations"} {
		if _, err := os.Stat(filepath.Join(root, "pb_orgs", "acme", sub)); !os.IsNotExist(err) {
			t.Fatalf("CreateOrg should not pre-create %s (err=%v)", sub, err)
		}
	}
}

// TestCreateOrg_RefusesNonBaseLockfile pins the artifact-set gate: a lockfile
// without the app shell cannot build an artifact, so CreateOrg must refuse it
// up front instead of leaving a bare org that no load path could ever boot.
func TestCreateOrg_RefusesNonBaseLockfile(t *testing.T) {
	cp, root := newProvCP(t)
	p := newFakeProvisioner(cp, root, func(string) {}, nil)

	if _, err := p.CreateOrg("acme", "Acme", map[string]string{"@tinycld/core": "1.0.0"}); err == nil || !strings.Contains(err.Error(), "app shell") {
		t.Fatalf("CreateOrg = %v, want the app-shell refusal", err)
	}
	if rec, _ := cp.App.FindFirstRecordByData("orgs", "slug", "acme"); rec != nil {
		t.Fatal("refused CreateOrg must leave no org row behind")
	}
}

// TestCreateOrg_VerifiesTenantBootBeforeReturning proves CreateOrg boots the
// org through the verify hook (which production wires to the org manager, so
// the org's migrations run inside the confined tenant process) — and that
// verify observes the org already active, since the manager refuses to load a
// non-active org.
func TestCreateOrg_VerifiesTenantBootBeforeReturning(t *testing.T) {
	cp, root := newProvCP(t)

	var calls []string
	statusAtVerify := ""
	verify := func(ctx context.Context, slug string) error {
		calls = append(calls, slug)
		if rec, err := cp.App.FindFirstRecordByData("orgs", "slug", slug); err == nil {
			statusAtVerify = rec.GetString("status")
		}
		return nil
	}
	p := newFakeProvisioner(cp, root, func(string) {}, verify)
	rec, err := p.CreateOrg("acme", "Acme", baseLock)
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
	cp, root := newProvCP(t)

	bootErr := errors.New("acme failed to start: migration 1700_bad.js: boom")
	failing := func(ctx context.Context, slug string) error { return bootErr }
	p := newFakeProvisioner(cp, root, func(string) {}, failing)

	if _, err := p.CreateOrg("acme", "Acme", baseLock); err == nil {
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

	// Once the boot succeeds (fixed artifact), the same CreateOrg resumes the
	// stranded row to active.
	p2 := newFakeProvisioner(cp, root, func(string) {}, func(context.Context, string) error { return nil })
	rec2, err := p2.CreateOrg("acme", "Acme", baseLock)
	if err != nil {
		t.Fatalf("expected resume after fixed boot, got: %v", err)
	}
	if rec2.GetString("status") != "active" {
		t.Fatalf("expected resumed org active, got %s", rec2.GetString("status"))
	}
}

// TestProvision_DeployEvicts proves a deploy through the D6 orchestrator
// evicts the org's cached instance so the next request respawns on the new
// build. The eviction is the async tail of Deploy (finish), so the test waits
// for it.
func TestProvision_DeployEvicts(t *testing.T) {
	cp, root := newProvCP(t)
	p := newFakeProvisioner(cp, root, func(string) {}, nil)
	if _, err := p.CreateOrg("acme", "Acme", baseLock); err != nil {
		t.Fatal(err)
	}

	var evicted atomic.Value
	d := newDeployer(cp.App, root, &fakeArtifactBuilder{hash: hashNew},
		func(slug string) { evicted.Store(slug) }, nil, quietTestLogger())
	if _, err := d.Deploy(context.Background(), "acme", map[string]string{"tinycld": "1.1.0"}, ""); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "deploy evicted acme", func() bool {
		slug, _ := evicted.Load().(string)
		return slug == "acme"
	})
}

func TestProvision_SuspendResumeArchive(t *testing.T) {
	cp, root := newProvCP(t)
	p := newFakeProvisioner(cp, root, func(string) {}, nil)
	if _, err := p.CreateOrg("acme", "Acme", baseLock); err != nil {
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

func TestProvision_DuplicateSlugErrors(t *testing.T) {
	cp, root := newProvCP(t)
	p := newFakeProvisioner(cp, root, func(string) {}, nil)
	if _, err := p.CreateOrg("acme", "Acme", baseLock); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateOrg("acme", "Acme2", baseLock); err == nil {
		t.Fatal("expected duplicate slug to error")
	}
}

func TestProvision_InvalidSlugErrors(t *testing.T) {
	cp, root := newProvCP(t)
	p := newFakeProvisioner(cp, root, func(string) {}, nil)
	if _, err := p.CreateOrg("Acme_Bad!", "x", baseLock); err == nil {
		t.Fatal("expected invalid slug to error")
	}
}

func TestProvision_CreateOrgResumesStrandedRow(t *testing.T) {
	cp, root := newProvCP(t)
	p := newFakeProvisioner(cp, root, func(string) {}, nil)
	rec, err := p.CreateOrg("acme", "Acme", baseLock)
	if err != nil {
		t.Fatal(err)
	}
	// simulate a strand: force status back to provisioning
	rec.Set("status", "provisioning")
	if err := cp.App.Save(rec); err != nil {
		t.Fatal(err)
	}
	// re-run must RESUME (not error) and end active
	rec2, err := p.CreateOrg("acme", "Acme", baseLock)
	if err != nil {
		t.Fatalf("expected resume, got error: %v", err)
	}
	if rec2.GetString("status") != "active" {
		t.Fatalf("expected resumed org active, got %s", rec2.GetString("status"))
	}
	// and an ACTIVE org still rejects duplicate create
	if _, err := p.CreateOrg("acme", "Acme", baseLock); err == nil {
		t.Fatal("expected duplicate active org to error")
	}
}

// TestProvision_DeployWritesAuditRecord proves every deploy leaves a
// deployments row carrying the new build's recipe hash and its settled status
// — the operator's audit trail for what an org ran, and when.
func TestProvision_DeployWritesAuditRecord(t *testing.T) {
	cp, root := newProvCP(t)
	p := newFakeProvisioner(cp, root, func(string) {}, nil)
	if _, err := p.CreateOrg("acme", "Acme", baseLock); err != nil {
		t.Fatal(err)
	}

	d := newDeployer(cp.App, root, &fakeArtifactBuilder{hash: hashNew},
		func(string) {}, nil, quietTestLogger())
	if _, err := d.Deploy(context.Background(), "acme", map[string]string{"tinycld": "1.1.0"}, ""); err != nil {
		t.Fatal(err)
	}

	// nil verify commits without a boot check; the row settles asynchronously.
	waitUntil(t, "deployment committed", func() bool {
		deps, err := cp.App.FindAllRecords("deployments")
		return err == nil && len(deps) == 1 && deps[0].GetString("status") == "committed"
	})
	deps, err := cp.App.FindAllRecords("deployments")
	if err != nil {
		t.Fatal(err)
	}
	if deps[0].GetString("recipe_hash") != hashNew {
		t.Fatalf("deployment recipe_hash = %q, want %s", deps[0].GetString("recipe_hash"), hashNew)
	}
}

func TestProvision_DeployRejectsArchivedOrg(t *testing.T) {
	cp, root := newProvCP(t)
	p := newFakeProvisioner(cp, root, func(string) {}, nil)
	if _, err := p.CreateOrg("acme", "Acme", baseLock); err != nil {
		t.Fatal(err)
	}
	if err := p.Archive("acme"); err != nil {
		t.Fatal(err)
	}
	d := newDeployer(cp.App, root, &fakeArtifactBuilder{hash: hashNew},
		func(string) {}, nil, quietTestLogger())
	if _, err := d.Deploy(context.Background(), "acme", baseLock, ""); err == nil || !strings.Contains(err.Error(), "archived") {
		t.Fatalf("Deploy to archived org = %v, want a status refusal", err)
	}
}
