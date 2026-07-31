package builder

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"tinycld.org/core/pkgbuild"
)

// JobSpec is everything a build job needs, computed by the trusted parent.
// The job consumes the pre-fetched member trees; nothing it produces feeds
// back into the artifact's identity (the parent already computed the recipe
// hash and writes RecipeFile itself).
type JobSpec struct {
	BuildID      string
	BuildDir     string // the workspace to assemble and build in
	ArtifactDir  string // where the job stages the runtime tree
	ScaffoldRoot string
	PnpmStoreDir string
	BinaryName   string
	Manifest     pkgbuild.RebuildManifest
	MemberDirs   map[string]string // slug → pre-fetched extracted package dir
	Integrities  map[string]string // slug → parent-computed tarball integrity
}

// Runner executes one build job. The production runner on Linux re-execs this
// binary into per-job confinement (uid/namespace/cgroup — task: the build
// executes package-author code by design); InProcessRunner is the unconfined
// fallback (darwin dev hosts) and the unit-test seam.
type Runner interface {
	Run(ctx context.Context, spec JobSpec, sink pkgbuild.ProgressSink) error
}

// InProcessRunner runs the job in this process: assemble the workspace from
// the pre-fetched members, re-verify peerVersions on disk, run the pkgbuild
// pipeline, and stage the runtime tree into spec.ArtifactDir.
type InProcessRunner struct {
	// Pipeline builds the pkgbuild.Pipeline for a job; nil means the real
	// in-process pipeline. Tests inject fake runners here.
	Pipeline func(spec JobSpec) pkgbuild.Pipeline
}

func (r InProcessRunner) Run(ctx context.Context, spec JobSpec, sink pkgbuild.ProgressSink) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	src := prefetchedSource{dirs: spec.MemberDirs, integrities: spec.Integrities}
	opts := pkgbuild.ScaffoldOptions{PnpmStoreDir: spec.PnpmStoreDir}
	if err := pkgbuild.AssembleBuild(sink, spec.Manifest, spec.BuildDir, src, spec.ScaffoldRoot, opts); err != nil {
		return fmt.Errorf("assemble: %w", err)
	}
	// Post-assemble on-disk re-check: the authoritative gate already ran
	// parent-side over evaluated manifests, but the assembled tree is what
	// actually gets built — pkgbuild's rule that the two must never disagree.
	if err := pkgbuild.VerifyTargetPeerVersions(spec.Manifest, spec.BuildDir); err != nil {
		return err
	}
	p := pkgbuild.Pipeline{BinaryName: spec.BinaryName}
	if r.Pipeline != nil {
		p = r.Pipeline(spec)
	}
	out, err := p.Execute(sink, spec.BuildDir, spec.BuildID)
	if err != nil {
		return err
	}
	return stageArtifact(spec, out)
}

// prefetchedSource satisfies pkgbuild.MemberSource from the parent's resolve
// output: Fetch copies the already-extracted tree into the build dir and
// reports the integrity the PARENT computed from the tarball — the job cannot
// substitute its own answer. CopyCurrent always refuses: a builder has no
// currently-active build (the design's "the builder always fetches").
type prefetchedSource struct {
	dirs        map[string]string
	integrities map[string]string
}

func (s prefetchedSource) Fetch(ms pkgbuild.MemberSpec, buildDir string) (string, error) {
	dir, ok := s.dirs[ms.Slug]
	if !ok {
		return "", fmt.Errorf("member %q was not pre-fetched", ms.Slug)
	}
	dest := filepath.Join(buildDir, ms.Slug)
	if err := os.RemoveAll(dest); err != nil {
		return "", err
	}
	if err := pkgbuild.CopyDir(dir, dest); err != nil {
		return "", err
	}
	return s.integrities[ms.Slug], nil
}

func (s prefetchedSource) CopyCurrent(ms pkgbuild.MemberSpec, _ string) (string, error) {
	return "", fmt.Errorf("builder has no current build to copy member %q from", ms.Slug)
}

// stageArtifact copies the runtime tree out of the built workspace into
// spec.ArtifactDir — exactly what a tenant needs to boot (D4's artifact
// store list): the server binary, the generated pb_hooks/ and pb_migrations/,
// and the staged web release (which already embeds native OTA bundles under
// native/) as pb_public/.
func stageArtifact(spec JobSpec, out pkgbuild.BuildOutput) error {
	appDir := filepath.Join(spec.BuildDir, pkgbuild.BaseMemberSlug)
	if err := os.MkdirAll(spec.ArtifactDir, 0o755); err != nil {
		return err
	}
	binary := filepath.Join(appDir, spec.BinaryName)
	if _, err := os.Stat(binary); err != nil {
		return fmt.Errorf("stage artifact: built binary missing: %w", err)
	}
	if _, err := pkgbuild.RunCmd(".", "cp", "-a", binary, filepath.Join(spec.ArtifactDir, spec.BinaryName)); err != nil {
		return fmt.Errorf("stage artifact: copy binary: %w", err)
	}
	serverDir := filepath.Join(appDir, "server")
	for _, name := range []string{"pb_hooks", "pb_migrations"} {
		src := filepath.Join(serverDir, name)
		dest := filepath.Join(spec.ArtifactDir, name)
		if _, err := os.Stat(src); err != nil {
			if os.IsNotExist(err) {
				// A workspace whose generator emitted nothing (no hook-bearing
				// members) stages an empty dir so the tenant mount points exist.
				if err := os.MkdirAll(dest, 0o755); err != nil {
					return err
				}
				continue
			}
			return err
		}
		if err := copyTreeDeref(src, dest); err != nil {
			return fmt.Errorf("stage artifact: copy %s: %w", name, err)
		}
	}
	if out.StageDir == "" {
		return fmt.Errorf("stage artifact: pipeline reported no staged release")
	}
	if err := copyTreeDeref(out.StageDir, filepath.Join(spec.ArtifactDir, "pb_public")); err != nil {
		return fmt.Errorf("stage artifact: copy web release: %w", err)
	}
	return nil
}

// copyTreeDeref copies src/ into dest, DEREFERENCING symlinks. The generator
// emits pb_hooks/pb_migrations as symlink farms pointing back into the
// workspace, and the workspace (the job dir) is deleted after commit — a
// plain cp -a would ship dangling links and the tenant's jsvm would fail to
// open every migration at boot. The artifact must be self-contained.
func copyTreeDeref(src, dest string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	_, err := pkgbuild.RunCmd(".", "cp", "-RLp", src+"/.", dest+"/")
	return err
}
