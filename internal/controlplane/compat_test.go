package controlplane

import (
	"path/filepath"
	"strings"
	"testing"

	"tinycld.org/multi-org/internal/lockfile"
	"tinycld.org/multi-org/internal/store"
)

// manifestTS builds a minimal manifest.ts whose evaluated manifest.json
// carries the given peerVersions block — the same publish path production
// uses, so these tests exercise the real manifest round-trip rather than
// hand-writing a manifest.json.
func manifestTS(name, slug, version string, peers map[string]string) []byte {
	var b strings.Builder
	b.WriteString("const manifest = {\n")
	b.WriteString("    name: '" + name + "',\n")
	b.WriteString("    slug: '" + slug + "',\n")
	b.WriteString("    version: '" + version + "',\n")
	b.WriteString("    description: 'test package',\n")
	if len(peers) > 0 {
		b.WriteString("    peerVersions: {\n")
		for peer, rng := range peers {
			b.WriteString("        '" + peer + "': '" + rng + "',\n")
		}
		b.WriteString("    },\n")
	}
	b.WriteString("}\nexport default manifest\n")
	return []byte(b.String())
}

// compatFixture stands up a control plane + store + provisioner with
// @tinycld/core@0.0.4 published (manifest-bearing, no peer requirements).
func compatFixture(t *testing.T) (*Provisioner, *store.PackageStore, string) {
	t.Helper()
	root := t.TempDir()
	cp, err := New(filepath.Join(root, "pb_control", "pb_data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.App.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cp.App.ResetBootstrapState() })
	if err := cpInitForTest(cp); err != nil {
		t.Fatal(err)
	}

	s := store.New(root)
	p := NewProvisioner(cp.App, root, s, func(string) {}, nil)

	if err := p.PublishPackage("@tinycld/core", "0.0.4", map[string][]byte{
		"manifest.ts":    manifestTS("@tinycld/core", "core", "0.0.4", nil),
		"server/a.pb.js": []byte("1"),
	}, "official"); err != nil {
		t.Fatal(err)
	}
	return p, s, root
}

func publishMailRequiring(t *testing.T, p *Provisioner, coreRange string) {
	t.Helper()
	err := p.PublishPackage("@tinycld/mail", "1.2.0", map[string][]byte{
		"manifest.ts": manifestTS("@tinycld/mail", "mail", "1.2.0",
			map[string]string{"@tinycld/core": coreRange}),
		"server/m.pb.js": []byte("x"),
	}, "official")
	if err != nil {
		t.Fatal(err)
	}
}

func TestCreateOrg_RejectsUnsatisfiedPeerVersions(t *testing.T) {
	p, _, _ := compatFixture(t)
	publishMailRequiring(t, p, ">=0.4.0 <0.5.0")

	_, err := p.CreateOrg("acme", "Acme", map[string]string{
		"@tinycld/core": "0.0.4",
		"@tinycld/mail": "1.2.0",
	})
	if err == nil {
		t.Fatal("expected CreateOrg to refuse an unsatisfiable peerVersions set")
	}
	for _, want := range []string{"@tinycld/mail", ">=0.4.0 <0.5.0", "0.0.4"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should name %q, got: %v", want, err)
		}
	}

	// The refusal must leave nothing active: a retried create with a fixed
	// lockfile has to be able to resume cleanly.
	if rec, _ := p.app.FindFirstRecordByData("orgs", "slug", "acme"); rec != nil {
		if rec.GetString("status") == "active" {
			t.Fatalf("org must not be active after a refused create, got %q", rec.GetString("status"))
		}
	}
}

func TestCreateOrg_AcceptsSatisfiedPeerVersions(t *testing.T) {
	p, _, _ := compatFixture(t)
	publishMailRequiring(t, p, ">=0.0.4 <0.1.0")

	rec, err := p.CreateOrg("acme", "Acme", map[string]string{
		"@tinycld/core": "0.0.4",
		"@tinycld/mail": "1.2.0",
	})
	if err != nil {
		t.Fatalf("CreateOrg with a satisfied range must succeed, got %v", err)
	}
	if rec.GetString("status") != "active" {
		t.Fatalf("expected active org, got %q", rec.GetString("status"))
	}
}

func TestCreateOrg_RejectsMissingPeer(t *testing.T) {
	p, _, _ := compatFixture(t)
	publishMailRequiring(t, p, ">=0.0.4 <0.1.0")

	_, err := p.CreateOrg("acme", "Acme", map[string]string{
		"@tinycld/mail": "1.2.0",
	})
	if err == nil {
		t.Fatal("expected CreateOrg to refuse a lockfile missing a required peer")
	}
	if !strings.Contains(err.Error(), "not in the lockfile") {
		t.Fatalf("error should say the peer is missing, got: %v", err)
	}
}

func TestDeploy_RejectsUnsatisfiedPeerVersions(t *testing.T) {
	p, _, _ := compatFixture(t)
	publishMailRequiring(t, p, ">=0.4.0 <0.5.0")

	if _, err := p.CreateOrg("acme", "Acme", map[string]string{
		"@tinycld/core": "0.0.4",
	}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	err := p.Deploy("acme", map[string]string{
		"@tinycld/core": "0.0.4",
		"@tinycld/mail": "1.2.0",
	})
	if err == nil {
		t.Fatal("expected Deploy to refuse an unsatisfiable peerVersions set")
	}

	// The refused deploy must not have replaced the org's lockfile.
	rec, _ := p.app.FindFirstRecordByData("orgs", "slug", "acme")
	if rec == nil {
		t.Fatal("org record missing")
	}
	if strings.Contains(rec.GetString("lockfile"), "@tinycld/mail") {
		t.Fatal("refused deploy must not overwrite the stored lockfile")
	}
}

// Unit-level cases on the checker itself, driven through the real store dirs
// so readManifestCapabilities runs against genuinely materialized manifests.
func TestCheckPeerVersions_EdgeCases(t *testing.T) {
	p, s, _ := compatFixture(t)

	// A package with no manifest at all declares no requirements.
	if err := s.Publish("@tinycld/bare", "9.9.9", map[string][]byte{"server/x.pb.js": []byte("1")}); err != nil {
		t.Fatal(err)
	}
	resolved, err := lockfile.OrgLockfile(map[string]string{
		"@tinycld/core": "0.0.4",
		"@tinycld/bare": "9.9.9",
	}).Resolve(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckPeerVersions(resolved); err != nil {
		t.Fatalf("manifest-less package must pass, got %v", err)
	}

	// An unparsable range is a violation, not a pass.
	if err := p.PublishPackage("@tinycld/typo", "1.0.0", map[string][]byte{
		"manifest.ts": manifestTS("@tinycld/typo", "typo", "1.0.0",
			map[string]string{"@tinycld/core": "not-a-range!!"}),
	}, "official"); err != nil {
		t.Fatal(err)
	}
	resolved, err = lockfile.OrgLockfile(map[string]string{
		"@tinycld/core": "0.0.4",
		"@tinycld/typo": "1.0.0",
	}).Resolve(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckPeerVersions(resolved); err == nil {
		t.Fatal("unparsable range must be a violation")
	} else if !strings.Contains(err.Error(), "unparsable") {
		t.Fatalf("error should say unparsable, got: %v", err)
	}
}
