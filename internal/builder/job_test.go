package builder

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"tinycld.org/core/pkgbuild"
)

// fakePipeline returns a pkgbuild.Pipeline whose subprocess seams are all
// stubbed: "go build" writes the -o target, streams do nothing, staging
// produces a minimal release dir. The pipeline's own step order and the
// runner's assemble/verify/stage logic stay real.
func fakePipeline(t *testing.T) pkgbuild.Pipeline {
	t.Helper()
	fakeRun := func(dir, name string, args ...string) (string, error) {
		if name == "go" && len(args) >= 3 && args[0] == "build" && args[1] == "-o" {
			if err := os.WriteFile(args[2], []byte("#!fake-binary"), 0o755); err != nil {
				return "", err
			}
		}
		return "", nil
	}
	fakeStream := func(_ func(string), dir, name string, args ...string) (string, error) {
		return "", nil
	}
	return pkgbuild.Pipeline{
		Run: fakeRun,
		// The CLI cross-compile step goes through RunEnv; write the -o target
		// (which sits behind -trimpath/-ldflags, not at args[1] like the
		// server build) so cli-dist/ gets populated when the build has a cli/.
		RunEnv: func(_ string, _ []string, name string, args ...string) (string, error) {
			if name == "go" && len(args) > 0 && args[0] == "build" {
				for i, a := range args {
					if a == "-o" && i+1 < len(args) {
						if err := os.WriteFile(args[i+1], []byte("#!fake-cli"), 0o755); err != nil {
							return "", err
						}
					}
				}
			}
			return "", nil
		},
		PnpmStream: fakeStream,
		ExpoStream: fakeStream,
		Stage: func(appDir string) (string, error) {
			stage := filepath.Join(appDir, "release-staging", "test-release")
			if err := os.MkdirAll(stage, 0o755); err != nil {
				return "", err
			}
			for name, body := range map[string]string{"app.html": "<html>", "release-id.txt": "test-release"} {
				if err := os.WriteFile(filepath.Join(stage, name), []byte(body), 0o644); err != nil {
					return "", err
				}
			}
			return stage, nil
		},
		NativeExport: func(_ pkgbuild.ProgressSink, _, _, _ string) ([]pkgbuild.BundleMeta, error) {
			return nil, nil
		},
		BinaryName: "tinycld",
	}
}

// TestInProcessRunner_AssemblesBuildsAndStages drives the real runner over a
// base-only workspace (node-free: the base member's identity comes from
// core/package.json, no manifest eval) with the pipeline's subprocesses faked.
func TestInProcessRunner_AssemblesBuildsAndStages(t *testing.T) {
	root := t.TempDir()
	baseDir := filepath.Join(root, "prefetched", "tinycld")
	if err := os.MkdirAll(filepath.Join(baseDir, "core"), 0o755); err != nil {
		t.Fatal(err)
	}
	corePkg := `{"name": "@tinycld/core", "version": "0.0.9"}` + "\n"
	if err := os.WriteFile(filepath.Join(baseDir, "core", "package.json"), []byte(corePkg), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := JobSpec{
		BuildID:      "recipe-test",
		BuildDir:     filepath.Join(root, "job", "build"),
		ArtifactDir:  filepath.Join(root, "job", "artifact"),
		ScaffoldRoot: writeScaffoldRoot(t),
		PnpmStoreDir: filepath.Join(root, "pnpm-store"),
		BinaryName:   "tinycld",
		Manifest: pkgbuild.RebuildManifest{
			BuildID: "recipe-test",
			Members: []pkgbuild.MemberSpec{{Slug: "tinycld", Version: "0.0.9", Spec: "tinycld@0.4.0"}},
		},
		MemberDirs:  map[string]string{"tinycld": baseDir},
		Integrities: map[string]string{"tinycld": "sha256:beef"},
	}

	runner := InProcessRunner{Pipeline: func(JobSpec) pkgbuild.Pipeline { return fakePipeline(t) }}
	if err := runner.Run(context.Background(), spec, nil); err != nil {
		t.Fatal(err)
	}

	// The assembled workspace: scaffold + member + resolved lock carrying the
	// parent-computed integrity.
	for _, rel := range []string{"package.json", "pnpm-workspace.yaml", "manifest.json", pkgbuild.MembersLockFile, "tinycld/core/package.json"} {
		if _, err := os.Stat(filepath.Join(spec.BuildDir, rel)); err != nil {
			t.Errorf("build dir missing %s: %v", rel, err)
		}
	}
	members, err := pkgbuild.ReadMembersLock(spec.BuildDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].Integrity != "sha256:beef" {
		t.Fatalf("members.lock = %+v", members)
	}

	// The staged artifact: binary, hook/migration mount points (empty here —
	// the faked pnpm install ran no generator), and the web release.
	for _, rel := range []string{"tinycld", "pb_hooks", "pb_migrations", "pb_public/app.html", "pb_public/release-id.txt"} {
		if _, err := os.Stat(filepath.Join(spec.ArtifactDir, rel)); err != nil {
			t.Errorf("artifact missing %s: %v", rel, err)
		}
	}
	info, err := os.Stat(filepath.Join(spec.ArtifactDir, "tinycld"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatal("staged binary is not executable")
	}

	// No cli/ module in this assembly → the (best-effort) cross-compile
	// skipped, and staging tolerates the absence rather than failing.
	if _, err := os.Stat(filepath.Join(spec.ArtifactDir, pkgbuild.CLIDistDirName)); !os.IsNotExist(err) {
		t.Fatalf("artifact should have no cli-dist without a cli module: %v", err)
	}
}

// TestInProcessRunner_StagesCliDist covers the assembly WITH a cli/ module:
// the pipeline cross-compiles into <appDir>/cli-dist and stageArtifact must
// carry it into the artifact (the build workspace is deleted after commit).
func TestInProcessRunner_StagesCliDist(t *testing.T) {
	root := t.TempDir()
	baseDir := filepath.Join(root, "prefetched", "tinycld")
	if err := os.MkdirAll(filepath.Join(baseDir, "core"), 0o755); err != nil {
		t.Fatal(err)
	}
	corePkg := `{"name": "@tinycld/core", "version": "0.0.9"}` + "\n"
	if err := os.WriteFile(filepath.Join(baseDir, "core", "package.json"), []byte(corePkg), 0o644); err != nil {
		t.Fatal(err)
	}
	// The member ships a cli/ module → the pipeline's cross-compile runs.
	if err := os.MkdirAll(filepath.Join(baseDir, "cli"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, "cli", "go.mod"), []byte("module tinycld.org/cli\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := JobSpec{
		BuildID:      "recipe-cli",
		BuildDir:     filepath.Join(root, "job", "build"),
		ArtifactDir:  filepath.Join(root, "job", "artifact"),
		ScaffoldRoot: writeScaffoldRoot(t),
		PnpmStoreDir: filepath.Join(root, "pnpm-store"),
		BinaryName:   "tinycld",
		Manifest: pkgbuild.RebuildManifest{
			BuildID: "recipe-cli",
			Members: []pkgbuild.MemberSpec{{Slug: "tinycld", Version: "0.0.9", Spec: "tinycld@0.4.0"}},
		},
		MemberDirs:  map[string]string{"tinycld": baseDir},
		Integrities: map[string]string{"tinycld": "sha256:beef"},
	}

	runner := InProcessRunner{Pipeline: func(JobSpec) pkgbuild.Pipeline { return fakePipeline(t) }}
	if err := runner.Run(context.Background(), spec, nil); err != nil {
		t.Fatal(err)
	}

	for _, target := range pkgbuild.CLITargets {
		staged := filepath.Join(spec.ArtifactDir, pkgbuild.CLIDistDirName, target.FileName())
		if _, err := os.Stat(staged); err != nil {
			t.Errorf("artifact missing %s: %v", target.FileName(), err)
		}
	}
}

func TestPrefetchedSource_RefusesUnknownMemberAndCopyCurrent(t *testing.T) {
	src := prefetchedSource{dirs: map[string]string{}, integrities: map[string]string{}}
	if _, err := src.Fetch(pkgbuild.MemberSpec{Slug: "ghost"}, t.TempDir()); err == nil {
		t.Fatal("Fetch of un-prefetched member succeeded")
	}
	if _, err := src.CopyCurrent(pkgbuild.MemberSpec{Slug: "tinycld"}, t.TempDir()); err == nil {
		t.Fatal("CopyCurrent succeeded — the builder has no current build")
	}
}
